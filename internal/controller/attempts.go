package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// attemptTableCap is how many attempt rows inspect(kind=digest) prints.
const attemptTableCap = 20

// beginAttempt inserts the open row for a turn that is about to be sent.
// It returns the attempt number stored (0 when the row could not be written).
func (h *Hub) beginAttempt(ctx context.Context, a store.Attempt) int {
	if strings.TrimSpace(a.TaskID) == "" {
		return 0
	}
	now := time.Now().UTC()
	if a.QueuedAt == nil {
		a.QueuedAt = &now
	}
	if a.StartedAt == nil {
		a.StartedAt = &now
	}
	row, err := h.store.UpsertAttempt(ctx, a)
	if err != nil {
		h.logger.Warn("begin attempt failed", "task_id", a.TaskID, "ticket_id", a.TicketID, "err", err)
		if a.Attempt > 0 {
			return a.Attempt
		}
		return 0
	}
	return row.Attempt
}

// abandonAttempt closes an open row when the turn could not be sent.
func (h *Hub) abandonAttempt(ctx context.Context, ticketID, taskID string, n int) {
	if strings.TrimSpace(taskID) == "" || n < 1 {
		return
	}
	code := 1
	now := time.Now().UTC()
	if _, err := h.store.UpsertAttempt(ctx, store.Attempt{
		TicketID:        ticketID,
		TaskID:          taskID,
		Attempt:         n,
		Status:          store.AttemptFailed,
		FailureCategory: protocol.ErrKindAgentError,
		ExitCode:        &code,
		EndedAt:         &now,
	}); err != nil {
		h.logger.Warn("abandon attempt failed", "task_id", taskID, "attempt", n, "err", err)
	}
}

// finishTicketAttempt writes the outcome of one ticket-chat attempt.
// attemptN 0 uses the digest's attempt, or the ticket's current attempt.
func (h *Hub) finishTicketAttempt(ctx context.Context, tk store.Ticket, cs store.ChatSession, task *store.Task, dig *protocol.RunDigest, status, category string, exitCode *int, attemptN int) {
	if task == nil || task.ID == "" {
		return
	}
	if cs.WorkerID != "" && (status == store.AttemptCompleted || status == store.AttemptReview) {
		h.shedCooldown.clear(cs.WorkerID) // the machine ran work to the end: its shed streak is over
	}
	n := attemptN
	if n < 1 {
		// The ticket counter is the attempt identity. A follow-up turn in the same
		// chat bumps the worker's turn count without starting a new attempt row;
		// only self-repair and requeue advance tickets.attempt.
		n = max(tk.Attempt, 1)
	}
	if category == "" && (status == store.AttemptFailed || status == store.AttemptCanceled) && dig != nil && dig.Failure != nil {
		category = dig.Failure.Category
	}
	if status == store.AttemptFailed && category == "" {
		category = protocol.ErrKindAgentError
	}
	if status == store.AttemptCanceled && category == "" {
		category = protocol.ErrKindCanceled
	}
	var since *time.Time
	if rows, err := h.store.ListAttempts(ctx, tk.ID, ""); err == nil {
		for _, r := range rows {
			if r.Attempt == n && r.StartedAt != nil {
				since = r.StartedAt
				break
			}
		}
	}
	if since == nil && task.StartedAt != nil {
		since = task.StartedAt
	}
	inTok, outTok, cost := h.attemptUsage(ctx, cs.ID, since)
	a := store.Attempt{
		TicketID:        tk.ID,
		TaskID:          task.ID,
		ChatID:          cs.ID,
		Attempt:         n,
		WorkerID:        cs.WorkerID,
		AccountID:       cs.AccountID,
		Agent:           cs.Agent,
		Model:           cs.Model,
		Status:          status,
		FailureCategory: category,
		ExitCode:        exitCode,
		TokensIn:        inTok,
		TokensOut:       outTok,
		CostUSD:         cost,
	}
	if !task.CreatedAt.IsZero() {
		queued := task.CreatedAt
		a.QueuedAt = &queued
	}
	if task.StartedAt != nil {
		a.StartedAt = task.StartedAt
	}
	if dig != nil {
		if dig.Agent != "" {
			a.Agent = dig.Agent
		}
		if dig.Model != "" {
			a.Model = dig.Model
		}
		if dig.DurationS > 0 {
			d := dig.DurationS
			a.DurationS = &d
		}
		a.GitJSON = attemptGitJSON(dig.Git)
		if dig.Verify != nil {
			v := *dig.Verify
			a.VerifyCmd = v.Cmd
			exit, passed, failed := v.Exit, v.Passed, v.Failed
			a.VerifyExit = &exit
			a.VerifyPassed = &passed
			a.VerifyFailed = &failed
		}
	}
	if attemptTerminal(status) {
		now := time.Now().UTC()
		a.EndedAt = &now
	}
	if _, err := h.store.UpsertAttempt(ctx, a); err != nil {
		h.logger.Warn("record attempt failed", "ticket_id", tk.ID, "task_id", task.ID, "attempt", n, "err", err)
		return
	}
	if attemptTerminal(status) {
		h.attemptEnded(ctx, tk.ID, task.ID, n)
	}
}

