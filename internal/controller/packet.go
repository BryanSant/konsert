package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// DispatchStandingInstruction is appended as one standing line after the compiled
// dispatch packet when dispatching a ticket task.
const DispatchStandingInstruction = "Work only in the repos listed. Use ticket_check as you complete acceptance items, ticket_followup for out-of-scope findings, ask_user when blocked."

// DefaultTicketTools is the agent-facing ticket tool set advertised in a dispatch packet.
var DefaultTicketTools = []string{
	"ticket_get",
	"ticket_check",
	"ticket_comment",
	"ticket_followup",
	"ask_user",
}

// PacketOptions tunes dispatch packet compilation (placement context and worktrees).
type PacketOptions struct {
	// WorkerID, when set, is used to resolve the worker's WorkdirRoot from its
	// reported machine facts so repo paths are filled as <root>/<ticket sid>/<repo>.
	WorkerID string
	// WorkdirRoot overrides the worker's reported WorkdirRoot when non-empty.
	WorkdirRoot string
	// Worktrees, when non-empty, supplies already-provisioned worktree paths by repo name.
	Worktrees []store.TaskWorktree
}

// FormatDispatchPrompt combines a compiled dispatch packet with the standing worker instruction.
func FormatDispatchPrompt(pkt protocol.DispatchPacket) string {
	return pkt.Text() + "\n\n" + DispatchStandingInstruction
}

// CompileDispatchPacket builds a protocol.DispatchPacket for ticketID from store state
// (project, bound repos, unchecked acceptance items, done blocker interface digests,
// verify command precedence, and tool list).
func (h *Hub) CompileDispatchPacket(ctx context.Context, ticketID string, opts PacketOptions) (protocol.DispatchPacket, error) {
	tk, err := h.store.GetTicket(ctx, ticketID)
	if err != nil {
		return protocol.DispatchPacket{}, err
	}

	var proj store.Project
	if tk.ProjectID != "" {
		if p, err := h.store.GetProject(ctx, tk.ProjectID); err == nil {
			proj = p
		}
	}

	// Resolve already-provisioned worktree paths if available (explicit opts or active leased task).
	wtByRepo := make(map[string]string)
	for _, wt := range opts.Worktrees {
		if wt.RepoName != "" && wt.WorktreePath != "" {
			wtByRepo[wt.RepoName] = wt.WorktreePath
		}
	}
	if len(wtByRepo) == 0 && tk.Lease != nil && tk.Lease.TaskID != "" {
		if t, err := h.store.GetTask(ctx, tk.Lease.TaskID); err == nil {
			for _, wt := range t.Worktrees {
				if wt.RepoName != "" && wt.WorktreePath != "" {
					wtByRepo[wt.RepoName] = wt.WorktreePath
				}
			}
		}
	}

	// Resolve workdirRoot after placement (explicit override > worker facts > lease worker facts).
	workdirRoot := strings.TrimSpace(opts.WorkdirRoot)
	workerID := strings.TrimSpace(opts.WorkerID)
	if workerID == "" && tk.Lease != nil {
		workerID = strings.TrimSpace(tk.Lease.WorkerID)
	}
	if workdirRoot == "" && workerID != "" {
		workdirRoot = h.workerWorkdirRoot(ctx, workerID)
	}

	bindings, err := h.store.ListTicketRepoBindings(ctx, tk.ID)
	if err != nil {
		return protocol.DispatchPacket{}, err
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

	repos := make([]protocol.DispatchPacketRepo, 0, len(bound))
	var repoVerifyCmds []string
	for _, br := range bound {
		branch := strings.TrimSpace(br.binding.TargetBranch)
		if branch == "" {
			branch = "syre/" + tk.SID
		}
		repoPath := wtByRepo[br.repo.Name]
		if repoPath == "" && workdirRoot != "" {
			repoPath = joinWorktreePath(workdirRoot, tk.SID, br.repo.Name)
		}
		repos = append(repos, protocol.DispatchPacketRepo{
			Name:   br.repo.Name,
			Path:   repoPath,
			Branch: branch,
		})
		if cmd := singleLine(br.repo.VerifyCmd); cmd != "" && !slices.Contains(repoVerifyCmds, cmd) {
			repoVerifyCmds = append(repoVerifyCmds, cmd)
		}
	}

	// Acceptance: unchecked items only.
	var acceptance []string
	for _, item := range tk.Acceptance {
		if item.Done {
			continue
		}
		if text := singleLine(item.Text); text != "" {
			acceptance = append(acceptance, text)
		}
	}

	// Contracts: for each blocked_by ticket that is done, pull interface from its latest task digest.
	var contracts []string
	if edges, err := h.store.ListTicketEdges(ctx, tk.ID); err == nil {
		for _, e := range edges {
			blocker, bErr := h.store.GetTicket(ctx, e.BlockedByID)
			if bErr != nil || blocker.Status != store.TicketStatusDone {
				continue
			}
			if iface := h.latestTicketInterfaceDigest(ctx, blocker.ID); iface != "" {
				contracts = append(contracts, fmt.Sprintf("%s (done): %s", blocker.SID, iface))
			}
		}
	}

	// Verify precedence: ticket verify_cmd > repo verify_cmd > project verify_cmd.
	verifyCmd := singleLine(tk.VerifyCmd)
	if verifyCmd == "" && len(repoVerifyCmds) > 0 {
		verifyCmd = strings.Join(repoVerifyCmds, " && ")
	}
	if verifyCmd == "" {
		verifyCmd = singleLine(proj.VerifyCmd)
	}

	return protocol.DispatchPacket{
		TicketSID:  tk.SID,
		Project:    strings.TrimSpace(proj.Name),
		Repos:      repos,
		Objective:  buildPacketObjective(tk.Title, tk.Body),
		Acceptance: acceptance,
		Contracts:  contracts,
		Verify:     verifyCmd,
		Tools:      append([]string(nil), DefaultTicketTools...),
	}, nil
}

func (h *Hub) workerWorkdirRoot(ctx context.Context, workerID string) string {
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return ""
	}
	if f := decodeWorkerFacts(w.FactsJSON); f != nil && strings.TrimSpace(f.WorkdirRoot) != "" {
		return strings.TrimSpace(f.WorkdirRoot)
	}
	return ""
}

