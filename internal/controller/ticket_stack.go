package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/BryanSant/syre/internal/forge"
	"github.com/BryanSant/syre/internal/store"
)

// stackedBlocker identifies an unmerged blocker ticket and the git branch it produced
// for a specific repository.
type stackedBlocker struct {
	TicketID  string
	TicketSID string
	Branch    string
}

// resolveRepoStack computes the ordered list of transitively relevant direct blockers for
// ticketID on repoID/repoName that have not yet merged into baseBranch.
//
// A blocker contributes a branch only if:
//   - it is reachable via blocked_by edges from ticketID,
//   - it has a repo binding for repoID and is in done or review status,
//   - it has a recorded worktree/task branch (from its latest task) different from baseBranch,
//   - its latest task has not been merged by delivery.
//
// If blocker X is an ancestor of blocker Y and both are unmerged on repoID, X is omitted
// because Y's branch already includes X's commits.
func (h *Hub) resolveRepoStack(ctx context.Context, ticketID, repoID, repoName, baseBranch string) []stackedBlocker {
	if ticketID == "" || repoID == "" {
		return nil
	}
	baseBranch = strings.TrimSpace(baseBranch)

	// Walk blocked_by edges transitively from ticketID.
	edgesByTicket := make(map[string][]string)
	visited := map[string]bool{ticketID: true}
	queue := []string{ticketID}
	var ancestors []string

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		edges, err := h.store.ListTicketEdges(ctx, cur)
		if err != nil {
			continue
		}
		for _, e := range edges {
			if e.BlockedByID == "" {
				continue
			}
			edgesByTicket[cur] = append(edgesByTicket[cur], e.BlockedByID)
			if !visited[e.BlockedByID] {
				visited[e.BlockedByID] = true
				ancestors = append(ancestors, e.BlockedByID)
				queue = append(queue, e.BlockedByID)
			}
		}
	}
	if len(ancestors) == 0 {
		return nil
	}

	candidates := make(map[string]stackedBlocker)
	for _, ancID := range ancestors {
		blk, err := h.store.GetTicket(ctx, ancID)
		if err != nil {
			continue
		}
		if blk.Status != store.TicketStatusDone && blk.Status != store.TicketStatusReview {
			continue
		}
		blkBindings, err := h.store.ListTicketRepoBindings(ctx, ancID)
		if err != nil {
			continue
		}
		var matchedBinding *store.TicketRepoBinding
		for i := range blkBindings {
			if blkBindings[i].RepoID == repoID {
				matchedBinding = &blkBindings[i]
				break
			}
		}
		if matchedBinding == nil {
			continue
		}

		tasks, err := h.store.ListTasksByTicket(ctx, ancID)
		if err != nil || len(tasks) == 0 {
			continue
		}
		latest := tasks[0]
		if h.isTaskMergedByDelivery(ctx, latest) {
			continue
		}
		branch := recordedTaskRepoBranch(latest, repoID, repoName, matchedBinding.TargetBranch, blk.SID)
		if branch == "" || (baseBranch != "" && branch == baseBranch) {
			continue
		}
		candidates[ancID] = stackedBlocker{
			TicketID:  ancID,
			TicketSID: blk.SID,
			Branch:    branch,
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Remove any candidate that is a transitive blocker of another candidate, because
	// the downstream candidate's branch already stacked on the upstream one.
	isAncestor := func(wantAnc, start string) bool {
		seen := map[string]bool{start: true}
		q := []string{start}
		for len(q) > 0 {
			cur := q[0]
			q = q[1:]
			for _, next := range edgesByTicket[cur] {
				if next == wantAnc {
					return true
				}
				if !seen[next] {
					seen[next] = true
					q = append(q, next)
				}
			}
		}
		return false
	}

	out := make([]stackedBlocker, 0, len(candidates))
	for id, cand := range candidates {
		covered := false
		for otherID := range candidates {
			if otherID == id {
				continue
			}
			if isAncestor(id, otherID) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, cand)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].TicketSID != out[j].TicketSID {
			return out[i].TicketSID < out[j].TicketSID
		}
		return out[i].TicketID < out[j].TicketID
	})

	// Deduplicate identical branch names while preserving order.
	seenBranch := make(map[string]bool, len(out))
	dedup := out[:0]
	for _, sb := range out {
		if seenBranch[sb.Branch] {
			continue
		}
		seenBranch[sb.Branch] = true
		dedup = append(dedup, sb)
	}
	return dedup
}

