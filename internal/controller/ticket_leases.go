package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

const (
	ticketLeaseTTLSetting        = "ticket_lease_ttl_seconds"
	defaultTicketLeaseTTLSeconds = 600 // 10 minutes
	minTicketLeaseTTLSeconds     = 10
	maxTicketLeaseTTLSeconds     = 86400

	verifyTimeoutSetting        = "verify_timeout_seconds"
	defaultVerifyTimeoutSeconds = 1200 // 20 minutes
	minVerifyTimeoutSeconds     = 10
	maxVerifyTimeoutSeconds     = 86400

	defaultTicketRetryLimit = 2
	verifyRepairMaxTokens   = 200
)

// verifyTimeout returns the configured verification command timeout (Hub override ->
// controller setting -> 20 minutes default).
func (h *Hub) verifyTimeout(ctx context.Context) time.Duration {
	if h.VerifyTimeout > 0 {
		return h.VerifyTimeout
	}
	if saved, err := h.store.GetAllSettings(ctx); err == nil {
		if raw := strings.TrimSpace(saved[verifyTimeoutSetting]); raw != "" {
			if sec, err := strconv.Atoi(raw); err == nil && sec >= minVerifyTimeoutSeconds && sec <= maxVerifyTimeoutSeconds {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return time.Duration(defaultVerifyTimeoutSeconds) * time.Second
}

// ticketAutonomy resolves effective retry_limit and escalate_on_blocker for tk
// (ticket override > project default > built-in default: retry_limit=2, escalate_on_blocker=false).
func (h *Hub) ticketAutonomy(ctx context.Context, tk store.Ticket) (retryLimit int, escalateOnBlocker bool) {
	retryLimit = defaultTicketRetryLimit
	var proj store.Project
	if tk.ProjectID != "" {
		if p, err := h.store.GetProject(ctx, tk.ProjectID); err == nil {
			proj = p
		}
	}
	if tk.Autonomy.RetryLimit != nil {
		retryLimit = *tk.Autonomy.RetryLimit
	} else if proj.Autonomy.RetryLimit != nil {
		retryLimit = *proj.Autonomy.RetryLimit
	}
	if tk.Autonomy.EscalateOnBlocker != nil {
		escalateOnBlocker = *tk.Autonomy.EscalateOnBlocker
	} else if proj.Autonomy.EscalateOnBlocker != nil {
		escalateOnBlocker = *proj.Autonomy.EscalateOnBlocker
	}
	return retryLimit, escalateOnBlocker
}

// ticketApprovalPolicy resolves effective approval_mode for ticketID
// (ticket override > project default > built-in default "autonomous").
func (h *Hub) ticketApprovalPolicy(ctx context.Context, ticketID string) string {
	if ticketID == "" {
		return store.AutonomyApprovalAutonomous
	}
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return store.AutonomyApprovalAutonomous
	}
	if m := strings.ToLower(strings.TrimSpace(tk.Autonomy.ApprovalMode)); m != "" {
		return m
	}
	if tk.ProjectID != "" {
		if p, err := h.store.GetProject(ctx, tk.ProjectID); err == nil {
			if m := strings.ToLower(strings.TrimSpace(p.Autonomy.ApprovalMode)); m != "" {
				return m
			}
		}
	}
	return store.AutonomyApprovalAutonomous
}

// guardedMaxRisk is the highest approval risk a ticket chat may use while the
// worker's workdir guard still sees every command: risk 2 ("accept edits") applies
// file edits but still asks before commands, so the guard can auto-approve
// in-workdir commands and escalate the rest. Risk 3+ modes ("auto", full access)
// skip the prompts the guard depends on.
const guardedMaxRisk = 2

// ticketApprovalMode selects the most autonomous approval mode for a ticket chat that
// keeps every command visible to the workdir guard, never exceeding the machine's
// sandbox posture. Codex is the exception: full-auto runs inside its OS sandbox
// (workspace-write, no network), which confines it to the workdir by itself.
func ticketApprovalMode(agent string, sandboxed bool, acct *protocol.AgentAccount) string {
	maxRisk := guardedMaxRisk
	if sandboxed && protocol.SandboxMaxRisk < maxRisk {
		maxRisk = protocol.SandboxMaxRisk
	}
	var mode string
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude":
		mode = "acceptEdits"
	case "codex":
		mode = "full-auto"
	}
	if acct == nil || len(acct.Approvals) == 0 {
		return mode
	}
	if mode != "" {
		for _, m := range acct.Approvals {
			if m.ID == mode {
				if m.Disabled != "" || (sandboxed && m.Risk > protocol.SandboxMaxRisk) {
					return highestSafeApprovalMode(acct.Approvals, maxRisk)
				}
				return mode
			}
		}
	}
	return highestSafeApprovalMode(acct.Approvals, maxRisk)
}

func highestSafeApprovalMode(modes []protocol.AgentApprovalMode, maxRisk int) string {
	bestRisk := -1
	best := ""
	for _, m := range modes {
		if m.Disabled != "" || m.Risk > maxRisk {
			continue
		}
		if m.Risk > bestRisk {
			bestRisk = m.Risk
			best = m.ID
		}
	}
	return best
}

func isVerifyFailureDigest(dig *protocol.RunDigest) bool {
	if dig == nil {
		return false
	}
	if dig.Verify != nil && dig.Verify.Exit != 0 {
		return true
	}
	return dig.Failure != nil && dig.Failure.Category == protocol.ErrKindVerify
}

// formatVerifyRepairPrompt builds a compact self-repair prompt capped at ~200 tokens:
//
//	Verification failed (attempt N/M). Fix the failures below, then end your turn.
//	<cmd>
//	<failing tests>
//	<tail>
func formatVerifyRepairPrompt(attempt, retryLimit int, dig *protocol.RunDigest) string {
	header := fmt.Sprintf("Verification failed (attempt %d/%d). Fix the failures below, then end your turn.", attempt, retryLimit)
	var cmdLine string
	if dig != nil && dig.Verify != nil && strings.TrimSpace(dig.Verify.Cmd) != "" {
		cmdLine = strings.TrimSpace(dig.Verify.Cmd)
	}
	var tests []string
	var tail []string
	if dig != nil && dig.Failure != nil {
		tests = append(tests, dig.Failure.Tests...)
		tail = append(tail, dig.Failure.Tail...)
	}

	render := func(tList, tailLines []string) string {
		var b strings.Builder
		b.WriteString(header)
		if cmdLine != "" {
			b.WriteByte('\n')
			b.WriteString(cmdLine)
		}
		if len(tList) > 0 {
			b.WriteByte('\n')
			b.WriteString("Failing tests: ")
			b.WriteString(strings.Join(tList, ", "))
		}
		if len(tailLines) > 0 {
			b.WriteByte('\n')
			b.WriteString(strings.Join(tailLines, "\n"))
		}
		return b.String()
	}

	out := render(tests, tail)
	if protocol.EstimateTokens(out) <= verifyRepairMaxTokens {
		return out
	}
	// 1. Trim tail lines from the front (oldest first, preserving most recent output).
	for len(tail) > 0 && protocol.EstimateTokens(out) > verifyRepairMaxTokens {
		tail = tail[1:]
		out = render(tests, tail)
	}
	// 2. Trim failing test names from the end.
	for len(tests) > 0 && protocol.EstimateTokens(out) > verifyRepairMaxTokens {
		tests = tests[:len(tests)-1]
		out = render(tests, tail)
	}
	// 3. Hard cap if still over budget.
	if protocol.EstimateTokens(out) > verifyRepairMaxTokens {
		maxChars := verifyRepairMaxTokens * 4
		runes := []rune(out)
		if len(runes) > maxChars {
			out = string(runes[:maxChars])
		}
	}
	return out
}

// ticketLeaseTTL returns the configured ticket lease TTL (Hub override -> controller
// setting -> 10 minutes default).
func (h *Hub) ticketLeaseTTL(ctx context.Context) time.Duration {
	if h.TicketLeaseTTL > 0 {
		return h.TicketLeaseTTL
	}
	if saved, err := h.store.GetAllSettings(ctx); err == nil {
		if raw := strings.TrimSpace(saved[ticketLeaseTTLSetting]); raw != "" {
			if sec, err := strconv.Atoi(raw); err == nil && sec >= minTicketLeaseTTLSeconds && sec <= maxTicketLeaseTTLSeconds {
				return time.Duration(sec) * time.Second
			}
		}
	}
	return store.DefaultTicketLeaseTTL
}

// ticketWorkerSelector maps a ticket's placement hints onto a WorkerSelector.
func (h *Hub) ticketWorkerSelector(ctx context.Context, tk store.Ticket) WorkerSelector {
	sel := WorkerSelector{
		OS:   strings.TrimSpace(tk.Hints.OS),
		Arch: strings.TrimSpace(tk.Hints.Arch),
	}
	if m := strings.TrimSpace(tk.Hints.Machine); m != "" {
		if id, err := h.store.ResolveID(ctx, ids.Machine, m); err == nil {
			sel.WorkerID = id
		} else {
			sel.Name = m
		}
	}
	if tk.Hints.MinRAMGB > 0 {
		sel.MinRAMMB = uint64(tk.Hints.MinRAMGB) * 1024
	}
	sel.GPU = tk.Hints.GPU
	if len(tk.Hints.Tools) > 0 {
		sel.RequiredTools = make(map[string]string, len(tk.Hints.Tools))
		for _, tool := range tk.Hints.Tools {
			if t := strings.TrimSpace(tool); t != "" {
				sel.RequiredTools[t] = ""
			}
		}
	}
	return sel
}

// scheduleDispatchableTickets runs at the end of each scheduler pass, after queued tasks.
// Precedence: fleet paused > project paused/directive > global/project play.
func (h *Hub) scheduleDispatchableTickets(ctx context.Context) {
	if h.fleetPaused(ctx) {
		return
	}
	projects, err := h.store.ListProjects(ctx)
	if err != nil {
		return
	}
	globalOn := h.autopilotGlobal(ctx)
	autopilotProjects := make(map[string]store.Project, len(projects))
	for _, p := range projects {
		if isProjectAutopilotPlaying(p, globalOn, false, h.projectDirectivePaused(ctx, p.ID)) {
			autopilotProjects[p.ID] = p
		}
	}
	if len(autopilotProjects) == 0 {
		return
	}

	// Batch any ready tickets missing repo bindings or marked needs_decomposition into a pulse.
	h.scanTicketsAwaitingDecomposition(ctx, autopilotProjects)

	tickets, err := h.store.ListDispatchableTickets(ctx, "", 200)
	if err != nil || len(tickets) == 0 {
		return
	}

	// Snapshot connected workers after reading autopilot/ticket state so an in-flight
	// scheduler pass kicked before a newer worker registered cannot pair a stale worker
	// snapshot with freshly enabled autopilot or newly ready tickets.
	workers, counts, slots, ok := h.schedulableWorkers(ctx)
	if !ok {
		return
	}
	workers, ticketLifts := h.applyMachinePolicy(ctx, workers, counts, protocol.WorkTicket, "tickets")
	if len(workers) == 0 {
		return
	}

	saved, _ := h.store.GetAllSettings(ctx)
	defaultAgent := strings.TrimSpace(saved["default_agent"])
	defaultModel := strings.TrimSpace(saved["default_model"])
	permMode := store.PermissionMode(strings.TrimSpace(saved["default_permission_mode"]))
	if permMode == "" {
		permMode = store.PermissionFullAccess
	}
	leaseTTL := h.ticketLeaseTTL(ctx)

	// Account for tickets already leased to each worker (their runs are agent chats,
	// not PTY tasks in ActiveTaskCounts).
	for _, w := range workers {
		if leased, err := h.store.ListLeasedTicketsByWorker(ctx, w.ID); err == nil && len(leased) > 0 {
			counts[w.ID] += len(leased)
		}
	}

	for _, tk := range tickets {
		if ctx.Err() != nil || h.fleetPaused(ctx) {
			return
		}
		if _, ok := autopilotProjects[tk.ProjectID]; !ok {
			continue
		}
		bindings, err := h.store.ListTicketRepoBindings(ctx, tk.ID)
		if err != nil || len(bindings) == 0 {
			continue
		}
		type resolvedBinding struct {
			binding store.TicketRepoBinding
			repo    store.Repo
		}
		resolved := make([]resolvedBinding, 0, len(bindings))
		reposOK := true
		for _, b := range bindings {
			repo, rErr := h.store.GetRepo(ctx, b.RepoID)
			if rErr != nil {
				reposOK = false
				break
			}
			resolved = append(resolved, resolvedBinding{binding: b, repo: repo})
		}
		if !reposOK {
			continue
		}
		firstBinding := bindings[0]
		holdText := strings.TrimSpace(tk.Title + "\n" + tk.Body)
		sel := h.ticketWorkerSelector(ctx, tk)

		pinnedAgent := strings.TrimSpace(tk.Hints.Agent) != ""
		agent := strings.TrimSpace(tk.Hints.Agent)
		if agent == "" {
			agent = defaultAgent
		}
		model := strings.TrimSpace(tk.Hints.Model)
		if model == "" {
			model = defaultModel
		}

		// Filter workers through holds (machine holds and resource holds matching ticket text).
		eligible := make([]store.Worker, 0, len(workers))
		lifted := map[string]*store.Override{}
		for _, w := range workers {
			if h.shedCooldown.active(w.ID, time.Now()) {
				continue // recovering from a guardrails shed
			}
			if _, held, ov := h.holdVerdict(ctx, holdCheck{workerID: w.ID, text: holdText}); !held {
				eligible = append(eligible, w)
				lifted[w.ID] = ov
			}
		}

		// Locality preference: prefer machines with checkouts of all bound repos; machines
		// without a checkout are still eligible only if the missing repo has remote_url set.
		// The scorer weighs it against the headroom: a machine that can't fit the work's
		// expected peak is a last resort, checkout or not (SYR-WCH0N, SYR-YCQQ1).
		peak := h.expectedPeak(ctx, h.workKeyFor(ctx, store.Attempt{TicketID: tk.ID}, store.Task{ProjectID: tk.ProjectID, RepoID: firstBinding.RepoID}), "")
		localityFn := func(w store.Worker) (int, bool, bool) {
			matched := 0
			for _, rb := range resolved {
				if _, ok := h.WorkerCheckoutPath(ctx, w.ID, rb.repo.RemoteURL); ok {
					matched++
				} else if strings.TrimSpace(rb.repo.RemoteURL) == "" {
					return 0, false, false
				}
			}
			return matched, matched == len(resolved), true
		}

		dec, idx := h.applyProviderPlacement(ctx, agent, "", pinnedAgent, eligible)
		if dec.Wait {
			continue
		}
		agent = dec.Agent
		eligible = filterProviderWorkers(eligible, agent, idx)
		ticketKey := h.workKeyFor(ctx, store.Attempt{TicketID: tk.ID}, store.Task{ProjectID: tk.ProjectID, RepoID: firstBinding.RepoID})
		scope := h.placementScopeOf(ctx, tk.ProjectID, firstBinding.RepoID, "", cmpOr(firstBinding.TargetBranch, firstBinding.BaseBranch), ticketKey)
		w, pout, ok := SelectWorkerScored(eligible, counts, sel, agent, permMode,
			h.withAccounts(ctx, h.placementOpts(ctx, scope, peak, tk.ID, localityFn), agent, "", true))
		if !ok {
			continue
		}
		if agent == "" {
			agent = pickAllowedAgent(w, idx)
			if agent == "" {
				continue
			}
		}
		if h.checkAccountLoggedIn(ctx, w.ID, agent, "") != nil {
			continue
		}

		taskID := ids.UUID()
		taskSID, err := h.store.NewSID(ctx, ids.Task)
		if err != nil {
			continue
		}
		chatID := ids.UUID()
		chatSID, err := h.store.NewSID(ctx, ids.Chat)
		if err != nil {
			continue
		}

		if h.fleetPaused(ctx) {
			return
		}
		now := time.Now().UTC()
		claimedTk, claimed, err := h.store.ClaimTicket(ctx, tk.ID, w.ID, taskID, now, leaseTTL)
		if err != nil || !claimed {
			continue
		}
		h.logPlacement(ctx, "ticket:"+tk.ID, ticketKey, "scheduler", w.ID, pout, placementWorkInfo{peak: peak})

		pkt, err := h.CompileDispatchPacket(ctx, claimedTk.ID, PacketOptions{WorkerID: w.ID})
		if err != nil {
			h.logger.Warn("compile dispatch packet failed; releasing lease", "ticket_id", tk.ID, "err", err)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			continue
		}
		prompt := FormatDispatchPrompt(pkt)

		branch := firstBinding.TargetBranch
		if branch == "" {
			branch = firstBinding.BaseBranch
		}
		orphanPolicy := protocol.OrphanPolicyRequeue
		if tk.OrphanPolicy == store.TicketOrphanPolicyFail {
			orphanPolicy = protocol.OrphanPolicyFail
		}

		cs := store.ChatSession{
			ID:         chatID,
			SID:        chatSID,
			Title:      claimedTk.SID + ": " + tk.Title,
			Backend:    store.ChatBackendAgent,
			WorkerID:   w.ID,
			Agent:      agent,
			Model:      model,
			FleetTools: false,
			TaskID:     taskID,
			TicketID:   tk.ID,
			Status:     protocol.ChatStatusStarting,
			CreatedAt:  now,
			UpdatedAt:  now,
			// The account the scorer chose on this machine (most quota left; SYR-NCEHE).
			AccountID: chosenAccount(pout, w.ID),
		}
		h.resolveChatTarget(ctx, &cs)
		if h.ticketApprovalPolicy(ctx, tk.ID) != store.AutonomyApprovalAsk {
			var acctPtr *protocol.AgentAccount
			if a, ok := h.lookupAccount(ctx, w.ID, cs.Agent, cs.AccountID); ok {
				acctPtr = &a
			}
			if mode := ticketApprovalMode(cs.Agent, w.PermissionMode == store.PermissionSandboxed, acctPtr); mode != "" {
				cs.ApprovalMode = mode
			}
		}
		h.initNotesCursor(ctx, &cs)

		startedAt := now
		task := store.Task{
			ID:             taskID,
			SID:            taskSID,
			WorkerID:       w.ID,
			ProjectID:      tk.ProjectID,
			RepoID:         firstBinding.RepoID,
			Agent:          cs.Agent,
			Model:          cs.Model,
			SessionKind:    "chat",
			Branch:         branch,
			Mode:           "chat",
			Origin:         protocol.TaskOriginCommander,
			PermissionMode: permMode,
			OrphanPolicy:   orphanPolicy,
			Prompt:         prompt,
			Status:         protocol.TaskStatusRunning,
			ParentChatID:   chatID,
			Title:          tk.Title,
			TicketID:       tk.ID,
			TicketTitle:    tk.Title,
			Cause:          &causeScheduler,
			CreatedAt:      now,
			StartedAt:      &startedAt,
		}
		task.WorkKey = h.taskWorkKey(ctx, tk.ID, task, "", "")
		if err := h.store.CreateTask(ctx, task); err != nil {
			h.logger.Warn("create task for claimed ticket failed; releasing lease", "ticket_id", tk.ID, "err", err)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			continue
		}
		if err := h.store.CreateChatSession(ctx, cs); err != nil {
			h.logger.Warn("create chat session for claimed ticket failed; releasing lease", "ticket_id", tk.ID, "err", err)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			_, _ = h.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "chat creation failed", "", nil, causeScheduler)
			continue
		}
		h.broadcastFleetUpdate("chat_session", cs)

		attemptN := h.beginAttempt(ctx, store.Attempt{
			TicketID:     tk.ID,
			TaskID:       taskID,
			ChatID:       chatID,
			Attempt:      claimedTk.Attempt,
			WorkerID:     w.ID,
			AccountID:    cs.AccountID,
			Agent:        cs.Agent,
			Model:        cs.Model,
			QueuedAt:     &task.CreatedAt,
			StartedAt:    task.StartedAt,
			Substitution: dec.Substitution,
		})
		if dec.Substitution != "" {
			h.noteProviderSubstitution(ctx, taskID, tk.ID, dec.Substitution)
		}
		// Fleet pause beats an in-flight pass. The open is the point of no return:
		// a pause stored while the packet was compiled must not still dispatch.
		if h.fleetPaused(ctx) {
			h.abandonAttempt(ctx, tk.ID, taskID, attemptN)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			_, _ = h.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "fleet paused", "", nil, causeScheduler)
			h.closeChat(ctx, cs.ID)
			return
		}
		if err := h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: prompt}); err != nil {
			h.abandonAttempt(ctx, tk.ID, taskID, attemptN)
			h.logger.Warn("dispatch of ticket chat failed; releasing ticket back to ready",
				"ticket_id", tk.ID, "task_id", taskID, "chat_id", chatID, "worker_id", w.ID, "err", err)
			_, _, _ = h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReady)
			_, _ = h.store.UpdateTaskStatus(ctx, taskID, protocol.TaskStatusCanceled, "dispatch failed", "", nil, causeScheduler)
			h.closeChat(ctx, cs.ID)
			continue
		}

		if ov := lifted[w.ID]; ov != nil {
			h.useOverride(ctx, *ov)
		}
		h.placed(ctx, slots, w.ID, counts[w.ID])
		h.liftUsed(ctx, ticketLifts, w.ID)
		counts[w.ID]++
		if updatedTask, err := h.store.GetTask(ctx, taskID); err == nil {
			h.recordTaskTransition(ctx, updatedTask, "", causeScheduler, "placed ticket "+claimedTk.SID+" on "+w.Name)
			h.broadcastFleetUpdate("task", updatedTask)
		}
		h.broadcastFleetUpdate("ticket", claimedTk)
		h.markProjectActive(claimedTk.ProjectID)
	}
}

