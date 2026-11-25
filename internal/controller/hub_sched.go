package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// DispatchToWorker sends task.start to workerID and tracks the assignment.
func (h *Hub) DispatchToWorker(workerID string, payload protocol.TaskStartPayload) error {
	h.mu.Lock()
	wc, ok := h.workers[workerID]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("worker %s is not connected", workerID)
	}
	h.taskWorker[payload.TaskID] = workerID
	h.mu.Unlock()

	wc.mu.Lock()
	wc.activeTasks[payload.TaskID] = true
	wc.mu.Unlock()

	if !h.sendWorkerJSON(wc, protocol.MsgTaskStart, payload) {
		wc.mu.Lock()
		delete(wc.activeTasks, payload.TaskID)
		wc.mu.Unlock()
		h.mu.Lock()
		if h.taskWorker[payload.TaskID] == workerID {
			delete(h.taskWorker, payload.TaskID)
		}
		h.mu.Unlock()
		return fmt.Errorf("failed to enqueue task.start to worker %s", workerID)
	}
	return nil
}

// kickScheduler requests a SchedulePending run on a tracked background goroutine.
// Concurrent kicks coalesce: at most one run is active and at most one more is
// queued behind it, so a burst of hellos or terminal statuses costs one or two passes.
func (h *Hub) kickScheduler() {
	h.schedDirty.Store(true)
	if !h.schedRunning.CompareAndSwap(false, true) {
		return
	}
	started := h.goBackground(func(ctx context.Context) {
		for {
			for h.schedDirty.Swap(false) {
				if ctx.Err() != nil {
					break
				}
				h.SchedulePending(ctx)
			}
			h.schedRunning.Store(false)
			// A kick may have landed between the last Swap and the Store above.
			if ctx.Err() != nil || !h.schedDirty.Load() || !h.schedRunning.CompareAndSwap(false, true) {
				return
			}
		}
	})
	if !started {
		h.schedRunning.Store(false)
	}
}

// taskStartSpec returns the persisted task.start payload and worker selector for t,
// and whether its worker was explicitly pinned. Tasks created before the spec was
// persisted fall back to a payload rebuilt from the task row.
func (h *Hub) taskStartSpec(ctx context.Context, t store.Task) (protocol.TaskStartPayload, WorkerSelector, bool) {
	var (
		payload     protocol.TaskStartPayload
		sel         WorkerSelector
		havePayload bool
		pinned      bool
	)
	spec, err := h.store.GetTaskStartSpec(ctx, t.ID)
	if err == nil && spec.PayloadJSON != "" {
		havePayload = json.Unmarshal([]byte(spec.PayloadJSON), &payload) == nil
	}
	if err == nil && spec.SelectorJSON != "" && json.Unmarshal([]byte(spec.SelectorJSON), &sel) == nil {
		pinned = spec.WorkerPinned
	} else {
		// Legacy row: the only constraint we know is the recorded worker.
		sel = WorkerSelector{WorkerID: t.WorkerID}
		pinned = t.WorkerID != ""
	}
	if !havePayload {
		payload = protocol.TaskStartPayload{
			Agent:          t.Agent,
			Model:          t.Model,
			SessionKind:    t.SessionKind,
			Workdir:        t.Workdir,
			Branch:         t.Branch,
			UseWorktree:    t.Branch != "",
			Prompt:         t.Prompt,
			Mode:           t.Mode,
			PermissionMode: string(t.PermissionMode),
			TimeoutSeconds: 3600,
		}
	}
	payload.TaskID = t.ID
	payload.SID = t.SID
	return payload, sel, pinned
}