// recordAdHocAttempt writes the outcome of a terminal task that is not a Syre ticket.
// Ticket chats record their own rows from the chat event, so a later task.status
// cannot overwrite review with completed.
func (h *Hub) recordAdHocAttempt(ctx context.Context, task store.Task, workerID string, dig protocol.RunDigest, taskStatus string, exitCode *int) {
	if task.ID == "" || h.taskIsSyreTicket(ctx, task.TicketID) {
		return
	}
	n := h.adHocAttemptNumber(ctx, task.ID, dig.Attempt)
	status := dig.Status
	switch status {
	case store.AttemptReview, store.AttemptNeedsYou, store.AttemptFailed, store.AttemptCompleted, store.AttemptCanceled:
	default:
		switch taskStatus {
		case protocol.TaskStatusCanceled:
			status = store.AttemptCanceled
		case protocol.TaskStatusFailed:
			status = store.AttemptFailed
		default:
			status = store.AttemptCompleted
		}
	}
	cat := ""
	if dig.Failure != nil {
		cat = dig.Failure.Category
	}
	if dig.Failure != nil && dig.Failure.Category == protocol.ErrKindCanceled {
		status = store.AttemptCanceled
	}
	if status == store.AttemptFailed && cat == "" {
		cat = protocol.ErrKindAgentError
	}
	if status == store.AttemptCanceled && cat == "" {
		cat = protocol.ErrKindCanceled
	}
	now := time.Now().UTC()
	a := store.Attempt{
		TaskID:          task.ID,
		Attempt:         n,
		WorkerID:        workerID,
		Agent:           dig.Agent,
		Model:           dig.Model,
		Status:          status,
		FailureCategory: cat,
		ExitCode:        exitCode,
		EndedAt:         &now,
		GitJSON:         attemptGitJSON(dig.Git),
	}
	if a.WorkerID == "" {
		a.WorkerID = task.WorkerID
	}
	if a.Agent == "" {
		a.Agent = task.Agent
	}
	if a.Model == "" {
		a.Model = task.Model
	}
	if !task.CreatedAt.IsZero() {
		queued := task.CreatedAt
		a.QueuedAt = &queued
	}
	if task.StartedAt != nil {
		a.StartedAt = task.StartedAt
	}
	if dig.DurationS > 0 {
		d := dig.DurationS
		a.DurationS = &d
	}
	if dig.Verify != nil {
		v := *dig.Verify
		a.VerifyCmd = v.Cmd
		exit, passed, failed := v.Exit, v.Passed, v.Failed
		a.VerifyExit = &exit
		a.VerifyPassed = &passed
		a.VerifyFailed = &failed
	}
	if cs, err := h.store.GetChatSessionByTaskID(ctx, task.ID); err == nil {
		a.ChatID = cs.ID
		a.AccountID = cs.AccountID
		since := a.StartedAt
		if rows, lerr := h.store.ListAttempts(ctx, "", task.ID); lerr == nil {
			for _, r := range rows {
				if r.Attempt == n && r.StartedAt != nil {
					since = r.StartedAt
				}
			}
		}
		a.TokensIn, a.TokensOut, a.CostUSD = h.attemptUsage(ctx, cs.ID, since)
	}
	if _, err := h.store.UpsertAttempt(ctx, a); err != nil {
		h.logger.Warn("record attempt failed", "task_id", task.ID, "attempt", n, "err", err)
		return
	}
	h.attemptEnded(ctx, "", task.ID, n)
}