// ticketChatBindings builds the ticket SID and per-repo bindings for opening a ticket-backed
// agent chat on workerID.
func (h *Hub) ticketChatBindings(ctx context.Context, ticketID, workerID string) (string, []protocol.TaskRepoBinding) {
	if ticketID == "" {
		return "", nil
	}
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return "", nil
	}
	bindings, err := h.store.ListTicketRepoBindings(ctx, tk.ID)
	if err != nil || len(bindings) == 0 {
		return tk.SID, nil
	}
	type boundRepo struct {
		binding store.TicketRepoBinding
		repo    store.Repo
	}
	bound := make([]boundRepo, 0, len(bindings))
	for _, b := range bindings {
		repo, rErr := h.store.GetRepo(ctx, b.RepoID)
		if rErr != nil {
			continue
		}
		bound = append(bound, boundRepo{binding: b, repo: repo})
	}
	sort.Slice(bound, func(i, j int) bool {
		if bound[i].repo.Name != bound[j].repo.Name {
			return bound[i].repo.Name < bound[j].repo.Name
		}
		return bound[i].repo.ID < bound[j].repo.ID
	})
	out := make([]protocol.TaskRepoBinding, 0, len(bound))
	for _, br := range bound {
		checkoutPath := ""
		if workerID != "" {
			checkoutPath, _ = h.WorkerCheckoutPath(ctx, workerID, br.repo.RemoteURL)
		}
		baseBranch := strings.TrimSpace(br.binding.BaseBranch)
		if baseBranch == "" {
			baseBranch = strings.TrimSpace(br.repo.DefaultBranch)
		}
		targetBranch := strings.TrimSpace(br.binding.TargetBranch)
		if targetBranch == "" {
			targetBranch = "syre/" + tk.SID
		}
		var stackOn []string
		if stack := h.resolveRepoStack(ctx, tk.ID, br.repo.ID, br.repo.Name, baseBranch); len(stack) > 0 {
			stackOn = make([]string, 0, len(stack))
			for _, sb := range stack {
				stackOn = append(stackOn, sb.Branch)
			}
		}
		out = append(out, protocol.TaskRepoBinding{
			RepoName:     br.repo.Name,
			RemoteURL:    br.repo.RemoteURL,
			CheckoutPath: checkoutPath,
			BaseBranch:   baseBranch,
			TargetBranch: targetBranch,
			StackOn:      stackOn,
		})
	}
	return tk.SID, out
}