// SchedulePending dispatches queued tasks to connected workers with capacity.
// Runs are serialized, and each task is claimed atomically in the store
// (queued -> assigned) before task.start is sent, so a task is never dispatched
// twice. A failed send returns the task to queued.
func (h *Hub) SchedulePending(ctx context.Context) {
	h.schedMu.Lock()
	defer h.schedMu.Unlock()

	// Pause everything stops all dispatching until the user resumes.
	if h.fleetPaused(ctx) {
		return
	}
	tasks, err := h.store.ListTasks(ctx, protocol.TaskStatusQueued, "", 200)
	if err != nil {
		return
	}
	all, err := h.store.ListWorkers(ctx)
	if err != nil {
		return
	}
	// Only connected workers can receive task.start; a disconnected-but-online
	// worker would otherwise win placement and leave the task waiting. Drained machines
	// accept nothing new.
	workers := make([]store.Worker, 0, len(all))
	for _, w := range all {
		if h.IsWorkerConnected(w.ID) && !w.Drained {
			workers = append(workers, w)
		}
	}
	if len(workers) == 0 {
		return
	}
	counts := h.ActiveTaskCounts()
	// Approved machine_slots overrides raise a machine's slot limit while they last.
	workers, slots := h.applySlotOverrides(ctx, workers)

	for _, t := range dispatchOrder(tasks, h.expeditedTasks(ctx)) {
		if ctx.Err() != nil {
			return
		}
		if t.Origin == protocol.TaskOriginExternal {
			continue
		}
		payload, sel, pinned := h.taskStartSpec(ctx, t)
		// Held machines (and machines holding a resource the task's prompt names) get no
		// automatic placement; the task waits until the user releases the hold.
		eligible := make([]store.Worker, 0, len(workers))
		lifted := map[string]*store.Override{}
		for _, w := range workers {
			if _, held, ov := h.holdVerdict(ctx, holdCheck{workerID: w.ID, taskID: t.ID, chatID: t.ParentChatID, text: t.Prompt}); !held {
				eligible = append(eligible, w)
				lifted[w.ID] = ov
			}
		}
		w, ok := SelectWorker(eligible, counts, sel, t.Agent, t.PermissionMode)
		if !ok {
			continue
		}
		if h.checkAccountLoggedIn(ctx, w.ID, payload.Agent, payload.AccountID) != nil {
			// Leave it queued: the account must sign in again first.
			continue
		}
		claimed, err := h.store.ClaimQueuedTask(ctx, t.ID, w.ID, causeScheduler)
		if err != nil || !claimed {
			continue
		}
		if payload.Shell == "" && (payload.Agent == "shell" || payload.SessionKind == "terminal") && w.DefaultShell != "" {
			payload.Shell = w.DefaultShell
		}
		if payload.Agent != "shell" && payload.SessionKind != "terminal" {
			payload.UseGitBash = w.AgentsUseGitBash
		}
		if err := h.DispatchToWorker(w.ID, payload); err != nil {
			h.logger.Warn("dispatch of queued task failed; returning it to the queue",
				"task_id", t.ID, "worker_id", w.ID, "err", err)
			_, _ = h.store.RequeueTask(ctx, t.ID, "", pinned, causeScheduler)
			continue
		}
		if ov := lifted[w.ID]; ov != nil {
			h.useOverride(ctx, *ov)
		}
		h.placed(ctx, slots, w.ID, counts[w.ID])
		counts[w.ID]++
		if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
			h.recordTaskTransition(ctx, updated, protocol.TaskStatusQueued, causeScheduler, "placed on "+w.Name)
			h.broadcastFleetUpdate("task", updated)
		}
	}

	// Ready queue: after queued tasks, claim and dispatch ready tickets for projects with autopilot enabled.
	h.scheduleDispatchableTickets(ctx, workers, counts, slots)
}

// dispatchOrder is the order queued tasks are placed in: expedited tasks first, then the
// rest, each oldest first. tasks is newest first (ListTasks).
func dispatchOrder(tasks []store.Task, expedited map[string]bool) []store.Task {
	out := make([]store.Task, 0, len(tasks))
	for _, first := range []bool{true, false} {
		for i := len(tasks) - 1; i >= 0; i-- {
			if expedited[tasks[i].ID] == first {
				out = append(out, tasks[i])
			}
		}
	}
	return out
}

// SendSteer forwards task.steer to the worker running taskID.
func (h *Hub) SendSteer(taskID, inputText string, appendNewline bool) error {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc == nil {
		return fmt.Errorf("no connected worker for task %s", taskID)
	}
	if !h.sendWorkerJSON(wc, protocol.MsgTaskSteer, protocol.TaskSteerPayload{
		TaskID:        taskID,
		InputText:     inputText,
		AppendNewline: appendNewline,
	}) {
		return fmt.Errorf("worker send queue full")
	}
	return nil
}

// SendCancel forwards task.cancel to the worker running taskID.
func (h *Hub) SendCancel(taskID string) bool {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc == nil {
		return false
	}
	return h.sendWorkerJSON(wc, protocol.MsgTaskCancel, protocol.TaskCancelPayload{TaskID: taskID})
}

// WaitForTask returns a channel that receives the task when it reaches a terminal status.
func (h *Hub) WaitForTask(taskID string) (<-chan store.Task, func()) {
	ch := make(chan store.Task, 1)
	h.mu.Lock()
	h.waiters[taskID] = append(h.waiters[taskID], ch)
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.waiters[taskID]
		for i, item := range list {
			if item == ch {
				h.waiters[taskID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(h.waiters[taskID]) == 0 {
			delete(h.waiters, taskID)
		}
	}
	return ch, cancel
}

// notifyTaskDone wakes waiters for a task that reached a terminal status and
// drops the hub's task->worker routing entry for it.
func (h *Hub) notifyTaskDone(t store.Task) {
	h.mu.Lock()
	list := h.waiters[t.ID]
	delete(h.waiters, t.ID)
	delete(h.taskWorker, t.ID)
	h.mu.Unlock()

	for _, ch := range list {
		select {
		case ch <- t:
		default:
		}
	}
}

// forgetTask drops the task->worker routing entry (task finished, requeued or orphaned).
func (h *Hub) forgetTask(taskID string) {
	h.mu.Lock()
	delete(h.taskWorker, taskID)
	h.mu.Unlock()
}

func (h *Hub) workerForTaskLocked(taskID string) *workerConn {
	wid, ok := h.taskWorker[taskID]
	if !ok {
		return nil
	}
	return h.workers[wid]
}

func (h *Hub) sendToTaskWorkerJSON(taskID, msgType string, payload any) {
	h.mu.RLock()
	wc := h.workerForTaskLocked(taskID)
	h.mu.RUnlock()
	if wc != nil {
		h.sendWorkerJSON(wc, msgType, payload)
	}
}