// markAttemptLost records the in-flight attempt as failed/lost.
// n < 1 selects the open row, or the next number when the latest row already finished.
// An empty task id is skipped so a lease with no task cannot fail the expire loop.
func (h *Hub) markAttemptLost(ctx context.Context, ticketID, taskID, workerID string, n int) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return
	}
	if n < 1 {
		n = h.lostAttemptNumber(ctx, ticketID, taskID)
	}
	code := -1
	now := time.Now().UTC()
	if _, err := h.store.UpsertAttempt(ctx, store.Attempt{
		TicketID:        strings.TrimSpace(ticketID),
		TaskID:          taskID,
		Attempt:         n,
		WorkerID:        workerID,
		Status:          store.AttemptFailed,
		FailureCategory: protocol.ErrKindLost,
		ExitCode:        &code,
		EndedAt:         &now,
	}); err != nil {
		h.logger.Warn("record lost attempt failed", "ticket_id", ticketID, "task_id", taskID, "attempt", n, "err", err)
		return
	}
	h.attemptEnded(ctx, strings.TrimSpace(ticketID), taskID, n)
}

// markTaskLostAttempt records an ad-hoc task's open attempt as lost.
// Syre ticket tasks are owned by the lease expiry path.
func (h *Hub) markTaskLostAttempt(ctx context.Context, t store.Task) {
	if t.ID == "" || h.taskIsSyreTicket(ctx, t.TicketID) {
		return
	}
	h.markAttemptLost(ctx, "", t.ID, t.WorkerID, 0)
}

func (h *Hub) taskIsSyreTicket(ctx context.Context, ticketID string) bool {
	if strings.TrimSpace(ticketID) == "" {
		return false
	}
	_, err := h.store.GetTicket(ctx, ticketID)
	return err == nil
}

func (h *Hub) adHocAttemptNumber(ctx context.Context, taskID string, digestAttempt int) int {
	rows, err := h.store.ListAttempts(ctx, "", taskID)
	if err != nil || len(rows) == 0 {
		if digestAttempt > 0 {
			return digestAttempt
		}
		return 1
	}
	maxN := 0
	openN := 0
	var latest store.Attempt
	for _, r := range rows {
		if r.Attempt >= maxN {
			maxN = r.Attempt
			latest = r
		}
		if r.Status == "" || r.Status == store.AttemptNeedsYou {
			openN = r.Attempt
		}
	}
	if openN > 0 {
		return openN
	}
	if digestAttempt > maxN {
		return digestAttempt
	}
	if latest.FailureCategory == protocol.ErrKindLost {
		if digestAttempt > 0 {
			return digestAttempt
		}
		return maxN + 1
	}
	if digestAttempt > 0 {
		return digestAttempt
	}
	return maxN
}

func (h *Hub) lostAttemptNumber(ctx context.Context, ticketID, taskID string) int {
	var (
		rows []store.Attempt
		err  error
	)
	if strings.TrimSpace(ticketID) != "" {
		rows, err = h.store.ListAttempts(ctx, ticketID, "")
	} else {
		rows, err = h.store.ListAttempts(ctx, "", taskID)
	}
	if err != nil || len(rows) == 0 {
		return 1
	}
	maxN := 0
	openN := 0
	for _, r := range rows {
		if r.Attempt > maxN {
			maxN = r.Attempt
		}
		if r.Status == "" || r.Status == store.AttemptNeedsYou {
			openN = r.Attempt
		}
	}
	if openN > 0 {
		return openN
	}
	return maxN + 1
}