// SendTicketTurn sends a follow-up prompt into the ticket's open agent chat session
// (used by self-repair and review follow-ups). The ticket must be in running, needs_you,
// or review status.
func (h *Hub) SendTicketTurn(ctx context.Context, ticketID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("prompt text is required")
	}
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	switch tk.Status {
	case store.TicketStatusRunning, store.TicketStatusNeedsYou, store.TicketStatusReview:
	default:
		return fmt.Errorf("ticket %s is in status %q and cannot accept a turn", tk.SID, tk.Status)
	}
	cs, err := h.activeTicketChat(ctx, tk)
	if err != nil {
		return err
	}
	if tk.Status == store.TicketStatusReview {
		tk.Status = store.TicketStatusRunning
		if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
			h.broadcastFleetUpdate("ticket", updated)
		}
		if cs.TaskID != "" {
			_, _ = h.setTaskStatus(ctx, cs.TaskID, protocol.TaskStatusRunning, "", "", nil, causeScheduler)
		}
	}
	return h.sendAgentChat(ctx, cs, chatSendRequest{SessionID: cs.ID, Content: text})
}

func (h *Hub) activeTicketChat(ctx context.Context, tk store.Ticket) (store.ChatSession, error) {
	if tk.Lease != nil && tk.Lease.TaskID != "" {
		if cs, err := h.store.GetChatSessionByTaskID(ctx, tk.Lease.TaskID); err == nil {
			return cs, nil
		}
	}
	sessions, err := h.store.ListChatSessionsByTicket(ctx, tk.ID)
	if err != nil {
		return store.ChatSession{}, err
	}
	for _, cs := range sessions {
		if cs.Backend == store.ChatBackendAgent && cs.QuestionID == "" {
			return cs, nil
		}
	}
	return store.ChatSession{}, fmt.Errorf("no agent chat session found for ticket %s", tk.SID)
}

