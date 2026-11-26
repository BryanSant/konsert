package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/BryanSant/syre/internal/forge"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// deliveryRank orders outcomes from best to worst; a multi-repo delivery reports the
// worst of its repos.
var deliveryRank = map[string]int{
	store.DeliveryStatusDelivered: 0,
	store.DeliveryStatusAutoMerge: 1,
	store.DeliveryStatusPROpen:    2,
	store.DeliveryStatusManual:    3,
	store.DeliveryStatusOffline:   4,
	store.DeliveryStatusClosed:    5,
	store.DeliveryStatusConflict:  6,
	store.DeliveryStatusFailed:    7,
}

// deliverMultiRepo delivers each of a multi-repo ticket's worktrees in turn (each one
// through the single-repo path, scoped to that repo) and records one combined review:
// the worst status, a per-repo message, the first repo's PR in the PR fields and the
// others in RepoPRs. One repo failing doesn't stop the others.
func (h *Hub) deliverMultiRepo(ctx context.Context, svc *FleetService, t store.Task, cur store.Review) (ReviewResult, error) {
	repoByName := map[string]store.Repo{}
	if t.ProjectID != "" {
		if repos, err := h.store.ListRepos(ctx, t.ProjectID); err == nil {
			for _, r := range repos {
				repoByName[r.Name] = r
			}
		}
	}
	prior := map[string]store.RepoPR{}
	for _, p := range cur.RepoPRs {
		prior[p.Repo] = p
	}

	var (
		parts    []string
		worst    = store.DeliveryStatusDelivered
		mode     string
		primary  *store.Review
		repoPRs  []store.RepoPR
		lastRes  ReviewResult
		firstErr error
	)
	for i, wt := range t.Worktrees {
		one := t
		one.Worktrees = []store.TaskWorktree{wt}
		one.EffectiveDir, one.Workdir, one.Branch = wt.WorktreePath, wt.WorktreePath, wt.Branch
		if r, ok := repoByName[wt.RepoName]; ok {
			one.RepoID = r.ID
		}
		// Each repo sees its own earlier PR (re-approval reuses it, not the first repo's).
		curOne := cur
		curOne.RepoPRs = nil
		if i > 0 {
			p := prior[wt.RepoName]
			curOne.PRNumber, curOne.PRURL, curOne.PRState, curOne.PRChecks, curOne.PRRemoteURL = p.Number, p.URL, p.State, p.Checks, p.RemoteURL
		}
		_, _ = h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.PRNumber, r.PRURL, r.PRState, r.PRChecks, r.PRRemoteURL = curOne.PRNumber, curOne.PRURL, curOne.PRState, curOne.PRChecks, curOne.PRRemoteURL
		})
		res, err := h.deliverApproved(ctx, svc, one, curOne)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		rv := res.Review
		lastRes = res
		mode = rv.DeliveryMode
		status := rv.DeliveryStatus
		if status == "" {
			status = store.DeliveryStatusFailed
		}
		if deliveryRank[status] > deliveryRank[worst] {
			worst = status
		}
		parts = append(parts, wt.RepoName+": "+strings.TrimSpace(rv.DeliveryMessage))
		if i == 0 {
			p := rv
			primary = &p
		} else if rv.PRNumber > 0 {
			repoPRs = append(repoPRs, store.RepoPR{Repo: wt.RepoName, Number: rv.PRNumber, URL: rv.PRURL, RemoteURL: rv.PRRemoteURL, State: rv.PRState, Checks: rv.PRChecks})
		}
	}

	final, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
		r.DeliveryMode = mode
		r.DeliveryStatus = worst
		r.DeliveryMessage = strings.Join(parts, " ")
		if primary != nil {
			r.PRNumber, r.PRURL, r.PRState, r.PRChecks, r.PRRemoteURL = primary.PRNumber, primary.PRURL, primary.PRState, primary.PRChecks, primary.PRRemoteURL
			r.CompareURL, r.MergeSHA = primary.CompareURL, primary.MergeSHA
		}
		r.RepoPRs = repoPRs
	})
	if err != nil && firstErr == nil {
		firstErr = err
	}
	lastRes.Review = final
	return lastRes, firstErr
}