// attemptUsage sums chat usage events at or after since (nil sums the whole chat).
// A zero cost is unknown and stays nil; a reported cost is kept.
func (h *Hub) attemptUsage(ctx context.Context, chatID string, since *time.Time) (int64, int64, *float64) {
	if chatID == "" {
		return 0, 0, nil
	}
	evs, err := h.store.ListChatEvents(ctx, chatID, 0, 5000)
	if err != nil {
		return 0, 0, nil
	}
	var inTok, outTok int64
	var costSum float64
	sawCost := false
	for _, e := range evs {
		if e.Kind != protocol.EventUsage {
			continue
		}
		if since != nil && !since.IsZero() && e.CreatedAt.Before(*since) {
			continue
		}
		var ev protocol.AgentChatEvent
		if json.Unmarshal([]byte(e.JSON), &ev, protocol.JSONOptions) != nil || ev.Usage == nil {
			continue
		}
		inTok += ev.Usage.InputTokens
		outTok += ev.Usage.OutputTokens
		if ev.Usage.CostUSD != 0 {
			sawCost = true
			costSum += ev.Usage.CostUSD
		}
	}
	if !sawCost {
		return inTok, outTok, nil
	}
	return inTok, outTok, &costSum
}

func attemptGitJSON(git map[string]string) string {
	if len(git) == 0 {
		return ""
	}
	b, err := json.Marshal(git, protocol.JSONOptions)
	if err != nil {
		return ""
	}
	s := string(b)
	if s == "" || s == "{}" || s == "null" {
		return ""
	}
	return s
}

func attemptTerminal(status string) bool {
	switch status {
	case store.AttemptReview, store.AttemptFailed, store.AttemptCompleted, store.AttemptCanceled:
		return true
	default:
		return false
	}
}

// attemptTable renders a capped attempts table for inspect(kind=digest).
// Ticket rows are preferred so a requeue still shows the earlier attempts.
func (s *FleetService) attemptTable(ctx context.Context, ticketID, taskID string) string {
	var (
		rows []store.Attempt
		err  error
	)
	if ticketID != "" {
		rows, err = s.store.ListAttempts(ctx, ticketID, "")
	} else if taskID != "" {
		rows, err = s.store.ListAttempts(ctx, "", taskID)
	}
	if err != nil || len(rows) == 0 {
		return ""
	}
	total := len(rows)
	if len(rows) > attemptTableCap {
		rows = rows[len(rows)-attemptTableCap:]
	}
	var b strings.Builder
	b.WriteString("attempts:")
	if total > attemptTableCap {
		fmt.Fprintf(&b, " (showing %d of %d)", attemptTableCap, total)
	}
	b.WriteString("\nattempt  machine  agent/model  duration  status  verify")
	for _, a := range rows {
		fmt.Fprintf(&b, "\n%d  %s  %s  %s  %s  %s",
			a.Attempt, attemptMachine(ctx, s, a), attemptAgentModel(a), attemptDuration(a), attemptStatusLabel(a), attemptVerifyLabel(a))
	}
	return b.String()
}

func attemptMachine(ctx context.Context, s *FleetService, a store.Attempt) string {
	if a.WorkerID != "" {
		if w, err := s.store.GetWorker(ctx, a.WorkerID); err == nil && w.Name != "" {
			return w.Name
		}
		return a.WorkerID
	}
	return "-"
}

func attemptAgentModel(a store.Attempt) string {
	switch {
	case a.Agent != "" && a.Model != "":
		return a.Agent + "/" + a.Model
	case a.Agent != "":
		return a.Agent
	case a.Model != "":
		return a.Model
	default:
		return "-"
	}
}

func attemptDuration(a store.Attempt) string {
	if a.DurationS == nil {
		return "-"
	}
	return strconv.FormatInt(*a.DurationS, 10) + "s"
}

func attemptStatusLabel(a store.Attempt) string {
	if a.Status == "" {
		return "-"
	}
	if a.FailureCategory == protocol.ErrKindMemoryPressureShed || a.FailureCategory == protocol.FailureHostMemoryFull {
		return "requeued/" + a.FailureCategory
	}
	if a.FailureCategory != "" {
		return a.Status + "/" + a.FailureCategory
	}
	return a.Status
}

func attemptVerifyLabel(a store.Attempt) string {
	if a.VerifyExit == nil && a.VerifyCmd == "" {
		return "-"
	}
	if a.VerifyExit != nil && *a.VerifyExit == 0 {
		return "pass"
	}
	if a.VerifyFailed != nil && *a.VerifyFailed > 0 {
		return fmt.Sprintf("fail %d", *a.VerifyFailed)
	}
	return "fail"
}