// closeTicketChats closes all agent chat sessions associated with ticketID.
func (h *Hub) closeTicketChats(ctx context.Context, ticketID string) {
	if ticketID == "" {
		return
	}
	sessions, err := h.store.ListChatSessionsByTicket(ctx, ticketID)
	if err != nil {
		return
	}
	for _, cs := range sessions {
		if cs.Backend == store.ChatBackendAgent && cs.QuestionID == "" {
			h.closeChat(ctx, cs.ID)
		}
	}
}

// stopTicketChatsWithGrace gracefully stops (cancel turn -> wait grace -> close session)
// all agent chat sessions associated with ticketID, returning how many were signaled.
func (h *Hub) stopTicketChatsWithGrace(ctx context.Context, ticketID string, graceS int) int {
	if ticketID == "" {
		return 0
	}
	graceS = protocol.NormalizeStopGraceS(graceS)
	sessions, err := h.store.ListChatSessionsByTicket(ctx, ticketID)
	if err != nil {
		return 0
	}
	n := 0
	for _, cs := range sessions {
		if cs.Backend == store.ChatBackendAgent && cs.QuestionID == "" {
			if _, err := h.stopChatWithGrace(ctx, cs, graceS, false); err == nil {
				n++
			}
		}
	}
	return n
}

// renewWorkerTicketLeases extends leases for every ticket leased to workerID whose
// chat session (or legacy task) is currently active in the worker's heartbeat.
func (h *Hub) renewWorkerTicketLeases(ctx context.Context, workerID string, activeTaskIDs, activeChatIDs []string, now time.Time) {
	if workerID == "" || (len(activeTaskIDs) == 0 && len(activeChatIDs) == 0) {
		return
	}
	leased, err := h.store.ListLeasedTicketsByWorker(ctx, workerID)
	if err != nil || len(leased) == 0 {
		return
	}
	activeTasks := make(map[string]bool, len(activeTaskIDs))
	for _, id := range activeTaskIDs {
		activeTasks[id] = true
	}
	activeChats := make(map[string]bool, len(activeChatIDs))
	for _, id := range activeChatIDs {
		activeChats[id] = true
	}
	var ticketIDs []string
	for _, tk := range leased {
		if tk.Lease == nil {
			continue
		}
		alive := false
		if tk.Lease.TaskID != "" {
			if cs, err := h.store.GetChatSessionByTaskID(ctx, tk.Lease.TaskID); err == nil {
				alive = activeChats[cs.ID]
			} else {
				alive = activeTasks[tk.Lease.TaskID]
			}
		} else if sessions, err := h.store.ListChatSessionsByTicket(ctx, tk.ID); err == nil && len(sessions) > 0 {
			for _, cs := range sessions {
				if activeChats[cs.ID] {
					alive = true
					break
				}
			}
		} else {
			alive = true
		}
		if alive {
			ticketIDs = append(ticketIDs, tk.ID)
		}
	}
	if len(ticketIDs) == 0 {
		return
	}
	if _, err := h.store.RenewLeases(ctx, workerID, ticketIDs, now, h.ticketLeaseTTL(ctx)); err != nil {
		h.logger.Warn("renew ticket leases failed", "worker_id", workerID, "err", err)
	}
}