func (h *Hub) latestTicketInterfaceDigest(ctx context.Context, ticketID string) string {
	tasks, err := h.store.ListTasksByTicket(ctx, ticketID)
	if err != nil {
		return ""
	}
	for _, t := range tasks {
		if strings.TrimSpace(t.DigestJSON) == "" {
			continue
		}
		var d protocol.RunDigest
		if err := json.Unmarshal([]byte(t.DigestJSON), &d, protocol.JSONOptions); err != nil {
			continue
		}
		if iface := singleLine(d.Interface); iface != "" {
			return iface
		}
		// Latest task with a digest had no interface; don't fall back to stale attempts.
		return ""
	}
	return ""
}

func buildPacketObjective(title, body string) string {
	t := singleLine(title)
	p := firstParagraph(body)
	if p == "" {
		return t
	}
	if t == "" {
		return p
	}
	if strings.HasPrefix(strings.ToLower(p), strings.ToLower(t)) {
		return p
	}
	last := t[len(t)-1]
	if last == '.' || last == '!' || last == '?' {
		return t + " " + p
	}
	return t + ". " + p
}

func firstParagraph(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	var parts []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		parts = append(parts, trimmed)
	}
	return singleLine(strings.Join(parts, " "))
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func joinWorktreePath(root, ticketSID, repoName string) string {
	sep := "/"
	if strings.Contains(root, `\`) {
		sep = `\`
	}
	root = strings.TrimRight(root, `/\`)
	return root + sep + ticketSID + sep + repoName
}