// pollRepoPRs refreshes the extra repos' PRs of a multi-repo review and reports whether
// every one of them is merged (closed counts as not delivered).
func (h *Hub) pollRepoPRs(ctx context.Context, resolver forge.Resolver, r store.Review) (all bool, anyClosed bool, prs []store.RepoPR, changed bool) {
	all = true
	for _, p := range r.RepoPRs {
		if p.State == "" || p.State == string(forge.PROpen) {
			if fRepo, err := resolver.Detect(ctx, p.RemoteURL); err == nil {
				if client, err := resolver.Client(ctx, fRepo); err == nil {
					if pr, err := client.GetPR(ctx, fRepo, p.Number); err == nil {
						if string(pr.State) != p.State || string(pr.Checks) != p.Checks {
							p.State, p.Checks, changed = string(pr.State), string(pr.Checks), true
							if pr.URL != "" {
								p.URL = pr.URL
							}
						}
					}
				}
			}
		}
		switch p.State {
		case string(forge.PRMerged):
		case string(forge.PRClosed):
			anyClosed, all = true, false
		default:
			all = false
		}
		prs = append(prs, p)
	}
	return all, anyClosed, prs, changed
}

func repoPRSummary(prs []store.RepoPR) string {
	var open []string
	for _, p := range prs {
		if p.State != string(forge.PRMerged) {
			open = append(open, fmt.Sprintf("%s #%d", p.Repo, p.Number))
		}
	}
	return strings.Join(open, ", ")
}

func hasOpenRepoPR(r store.Review) bool {
	for _, p := range r.RepoPRs {
		if p.State == "" || p.State == string(forge.PROpen) {
			return true
		}
	}
	return false
}

// stackedPRBase returns the branch a ticket's PR should target while it is stacked: the
// branch of its single unmerged blocker on this repo, if that blocker has an open PR
// (so the branch exists on the forge). "" means target the default branch.
func (h *Hub) stackedPRBase(ctx context.Context, t store.Task, defaultBranch string) string {
	if t.TicketID == "" || t.RepoID == "" {
		return ""
	}
	repo, err := h.store.GetRepo(ctx, t.RepoID)
	if err != nil {
		return ""
	}
	stack := h.resolveRepoStack(ctx, t.TicketID, t.RepoID, repo.Name, defaultBranch)
	if len(stack) != 1 || stack[0].Branch == "" {
		return ""
	}
	tasks, err := h.store.ListTasksByTicket(ctx, stack[0].TicketID)
	if err != nil {
		return ""
	}
	for _, bt := range tasks {
		if rv, err := h.store.GetReview(ctx, bt.ID, ""); err == nil && rv.PRNumber > 0 &&
			(rv.PRState == "" || rv.PRState == string(forge.PROpen)) {
			return stack[0].Branch
		}
	}
	return ""
}

// retargetStackedPR moves an open PR that was stacked on a blocker's branch onto the
// default branch once the stack is gone (the blocker merged): rebase the branch onto the
// default branch, force-push with lease, and change the PR's base on the forge.
func (h *Hub) retargetStackedPR(ctx context.Context, client forge.Client, fRepo forge.Repo, r store.Review, task store.Task) {
	if r.PRBase == "" || r.PRNumber <= 0 || task.WorkerID == "" || !h.IsWorkerConnected(task.WorkerID) {
		return
	}
	cfg := h.resolveDelivery(ctx, task, true)
	def := cfg.defaultBranch
	if info, err := client.RepoInfo(ctx, fRepo); err == nil && info.DefaultBranch != "" {
		def = info.DefaultBranch
	}
	if r.PRBase == def || h.stackedPRBase(ctx, task, def) != "" {
		return
	}
	pushRes, err := h.workerGitPush(ctx, task.WorkerID, protocol.GitPushRequest{
		Workdir: cfg.workdir, Branch: cfg.branch, Remote: cfg.remoteName,
		RebaseOnto: def, StackBaseSHA: h.ticketStackRetargetBaseSHA(ctx, task, def),
	})
	if err != nil || !pushRes.OK {
		h.logger.Warn("retarget stacked PR: rebase/push failed", "task_id", task.ID, "pr", r.PRNumber, "err", err, "push_error", pushRes.Error)
		return
	}
	if err := client.UpdatePRBase(ctx, fRepo, r.PRNumber, def); err != nil {
		h.logger.Warn("retarget stacked PR: update base failed", "task_id", task.ID, "pr", r.PRNumber, "err", err)
		return
	}
	msg := fmt.Sprintf("PR #%d retargeted from %s to %s after the PR it was stacked on merged.", r.PRNumber, r.PRBase, def)
	_, _ = h.saveReview(ctx, task.ID, "", func(rv *store.Review) {
		rv.PRBase = def
		rv.DeliveryMessage = msg
	})
	h.recordDeliveryOutcome(ctx, task, r.DeliveryMode, r.DeliveryStatus, msg, map[string]any{"pr_number": r.PRNumber, "base": def})
}