// syncTicketFromChatEvent maps agent chat events onto the bound ticket and task run record:
//   - session_ready        -> persists provisioned workdir + worktrees on chat/task
//   - elicitation          -> ticket needs_you (lease kept, chat kept open)
//   - elicitation_resolved -> ticket running (lease kept, chat kept open)
//   - turn_end (ok)        -> persist digest, ticket review (lease released, chat kept open)
//   - turn_end (cancelled) -> persist digest, ticket canceled (lease released, chat closed)
//   - turn_end (error) / error -> persist digest with failure category, ticket failed (lease released, chat closed)
func (h *Hub) syncTicketFromChatEvent(ctx context.Context, cs store.ChatSession, ev protocol.AgentChatEvent) {
	if cs.TicketID == "" || cs.QuestionID != "" {
		return
	}
	tk, err := h.store.GetTicket(ctx, cs.TicketID)
	if err != nil {
		return
	}
	var task *store.Task
	if cs.TaskID != "" {
		if t, err := h.store.GetTask(ctx, cs.TaskID); err == nil {
			task = &t
		}
	}

	if ev.Kind == protocol.EventSessionReady {
		if ev.Workdir != "" && cs.Cwd != ev.Workdir {
			cs.Cwd = ev.Workdir
			_ = h.store.UpdateChatSession(ctx, cs)
		}
		if task != nil {
			if ev.Workdir != "" {
				_ = h.store.UpdateTaskAssignment(ctx, task.ID, cs.WorkerID, ev.Workdir)
			}
			if len(ev.Worktrees) > 0 {
				wts := make([]store.TaskWorktree, len(ev.Worktrees))
				for i, wt := range ev.Worktrees {
					wts[i] = store.TaskWorktree{
						RepoName:     wt.RepoName,
						CheckoutPath: wt.CheckoutPath,
						WorktreePath: wt.WorktreePath,
						Branch:       wt.Branch,
						BaseSHA:      wt.BaseSHA,
					}
				}
				_ = h.store.UpdateTaskWorktrees(ctx, task.ID, wts)
			}
		}
		return
	}

	// Build a fallback digest for hub-generated elicitations (ask_user) or errors when syred
	// did not attach one.
	dig := ev.Digest
	if dig == nil && task != nil {
		switch ev.Kind {
		case protocol.EventElicitation:
			if ev.Elicitation != nil {
				dig = &protocol.RunDigest{
					TicketSID: tk.SID,
					Status:    protocol.DigestStatusNeedsYou,
					Agent:     cs.Agent,
					Model:     cs.Model,
					Question:  &protocol.DigestQuestion{Text: protocol.FormatElicitationText(ev.Elicitation)},
				}
			}
		case protocol.EventError:
			if ev.Error != nil && ev.Error.Kind == protocol.ErrKindConflict {
				dig = &protocol.RunDigest{
					TicketSID: tk.SID,
					Status:    protocol.DigestStatusNeedsYou,
					Agent:     cs.Agent,
					Model:     cs.Model,
					Question:  &protocol.DigestQuestion{Text: ev.Error.Message},
				}
			} else if ev.Error != nil && (ev.Error.Kind != protocol.ErrKindTimeout || cs.Status == protocol.ChatStatusFailed) {
				cat := ev.Error.Kind
				if cat == "" || cat == protocol.ErrKindExit || cat == protocol.ErrKindOther || cat == protocol.ErrKindProtocol {
					cat = protocol.ErrKindAgentError
				}
				if cat == protocol.ErrKindNotFound {
					cat = protocol.ErrKindMissingTool
				}
				dig = &protocol.RunDigest{
					TicketSID: tk.SID,
					Status:    protocol.DigestStatusFailed,
					Agent:     cs.Agent,
					Model:     cs.Model,
					Summary:   ev.Error.Message,
					Failure:   &protocol.DigestFailure{Category: cat},
				}
			}
		}
	}
	// Keep merged digest around for pulse triggers below.
	var mergedDig *protocol.RunDigest
	if dig != nil {
		d := *dig
		// The ticket counter names the attempt. The worker's per-turn count
		// climbs on follow-ups that stay on the same attempt row.
		d.Attempt = max(tk.Attempt, 1)
		if task != nil {
			mergeTaskFollowupsIntoDigest(&d, task.Followups)
			if b, err := json.Marshal(&d, protocol.JSONOptions); err == nil {
				_ = h.store.UpdateTaskDigest(ctx, task.ID, string(b))
			}
		}
		mergedDig = &d
	}

	// Only drive ticket transitions if this chat belongs to the ticket's current run.
	if tk.Lease != nil && tk.Lease.TaskID != "" && cs.TaskID != "" && tk.Lease.TaskID != cs.TaskID {
		return
	}

	switch ev.Kind {
	case protocol.EventElicitation:
		if tk.Status == store.TicketStatusRunning || tk.Status == store.TicketStatusNeedsYou {
			if tk.Status == store.TicketStatusRunning {
				tk.Status = store.TicketStatusNeedsYou
				if updated, err := h.store.UpdateTicket(ctx, tk); err == nil {
					h.broadcastFleetUpdate("ticket", updated)
					h.pulseTicketTransition(ctx, updated, task, mergedDig, "question")
				}
			}
			h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, store.AttemptNeedsYou, "", nil, 0)
		}
		if task != nil && !protocol.IsTerminalTaskStatus(task.Status) {
			_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusWaitingInput, "", "", nil, causeScheduler)
		}

	case protocol.EventElicitationOK:
		if tk.Status == store.TicketStatusNeedsYou {
			tk.Status = store.TicketStatusRunning
			if updated, err := h.store.UpdateTicket(ctx, tk); err == nil {
				h.broadcastFleetUpdate("ticket", updated)
			}
		}
		if task != nil && task.Status == protocol.TaskStatusWaitingInput {
			_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusRunning, "", "", nil, causeScheduler)
		}

	case protocol.EventTurnEnd:
		if tk.Status != store.TicketStatusRunning && tk.Status != store.TicketStatusNeedsYou {
			return
		}
		summary := ""
		if dig != nil {
			summary = dig.Summary
		}
		switch ev.StopReason {
		case "cancelled":
			h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, store.AttemptCanceled, protocol.ErrKindCanceled, nil, 0)
			if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusCanceled); err == nil && changed {
				h.broadcastFleetUpdate("ticket", updated)
				h.checkProjectBoardIdle(ctx, updated.ProjectID)
			}
			if task != nil && !protocol.IsTerminalTaskStatus(task.Status) {
				_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusCanceled, summary, "", nil, causeScheduler)
			}
			h.closeChat(ctx, cs.ID)
		case "error":
			errCode := 1
			h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, store.AttemptFailed, "", &errCode, 0)
			if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusFailed); err == nil && changed {
				h.broadcastFleetUpdate("ticket", updated)
				h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindAgentError)
			}
			if task != nil && !protocol.IsTerminalTaskStatus(task.Status) {
				code := 1
				_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusFailed, summary, "", &code, causeScheduler)
			}
			h.closeChat(ctx, cs.ID)
		default:
			if isVerifyFailureDigest(dig) {
				retryLimit, escalateOnBlocker := h.ticketAutonomy(ctx, tk)
				currentAttempt := max(tk.Attempt, 1)
				if dig != nil && dig.Attempt > currentAttempt {
					currentAttempt = dig.Attempt
				}
				exitCode := 1
				if dig != nil && dig.Verify != nil && dig.Verify.Exit != 0 {
					exitCode = dig.Verify.Exit
				}
				h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, store.AttemptFailed, protocol.ErrKindVerify, &exitCode, currentAttempt)
				// Attempts the guardrails shed were not the work failing: they do not
				// spend the retry budget.
				if currentAttempt-h.ticketShedCount(ctx, tk.ID) < retryLimit {
					tk.Attempt = currentAttempt + 1
					if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
						h.broadcastFleetUpdate("ticket", updated)
					}
					repairN := 0
					if task != nil {
						repairN = h.beginAttempt(ctx, store.Attempt{
							TicketID:  tk.ID,
							TaskID:    task.ID,
							ChatID:    cs.ID,
							Attempt:   tk.Attempt,
							WorkerID:  cs.WorkerID,
							AccountID: cs.AccountID,
							Agent:     cs.Agent,
							Model:     cs.Model,
						})
					}
					repairPrompt := formatVerifyRepairPrompt(currentAttempt, retryLimit, dig)
					if sErr := h.SendTicketTurn(ctx, tk.ID, repairPrompt); sErr == nil {
						return
					} else {
						if task != nil {
							h.abandonAttempt(ctx, tk.ID, task.ID, repairN)
						}
						h.logger.Warn("self-repair turn dispatch failed; escalating ticket",
							"ticket_id", tk.ID, "ticket_sid", tk.SID, "attempt", currentAttempt, "err", sErr)
					}
				}
				// Retries exhausted (or repair dispatch failed). The failed row was
				// recorded above; exitCode is the verify command's exit.
				if escalateOnBlocker {
					if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusNeedsYou); err == nil && changed {
						h.broadcastFleetUpdate("ticket", updated)
						h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindVerify)
						h.kickScheduler()
					} else if tk.Lease == nil {
						tk.Status = store.TicketStatusNeedsYou
						if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
							h.broadcastFleetUpdate("ticket", updated)
							h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindVerify)
							h.kickScheduler()
						}
					}
					if task != nil && !protocol.IsTerminalTaskStatus(task.Status) {
						_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusWaitingInput, summary, "", &exitCode, causeScheduler)
					}
					return
				}
				if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusFailed); err == nil && changed {
					h.broadcastFleetUpdate("ticket", updated)
					h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindVerify)
					h.kickScheduler()
				} else if tk.Lease == nil {
					tk.Status = store.TicketStatusFailed
					if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
						h.broadcastFleetUpdate("ticket", updated)
						h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindVerify)
						h.kickScheduler()
					}
				}
				if task != nil && !protocol.IsTerminalTaskStatus(task.Status) {
					_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusFailed, summary, "", &exitCode, causeScheduler)
				}
				h.closeChat(ctx, cs.ID)
				return
			}
			// Optional cross-agent review before the ticket reaches review (crossreview.go).
			if h.maybeStartCrossReview(ctx, tk, cs, task, mergedDig, summary) {
				return
			}
			h.completeTicketRun(ctx, tk, cs, task, mergedDig, summary)
		}

	case protocol.EventError:
		// Watchdog timeout keeps the turn running; ignore unless chat actually failed.
		if ev.Error != nil && ev.Error.Kind == protocol.ErrKindTimeout && cs.Status != protocol.ChatStatusFailed {
			return
		}
		if tk.Status != store.TicketStatusRunning && tk.Status != store.TicketStatusNeedsYou {
			return
		}
		if ev.Error != nil && ev.Error.Kind == protocol.ErrKindConflict {
			errCode := 1
			h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, store.AttemptNeedsYou, protocol.ErrKindConflict, &errCode, 0)
			if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusNeedsYou); err == nil && changed {
				h.broadcastFleetUpdate("ticket", updated)
				h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindConflict)
				h.kickScheduler()
			} else if tk.Lease == nil {
				tk.Status = store.TicketStatusNeedsYou
				if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
					h.broadcastFleetUpdate("ticket", updated)
					h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindConflict)
					h.kickScheduler()
				}
			}
			if task != nil {
				_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusWaitingInput, ev.Error.Message, "", &errCode, causeScheduler)
			}
			h.closeChat(ctx, cs.ID)
			return
		}
		if ev.Error != nil && ev.Error.Kind == protocol.ErrKindMemoryPressureShed {
			h.handleTicketShed(ctx, tk, cs, task, mergedDig, ev.Error)
			return
		}
		errCode := 1
		if ev.Error != nil && ev.Error.ExitCode != nil {
			errCode = *ev.Error.ExitCode
		}
		h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, store.AttemptFailed, "", &errCode, 0)
		if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusFailed); err == nil && changed {
			h.broadcastFleetUpdate("ticket", updated)
			h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindAgentError)
		} else if tk.Lease == nil {
			tk.Status = store.TicketStatusFailed
			if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
				h.broadcastFleetUpdate("ticket", updated)
				h.pulseTicketTransition(ctx, updated, task, mergedDig, protocol.ErrKindAgentError)
			}
		}
		if task != nil {
			msg := ""
			var code *int
			if ev.Error != nil {
				msg = ev.Error.Message
				code = ev.Error.ExitCode
			}
			if code == nil {
				c := 1
				code = &c
			}
			_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusFailed, msg, "", code, causeScheduler)
		}
		h.closeChat(ctx, cs.ID)
	}
}

