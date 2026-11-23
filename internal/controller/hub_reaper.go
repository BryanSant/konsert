package controller

import (
	"context"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// RunReaper periodically marks workers with missed heartbeats offline and applies orphan policies.
// It returns when ctx is canceled or the hub shuts down.
func (h *Hub) RunReaper(ctx context.Context) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(h.ctx, stop)()

	interval := h.ReapInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.reapStaleWorkers(ctx)
		}
	}
}

func (h *Hub) reapStaleWorkers(ctx context.Context) {
	timeout := h.StaleWorkerTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cutoff := time.Now().UTC().Add(-timeout)
	staleIDs, err := h.store.MarkStaleWorkersOffline(ctx, cutoff)
	if err != nil || len(staleIDs) == 0 {
		return
	}
	for _, wid := range staleIDs {
		h.logger.Warn("marking stale worker offline", "worker_id", wid)
		h.mu.Lock()
		if wc, ok := h.workers[wid]; ok {
			wc.cancel()
			delete(h.workers, wid)
		}
		h.mu.Unlock()

		if w, err := h.store.GetWorker(ctx, wid); err == nil {
			h.broadcastFleetUpdate("worker", w)
		}
		h.handleOrphanTasks(ctx, wid)
	}
}

func (h *Hub) handleOrphanTasks(ctx context.Context, workerID string) {
	tasks, err := h.store.ListTasks(ctx, "", workerID, 200)
	if err != nil {
		return
	}
	requeued := false
	defer func() {
		if requeued {
			h.kickScheduler()
		}
	}()
	for _, t := range tasks {
		if protocol.IsTerminalTaskStatus(t.Status) || t.Status == protocol.TaskStatusQueued {
			continue
		}
		h.forgetTask(t.ID)
		// Passively discovered external sessions should never turn red ("failed") on daemon disconnect.
		if t.Origin == protocol.TaskOriginExternal {
			_ = h.store.UpdateTaskStatus(ctx, t.ID, protocol.TaskStatusCompleted, "external session detached", "", nil)
			if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
			continue
		}
		if t.OrphanPolicy == protocol.OrphanPolicyRequeue {
			// Keep the dead worker only when the task was explicitly pinned to it;
			// otherwise the scheduler may place it on any matching worker.
			_, _, pinned := h.taskStartSpec(ctx, t)
			if ok, _ := h.store.RequeueTask(ctx, t.ID, "requeued after worker disconnect", pinned); ok {
				requeued = true
			}
			if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
			continue
		}
		code := -1
		_ = h.store.UpdateTaskStatus(ctx, t.ID, protocol.TaskStatusFailed, "worker went offline while task was active", "", &code)
		if updated, err := h.store.GetTask(ctx, t.ID); err == nil {
			h.notifyTaskDone(updated)
			h.broadcastFleetUpdate("task", updated)
		}
	}
}

// scheduleLostTaskCheck re-examines taskIDs after LostTaskGrace and reconciles
// any that the worker still does not report as active.
func (h *Hub) scheduleLostTaskCheck(wc *workerConn, taskIDs []string) {
	if len(taskIDs) == 0 {
		return
	}
	grace := h.LostTaskGrace
	if grace <= 0 {
		grace = 45 * time.Second
	}
	ids := append([]string(nil), taskIDs...)
	h.goBackground(func(ctx context.Context) {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		h.reconcileLostTasks(ctx, wc, ids)
	})
}

// reconcileLostTasks marks tasks lost that a still-connected worker no longer
// reports and that have not reached a terminal status. Tasks with the requeue
// orphan policy go back to the queue; others fail with exit code -1. If the
// worker disconnected meanwhile, the reaper's orphan handling owns the tasks.
func (h *Hub) reconcileLostTasks(ctx context.Context, wc *workerConn, taskIDs []string) {
	h.mu.RLock()
	current := h.workers[wc.id] == wc
	h.mu.RUnlock()
	if !current {
		return
	}
	kick := false
	for _, id := range taskIDs {
		wc.mu.Lock()
		active := wc.activeTasks[id]
		wc.mu.Unlock()
		if active {
			continue
		}
		t, err := h.store.GetTask(ctx, id)
		if err != nil || t.WorkerID != wc.id || t.Origin == protocol.TaskOriginExternal ||
			t.Status == protocol.TaskStatusQueued || protocol.IsTerminalTaskStatus(t.Status) {
			continue
		}
		h.logger.Warn("worker no longer reports task; reconciling as lost", "task_id", id, "worker_id", wc.id, "status", t.Status)
		h.forgetTask(id)
		if t.OrphanPolicy == protocol.OrphanPolicyRequeue {
			_, _, pinned := h.taskStartSpec(ctx, t)
			if ok, _ := h.store.RequeueTask(ctx, id, "requeued: worker no longer reports the task", pinned); ok {
				kick = true
			}
			if updated, err := h.store.GetTask(ctx, id); err == nil {
				h.broadcastFleetUpdate("task", updated)
			}
			continue
		}
		code := -1
		_ = h.store.UpdateTaskStatus(ctx, id, protocol.TaskStatusFailed, "task lost: worker no longer reports it", "", &code)
		if updated, err := h.store.GetTask(ctx, id); err == nil {
			h.notifyTaskDone(updated)
			h.broadcastFleetUpdate("task", updated)
		}
		kick = true // capacity freed
	}
	if kick {
		h.kickScheduler()
	}
}