// isTaskMergedByDelivery reports whether delivery merged t's branch into its base branch.
// keep_branch delivery marks the review delivered without merging, so it returns false.
func (h *Hub) isTaskMergedByDelivery(ctx context.Context, t store.Task) bool {
	if t.ID == "" {
		return false
	}
	rv, err := h.store.GetReview(ctx, t.ID, "")
	if err != nil {
		return false
	}
	if rv.PRState == string(forge.PRMerged) {
		return true
	}
	return rv.DeliveryStatus == store.DeliveryStatusDelivered && rv.DeliveryMode != store.DeliveryModeKeepBranch
}

// recordedTaskRepoBranch extracts the branch recorded for repoName/repoID on task t.
func recordedTaskRepoBranch(t store.Task, repoID, repoName, bindingTarget, ticketSID string) string {
	for _, wt := range t.Worktrees {
		if wt.RepoName == repoName && strings.TrimSpace(wt.Branch) != "" {
			return strings.TrimSpace(wt.Branch)
		}
	}
	if t.RepoID == repoID && strings.TrimSpace(t.Branch) != "" {
		return strings.TrimSpace(t.Branch)
	}
	if b := strings.TrimSpace(bindingTarget); b != "" {
		return b
	}
	if ticketSID != "" {
		return "syre/" + ticketSID
	}
	return ""
}

// formatStackedPacketLine renders one repo's stacked line for DispatchPacket,
// e.g. "demo: stacked on tk-fnwgh (syre/tk-fnwgh)".
func formatStackedPacketLine(repoName string, stack []stackedBlocker) string {
	if len(stack) == 0 {
		return ""
	}
	parts := make([]string, 0, len(stack))
	for _, sb := range stack {
		parts = append(parts, fmt.Sprintf("%s (%s)", sb.TicketSID, sb.Branch))
	}
	return fmt.Sprintf("%s: stacked on %s", repoName, strings.Join(parts, ", "))
}

// ticketStackRetargetBaseSHA returns the recorded worktree BaseSHA for a ticket task when
// the ticket depended on blocker(s) in the same repo that have since merged into baseBranch.
// Delivery uses this SHA to rebase --onto <base> <baseSHA> before merging or pushing so
// already-merged blocker commits are cleanly stripped.
func (h *Hub) ticketStackRetargetBaseSHA(ctx context.Context, t store.Task, baseBranch string) string {
	if t.TicketID == "" || t.RepoID == "" || len(t.Worktrees) == 0 {
		return ""
	}
	repo, err := h.store.GetRepo(ctx, t.RepoID)
	if err != nil {
		return ""
	}
	var baseSHA string
	for _, wt := range t.Worktrees {
		if wt.RepoName == repo.Name && strings.TrimSpace(wt.BaseSHA) != "" {
			baseSHA = strings.TrimSpace(wt.BaseSHA)
			break
		}
	}
	if baseSHA == "" {
		return ""
	}
	// Only retarget if the ticket actually had a blocker on this repo AND no unmerged
	// blockers remain on this repo (all blockers have merged into base).
	if !h.ticketHasAnyRepoBlocker(ctx, t.TicketID, t.RepoID) {
		return ""
	}
	if remaining := h.resolveRepoStack(ctx, t.TicketID, t.RepoID, repo.Name, baseBranch); len(remaining) > 0 {
		return ""
	}
	return baseSHA
}

func (h *Hub) ticketHasAnyRepoBlocker(ctx context.Context, ticketID, repoID string) bool {
	visited := map[string]bool{ticketID: true}
	queue := []string{ticketID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		edges, err := h.store.ListTicketEdges(ctx, cur)
		if err != nil {
			continue
		}
		for _, e := range edges {
			if e.BlockedByID == "" || visited[e.BlockedByID] {
				continue
			}
			visited[e.BlockedByID] = true
			if bindings, err := h.store.ListTicketRepoBindings(ctx, e.BlockedByID); err == nil {
				for _, b := range bindings {
					if b.RepoID == repoID {
						return true
					}
				}
			}
			queue = append(queue, e.BlockedByID)
		}
	}
	return false
}