// isTaskLeasedTicket reports whether t is currently the active leased task of a Syre ticket.
func (h *Hub) isTaskLeasedTicket(ctx context.Context, t store.Task) bool {
	if t.TicketID == "" {
		return false
	}
	tk, err := h.store.GetTicket(ctx, t.TicketID)
	return err == nil && tk.Lease != nil && tk.Lease.TaskID == t.ID
}

// expireTicketLeases expires leases past their deadline and applies each ticket's orphan_policy.
func (h *Hub) expireTicketLeases(ctx context.Context, now time.Time) {
	expired, err := h.store.ExpireLeases(ctx, now)
	if err != nil || len(expired) == 0 {
		return
	}
	requeued := false
	for _, ex := range expired {
		h.logger.Warn("ticket lease expired",
			"ticket_id", ex.Ticket.ID,
			"ticket_sid", ex.Ticket.SID,
			"worker_id", ex.WorkerID,
			"task_id", ex.TaskID,
			"orphan_policy", ex.Ticket.OrphanPolicy,
			"next_status", ex.Ticket.Status,
		)
		if ex.TaskID != "" {
			h.markAttemptLost(ctx, ex.Ticket.ID, ex.TaskID, ex.WorkerID, ex.Ticket.Attempt)
			h.forgetTask(ex.TaskID)
			code := -1
			_, _ = h.setTaskStatus(ctx, ex.TaskID, protocol.TaskStatusFailed, "ticket lease expired", "", &code, causeReaper)
		}
		h.closeTicketChats(ctx, ex.Ticket.ID)
		h.broadcastFleetUpdate("ticket", ex.Ticket)
		if ex.Ticket.Status == store.TicketStatusReady {
			requeued = true
		} else if ex.Ticket.Status == store.TicketStatusFailed {
			h.pulseTicketTransition(ctx, ex.Ticket, nil, nil, protocol.ErrKindTimeout)
		}
	}
	if requeued {
		h.kickScheduler()
	}
}

