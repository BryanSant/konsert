package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Failure-driven retries of ticket runs (SYR-DNFEV). What a failed run gets depends on
// why it failed:
//   - oom, disk_full, timeout: the work needs more than that machine has: run again on a
//     larger machine class (more RAM; more free disk for disk_full), if one is connected.
//   - usage_limit, rate_limit: the account, not the work: run again; placement already
//     prefers accounts with quota left.
//   - auth, model, not_installed, missing_tool: only a person can fix it: Needs you.
//   - anything else (agent errors, exits): failed, as before.
//
// Retries are counted per ticket across these categories and capped by the retry_limit
// setting (default 2). A guardrails shed (memory_pressure_shed, host_memory_full) is
// not a failure of the work: handleTicketShed requeues it and it never spends a retry.

type retryAction int

const (
	retryFail   retryAction = iota // the ticket fails
	retryAgain                     // back to ready, any machine
	retryLarger                    // back to ready on a larger machine class
	retryAsk                       // Needs you: a person must fix something
)

const (
	retryLimitSetting  = "retry_limit"
	defaultRetryLimit  = 2
	maxRetryLimitValue = 10
)

// retryCategories are the failures that spend the retry budget.
var retryCategories = map[string]retryAction{
	protocol.ErrKindOOM:        retryLarger,
	protocol.ErrKindDiskFull:   retryLarger,
	protocol.ErrKindTimeout:    retryLarger,
	protocol.ErrKindUsageLimit: retryAgain,
	protocol.ErrKindRateLimit:  retryAgain,
}

// askCategories need a person: a login, a model name, a CLI or a tool to install.
var askCategories = map[string]bool{
	protocol.ErrKindAuth:        true,
	protocol.ErrKindModel:       true,
	protocol.ErrKindNotFound:    true,
	protocol.ErrKindMissingTool: true,
}

// retryDecision is what a ticket run that failed with category gets, after used of its
// limit retries.
func retryDecision(category string, used, limit int) retryAction {
	if askCategories[category] {
		return retryAsk
	}
	if a, ok := retryCategories[category]; ok && used < limit {
		return a
	}
	return retryFail
}

// retryLimit is the retry_limit setting (0 turns retries off).
func (h *Hub) retryLimit(ctx context.Context) int {
	saved, _ := h.store.GetAllSettings(ctx)
	if v, err := strconv.Atoi(strings.TrimSpace(saved[retryLimitSetting])); err == nil && v >= 0 {
		return min(v, maxRetryLimitValue)
	}
	return defaultRetryLimit
}

// retriesUsed counts the ticket's failed attempts that spent a retry.
func retriesUsed(attempts []store.Attempt) int {
	n := 0
	for _, a := range attempts {
		if a.Status == store.AttemptFailed {
			if _, ok := retryCategories[a.FailureCategory]; ok {
				n++
			}
		}
	}
	return n
}

// lastSizedFailure is the most recent failed attempt that asked for a larger machine.
func lastSizedFailure(attempts []store.Attempt) (store.Attempt, bool) {
	var last store.Attempt
	found := false
	for _, a := range attempts {
		if a.Status != store.AttemptFailed || retryCategories[a.FailureCategory] != retryLarger {
			continue
		}
		if !found || a.Attempt >= last.Attempt {
			last, found = a, true
		}
	}
	return last, found
}

func workerFacts(w store.Worker) protocol.MachineFacts {
	var f protocol.MachineFacts
	if w.FactsJSON != "" {
		_ = json.Unmarshal([]byte(w.FactsJSON), &f, protocol.JSONOptions)
	}
	return f
}

// largerClass raises sel so the ticket's next run needs more than the machine its last
// sized failure ran on: more RAM (and more free disk after disk_full).
func (h *Hub) largerClass(ctx context.Context, tk store.Ticket, sel *WorkerSelector) {
	attempts, err := h.store.ListAttempts(ctx, tk.ID, "")
	if err != nil {
		return
	}
	a, ok := lastSizedFailure(attempts)
	if !ok || a.WorkerID == "" {
		return
	}
	w, err := h.store.GetWorker(ctx, a.WorkerID)
	if err != nil {
		return
	}
	f := workerFacts(w)
	if f.RAMTotalMB > 0 && sel.MinRAMMB <= f.RAMTotalMB {
		sel.MinRAMMB = f.RAMTotalMB + 1
	}
	if a.FailureCategory == protocol.ErrKindDiskFull && f.DiskFreeGB > 0 && sel.MinDiskFreeGB <= f.DiskFreeGB {
		sel.MinDiskFreeGB = f.DiskFreeGB + 1
	}
}

// largerMachineExists reports whether some machine meets the raised selector.
func (h *Hub) largerMachineExists(ctx context.Context, sel WorkerSelector) bool {
	workers, err := h.store.ListWorkers(ctx)
	if err != nil {
		return false
	}
	for _, w := range workers {
		f := workerFacts(w)
		if (sel.MinRAMMB == 0 || f.RAMTotalMB >= sel.MinRAMMB) && (sel.MinDiskFreeGB == 0 || f.DiskFreeGB >= sel.MinDiskFreeGB) {
			return true
		}
	}
	return false
}

// retryOutcome decides where a failed ticket run goes: ready (a retry), needs_you, or
// failed, with the line the summary gets.
func (h *Hub) retryOutcome(ctx context.Context, tk store.Ticket, category string) (string, string) {
	attempts, _ := h.store.ListAttempts(ctx, tk.ID, "")
	used, limit := retriesUsed(attempts), h.retryLimit(ctx)
	// The attempt that just failed is already recorded: it is one of the used.
	switch retryDecision(category, max(used-1, 0), limit) {
	case retryAgain:
		return store.TicketStatusReady, fmt.Sprintf("Retrying (%d of %d) after %s.", used, limit, category)
	case retryLarger:
		sel := h.ticketWorkerSelector(ctx, tk)
		if !h.largerMachineExists(ctx, sel) {
			return store.TicketStatusNeedsYou, fmt.Sprintf("It failed with %s and no connected machine is larger than the one it ran on.", category)
		}
		return store.TicketStatusReady, fmt.Sprintf("Retrying on a larger machine (%d of %d) after %s.", used, limit, category)
	case retryAsk:
		return store.TicketStatusNeedsYou, fmt.Sprintf("It failed with %s: that needs you (a login, a model, or a tool to install).", category)
	}
	if _, ok := retryCategories[category]; ok && limit > 0 {
		return store.TicketStatusFailed, fmt.Sprintf("Failed with %s after %d retries.", category, limit)
	}
	return store.TicketStatusFailed, ""
}
