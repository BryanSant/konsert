package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

const (
	worktreeRetentionDaysSetting = "worktree_retention_days"
	defaultWorktreeRetentionDays = 7
	maxWorktreeRetentionDays     = 365
)

// worktreeRetention returns how long failed/canceled ticket worktrees are retained
// before automatic cleanup (Hub override -> controller setting -> 7 days default).
func (h *Hub) worktreeRetention(ctx context.Context) time.Duration {
	if h.WorktreeRetention > 0 {
		return h.WorktreeRetention
	}
	if h.store != nil {
		if saved, err := h.store.GetAllSettings(ctx); err == nil {
			if raw := strings.TrimSpace(saved[worktreeRetentionDaysSetting]); raw != "" {
				if days, err := strconv.Atoi(raw); err == nil && days >= 0 && days <= maxWorktreeRetentionDays {
					return time.Duration(days) * 24 * time.Hour
				}
			}
		}
	}
	return defaultWorktreeRetentionDays * 24 * time.Hour
}

// CleanupTicketWorktrees asks the owning daemon(s) to remove all uncleaned worktrees
// provisioned for tasks of ticketID (git worktree remove + prune; branches are kept).
func (h *Hub) CleanupTicketWorktrees(ctx context.Context, ticketID string) error {
	if ticketID == "" || h.store == nil {
		return nil
	}
	tasks, err := h.store.ListTasksByTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, t := range tasks {
		if len(t.Worktrees) == 0 || t.WorktreesCleanedAt != nil {
			continue
		}
		h.cleanupTaskWorktrees(ctx, t, now)
	}
	return nil
}

// cleanupTaskWorktrees sends git.worktree.cleanup for each worktree recorded on t.
// Returns true if every worktree was removed and the task was marked cleaned.
func (h *Hub) cleanupTaskWorktrees(ctx context.Context, t store.Task, now time.Time) bool {
	if len(t.Worktrees) == 0 || t.WorkerID == "" || !h.IsWorkerConnected(t.WorkerID) {
		return false
	}
	allOK := true
	for _, wt := range t.Worktrees {
		if strings.TrimSpace(wt.WorktreePath) == "" {
			continue
		}
		rpcCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		env, err := h.RequestWorker(rpcCtx, t.WorkerID, protocol.MsgGitWorktreeCleanup, protocol.GitWorktreeCleanupRequest{
			Workdir:          wt.WorktreePath,
			RepoRoot:         wt.CheckoutPath,
			PruneEmptyParent: true,
		})
		cancel()
		if err != nil {
			h.logger.Warn("ticket worktree cleanup RPC failed",
				"task_id", t.ID, "ticket_id", t.TicketID, "worker_id", t.WorkerID, "worktree", wt.WorktreePath, "err", err)
			allOK = false
			continue
		}
		res, decErr := protocol.DecodePayload[protocol.GitWorktreeCleanupResult](env)
		if decErr != nil || !res.OK {
			errMsg := ""
			if decErr != nil {
				errMsg = decErr.Error()
			} else {
				errMsg = res.Error
			}
			h.logger.Warn("ticket worktree cleanup returned error",
				"task_id", t.ID, "ticket_id", t.TicketID, "worker_id", t.WorkerID, "worktree", wt.WorktreePath, "err", errMsg)
			allOK = false
		}
	}
	if allOK {
		_ = h.store.MarkTaskWorktreesCleaned(ctx, t.ID, now)
	}
	return allOK
}

// sweepTicketWorktrees removes worktrees for:
//   - tickets in done status (or deleted tickets) immediately (retrying if the worker was offline), and
//   - tickets in failed/canceled status once their retention period has elapsed.
func (h *Hub) sweepTicketWorktrees(ctx context.Context, now time.Time) {
	if h.store == nil {
		return
	}
	tasks, err := h.store.ListTasksPendingWorktreeCleanup(ctx, 100)
	if err != nil || len(tasks) == 0 {
		return
	}
	retention := h.worktreeRetention(ctx)
	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		tk, err := h.store.GetTicket(ctx, t.TicketID)
		if errors.Is(err, store.ErrNotFound) {
			h.cleanupTaskWorktrees(ctx, t, now)
			continue
		}
		if err != nil {
			continue
		}
		switch tk.Status {
		case store.TicketStatusDone:
			h.cleanupTaskWorktrees(ctx, t, now)
		case store.TicketStatusFailed, store.TicketStatusCanceled:
			ref := tk.UpdatedAt
			if ref.IsZero() && t.CompletedAt != nil {
				ref = *t.CompletedAt
			}
			if !ref.IsZero() && now.Sub(ref) >= retention {
				h.cleanupTaskWorktrees(ctx, t, now)
			}
		}
	}
}