// RequeueTicket breaks any active lease on ticketID, moves it back to ready, cancels its
// running task (if any), and records an attributed audit + fleet timeline entry.
func (h *Hub) RequeueTicket(ctx context.Context, ticketID, note string) (store.Ticket, error) {
	return h.breakTicketLease(ctx, ticketID, store.TicketStatusReady, AuditTicketRequeue, "requeue", note, 0)
}

// CancelTicket breaks any active lease on ticketID, moves it to canceled, cancels its
// running task (if any), and records an attributed audit + fleet timeline entry.
func (h *Hub) CancelTicket(ctx context.Context, ticketID, note string) (store.Ticket, error) {
	return h.CancelTicketWithGrace(ctx, ticketID, note, protocol.DefaultStopGraceS)
}

// CancelTicketWithGrace gracefully stops a ticket's chat(s) and leased task with graceS
// before moving it to canceled, with audit attribution and batched commander fleet events.
func (h *Hub) CancelTicketWithGrace(ctx context.Context, ticketID, note string, graceS int) (store.Ticket, error) {
	return h.breakTicketLease(ctx, ticketID, store.TicketStatusCanceled, AuditTicketCancel, "cancel", note, protocol.NormalizeStopGraceS(graceS))
}

func (h *Hub) breakTicketLease(ctx context.Context, ticketID, nextStatus, auditAction, verb, note string, graceS int) (store.Ticket, error) {
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return store.Ticket{}, err
	}
	cause := causeFrom(ctx)
	defer h.beginDeliveryBatch()()
	priorStatus := tk.Status
	var priorWorkerID, priorTaskID string
	if tk.Lease != nil {
		priorWorkerID = tk.Lease.WorkerID
		priorTaskID = tk.Lease.TaskID
	}

	updated, _, err := h.store.ReleaseTicket(ctx, tk.ID, nextStatus)
	if err != nil {
		return store.Ticket{}, err
	}

	// Stop or close any agent chat sessions bound to this ticket and cancel the prior task.
	if graceS > 0 {
		h.stopTicketChatsWithGrace(ctx, tk.ID, graceS)
	} else {
		h.closeTicketChats(ctx, tk.ID)
	}
	if priorTaskID != "" {
		if t, err := h.store.GetTask(ctx, priorTaskID); err == nil && !protocol.IsTerminalTaskStatus(t.Status) {
			h.setPendingCause(priorTaskID, protocol.TaskStatusCanceled, cause)
			var sent bool
			if graceS > 0 {
				sent = h.SendCancelWithGrace(priorTaskID, graceS)
			} else {
				sent = h.SendCancel(priorTaskID)
			}
			if !sent {
				_, _ = h.setTaskStatus(ctx, priorTaskID, protocol.TaskStatusCanceled, "ticket "+verb+"d", "", nil, cause)
			}
		}
	}

	msg := fmt.Sprintf("%s %sd ticket %s (%s)", capitalize(describeActor(cause)), verb, tk.SID, tk.Title)
	if n := strings.TrimSpace(note); n != "" {
		msg += ": " + n
	}
	meta := map[string]any{
		"ticket_id":    tk.ID,
		"ticket_sid":   tk.SID,
		"prior_status": priorStatus,
		"next_status":  updated.Status,
		"note":         strings.TrimSpace(note),
	}
	if graceS > 0 {
		meta["grace_s"] = graceS
	}
	h.recordAudit(ctx, store.AuditEvent{
		Action:   auditAction,
		WorkerID: priorWorkerID,
		TaskID:   priorTaskID,
		Message:  msg,
		MetaJSON: auditMeta(meta),
	})
	h.recordIntervention(ctx, cause, auditAction, priorWorkerID, priorTaskID, "", msg)
	h.broadcastFleetUpdate("ticket", updated)
	if nextStatus == store.TicketStatusReady {
		h.kickScheduler()
	} else if nextStatus == store.TicketStatusCanceled {
		h.checkProjectBoardIdle(ctx, updated.ProjectID)
	}
	return updated, nil
}

// completeTicketRun records a verified, successful run and moves the ticket to review
// (releasing its lease), or back to review after a follow-up turn.
func (h *Hub) completeTicketRun(ctx context.Context, tk store.Ticket, cs store.ChatSession, task *store.Task, mergedDig *protocol.RunDigest, summary string) {
	okStatus := store.AttemptReview
	if mergedDig != nil && mergedDig.Status == protocol.DigestStatusCompleted {
		okStatus = store.AttemptCompleted
	}
	if mergedDig != nil && mergedDig.Failure != nil && mergedDig.Failure.Category == protocol.ErrKindCanceled {
		okStatus = store.AttemptCanceled
	}
	okCode := 0
	h.finishTicketAttempt(ctx, tk, cs, task, mergedDig, okStatus, "", &okCode, 0)
	if updated, changed, err := h.store.ReleaseTicket(ctx, tk.ID, store.TicketStatusReview); err == nil && changed {
		h.broadcastFleetUpdate("ticket", updated)
		h.pulseTicketTransition(ctx, updated, task, mergedDig, "")
		h.kickScheduler()
	} else if tk.Lease == nil {
		// Follow-up turn from review (no active lease): transition back to review.
		tk.Status = store.TicketStatusReview
		if updated, uErr := h.store.UpdateTicket(ctx, tk); uErr == nil {
			h.broadcastFleetUpdate("ticket", updated)
			h.pulseTicketTransition(ctx, updated, task, mergedDig, "")
			h.kickScheduler()
		}
	}
	if task != nil {
		code := 0
		_, _ = h.setTaskStatus(ctx, task.ID, protocol.TaskStatusCompleted, summary, "", &code, causeScheduler)
	}
}
