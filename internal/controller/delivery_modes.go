package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/forge"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

const (
	deliveryModeSetting = "default_delivery_mode"
	mergeStyleSetting   = "default_merge_style"
	deliveryPollEvery   = 60 * time.Second
	gitDeliveryTimeout  = 60 * time.Second
)

// DeliveryPlan describes what Approve will do for a task (GET /api/tasks/{id}/delivery).
type DeliveryPlan struct {
	Mode          string `json:"mode"`
	MergeStyle    string `json:"merge_style"`
	HasRemote     bool   `json:"has_remote"`
	RemoteURL     string `json:"remote_url,omitempty"`
	DefaultBranch string `json:"default_branch"`
	Branch        string `json:"branch,omitempty"`
	CanPush       bool   `json:"can_push"`
	Fork          string `json:"fork,omitempty"`
	HasForge      bool   `json:"has_forge"`
	ApproveLabel  string `json:"approve_label"`
	PRNumber      int    `json:"pr_number,omitzero"`
	PRURL         string `json:"pr_url,omitempty"`
	PRState       string `json:"pr_state,omitempty"`
	PRChecks      string `json:"pr_checks,omitempty"`
	PRAutoMerge   bool   `json:"pr_auto_merge,omitzero"`
	CompareURL    string `json:"compare_url,omitempty"`
}

type resolvedDelivery struct {
	mode          string
	mergeStyle    string
	hasRemote     bool
	remoteURL     string
	remoteName    string
	defaultBranch string
	branch        string
	workdir       string
}

func taskHasGitChanges(t store.Task) bool {
	if strings.TrimSpace(t.GitSummaryJSON) == "" {
		return false
	}
	var g protocol.GitSummary
	if err := json.Unmarshal([]byte(t.GitSummaryJSON), &g, protocol.JSONOptions); err != nil {
		return false
	}
	return len(g.Commits) > 0 || strings.TrimSpace(g.DiffStat) != "" || len(g.DirtyFiles) > 0
}

func (h *Hub) deliveryClock() time.Time {
	if h.deliveryNow != nil {
		return h.deliveryNow().UTC()
	}
	return time.Now().UTC()
}

// resolveDelivery resolves the effective delivery mode, merge style, remote URL, and
// branch names for task t by consulting repo override -> project override -> global
// setting -> automatic default (pr_merge with remote, direct without).
func (h *Hub) resolveDelivery(ctx context.Context, t store.Task, queryWorker bool) resolvedDelivery {
	dir := firstNonEmpty(t.EffectiveDir, t.Workdir)
	out := resolvedDelivery{
		mergeStyle:    store.MergeStyleSquash,
		defaultBranch: "main",
		branch:        strings.TrimSpace(t.Branch),
		workdir:       dir,
	}

	var repo store.Repo
	var hasRepo bool
	if t.RepoID != "" {
		if r, err := h.store.GetRepo(ctx, t.RepoID); err == nil {
			repo, hasRepo = r, true
		}
	}
	if !hasRepo && t.WorkerID != "" && t.Workdir != "" {
		if repos, err := h.store.ListRepos(ctx, "", t.WorkerID); err == nil {
			for _, r := range repos {
				if r.Path == t.Workdir || (t.EffectiveDir != "" && strings.HasPrefix(t.EffectiveDir, r.Path)) {
					repo, hasRepo = r, true
					break
				}
			}
		}
	}

	var proj store.Project
	projID := t.ProjectID
	if projID == "" && hasRepo {
		projID = repo.ProjectID
	}
	if projID != "" {
		if p, err := h.store.GetProject(ctx, projID); err == nil {
			proj = p
		}
	}

	saved, _ := h.store.GetAllSettings(ctx)

	if hasRepo && repo.RemoteURL != "" {
		out.remoteURL = repo.RemoteURL
		out.hasRemote = true
	}

	if queryWorker && dir != "" && t.WorkerID != "" && h.IsWorkerConnected(t.WorkerID) {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		env, err := h.RequestWorker(rctx, t.WorkerID, protocol.MsgGitInfo, protocol.GitInfoRequest{Workdir: dir})
		cancel()
		if err == nil {
			if info, err := protocol.DecodePayload[protocol.GitInfoResult](env); err == nil && info.OK {
				if info.Branch != "" {
					out.branch = info.Branch
				}
				if info.DefaultBranch != "" {
					out.defaultBranch = info.DefaultBranch
				}
				if info.HasRemote && info.RemoteURL != "" {
					out.hasRemote = true
					out.remoteURL = info.RemoteURL
					out.remoteName = info.RemoteName
				}
			}
		}
	}

	// Merge style: repo -> project -> global -> squash.
	out.mergeStyle = firstNonEmpty(repo.MergeStyle, proj.MergeStyle, saved[mergeStyleSetting], store.MergeStyleSquash)

	// Delivery mode: repo -> project -> global (if applicable) -> auto default.
	switch {
	case repo.DeliveryMode != "":
		out.mode = repo.DeliveryMode
	case proj.DeliveryMode != "":
		out.mode = proj.DeliveryMode
	case saved[deliveryModeSetting] != "":
		gm := saved[deliveryModeSetting]
		if out.hasRemote || gm == store.DeliveryModeDirect || gm == store.DeliveryModeKeepBranch {
			out.mode = gm
		} else {
			out.mode = store.DeliveryModeDirect
		}
	default:
		if out.hasRemote {
			out.mode = store.DeliveryModePRMerge
		} else {
			out.mode = store.DeliveryModeDirect
		}
	}
	return out
}

// TaskDeliveryPlan computes the delivery preview shown in the Review panel.
func (h *Hub) TaskDeliveryPlan(ctx context.Context, t store.Task) DeliveryPlan {
	cfg := h.resolveDelivery(ctx, t, true)
	plan := DeliveryPlan{
		Mode:          cfg.mode,
		MergeStyle:    cfg.mergeStyle,
		HasRemote:     cfg.hasRemote,
		RemoteURL:     cfg.remoteURL,
		DefaultBranch: cfg.defaultBranch,
		Branch:        cfg.branch,
		CanPush:       true,
	}

	if rv, err := h.store.GetReview(ctx, t.ID, ""); err == nil {
		plan.PRNumber = rv.PRNumber
		plan.PRURL = rv.PRURL
		plan.PRState = rv.PRState
		plan.PRChecks = rv.PRChecks
		plan.PRAutoMerge = rv.PRAutoMerge
		plan.CompareURL = rv.CompareURL
	}

	if (cfg.mode == store.DeliveryModePRMerge || cfg.mode == store.DeliveryModePROpen) && cfg.hasRemote {
		if resolver := h.forgeResolver(); resolver != nil {
			if fRepo, err := resolver.Detect(ctx, cfg.remoteURL); err == nil {
				if client, err := resolver.Client(ctx, fRepo); err == nil {
					plan.HasForge = true
					if info, err := client.RepoInfo(ctx, fRepo); err == nil {
						if info.DefaultBranch != "" {
							plan.DefaultBranch = info.DefaultBranch
						}
						plan.CanPush = info.CanPush
						if info.Fork != nil {
							plan.Fork = info.Fork.FullName()
						}
					}
				}
			}
		}
		if !plan.HasForge && plan.CompareURL == "" && cfg.branch != "" {
			plan.CompareURL = compareURLFallback(cfg.remoteURL, cfg.branch, cfg.defaultBranch)
		}
	}

	plan.ApproveLabel = FormatApproveLabel(plan)
	return plan
}

// FormatApproveLabel renders the plain-English description of what Approve will do.
func FormatApproveLabel(p DeliveryPlan) string {
	base := firstNonEmpty(p.DefaultBranch, "main")
	style := firstNonEmpty(p.MergeStyle, store.MergeStyleSquash)
	switch p.Mode {
	case store.DeliveryModeDirect:
		verb := mergeStyleVerb(style)
		if p.HasRemote {
			return fmt.Sprintf("Approve → %s into %s and push", verb, base)
		}
		return fmt.Sprintf("Approve → %s into %s (local only)", verb, base)
	case store.DeliveryModePRMerge:
		if p.PRNumber > 0 {
			return fmt.Sprintf("Approve → merge PR #%d (%s) when checks pass", p.PRNumber, style)
		}
		if p.HasForge {
			return fmt.Sprintf("Approve → push branch, open PR and merge (%s) when checks pass", style)
		}
		return "Approve → push branch and show compare URL (no forge credential)"
	case store.DeliveryModePROpen:
		if !p.HasForge {
			return "Approve → push branch and show compare URL (no forge credential)"
		}
		if !p.CanPush {
			if p.Fork != "" {
				return fmt.Sprintf("Approve → push to fork (%s) and open a PR against upstream", p.Fork)
			}
			return "Approve → push to fork and open a PR against upstream"
		}
		return fmt.Sprintf("Approve → push branch and open a PR against %s", base)
	case store.DeliveryModeKeepBranch:
		if p.Branch != "" {
			return fmt.Sprintf("Approve → mark accepted and keep branch %s", p.Branch)
		}
		return "Approve → mark accepted and keep branch"
	default:
		return "Approve"
	}
}

func mergeStyleVerb(style string) string {
	switch style {
	case store.MergeStyleMerge:
		return "merge"
	case store.MergeStyleRebase:
		return "rebase-merge"
	default:
		return "squash-merge"
	}
}

func mergeStylePastTense(style string) string {
	switch style {
	case store.MergeStyleMerge:
		return "Merged"
	case store.MergeStyleRebase:
		return "Rebase-merged"
	default:
		return "Squash-merged"
	}
}

// onTaskEnteredReview is called when a task completes with git changes. In pr_merge mode,
// it pushes the task branch and opens the PR/MR in the background so CI can run before
// the user approves.
func (h *Hub) onTaskEnteredReview(t store.Task) {
	h.goBackground(func(ctx context.Context) {
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		h.prepareReviewPR(pctx, t)
	})
}

// prepareReviewPR pushes the task branch and opens a PR/MR when the repo's mode is pr_merge.
func (h *Hub) prepareReviewPR(ctx context.Context, t store.Task) {
	cfg := h.resolveDelivery(ctx, t, true)
	if cfg.mode != store.DeliveryModePRMerge || !cfg.hasRemote {
		return
	}
	if cur, err := h.store.GetReview(ctx, t.ID, ""); err == nil {
		if cur.PRNumber > 0 || cur.Decision != "" {
			return
		}
	}
	if cfg.workdir == "" || t.WorkerID == "" || !h.IsWorkerConnected(t.WorkerID) {
		return
	}

	prTitle := firstNonEmpty(t.Title, firstLine(t.Prompt), "Task "+t.SID)
	fRepo, client, forgeErr := h.resolveForgeClient(ctx, cfg.remoteURL)
	if client == nil {
		// No forge or no credential: push branch and show CompareURL.
		pushRes, err := h.workerGitPush(ctx, t.WorkerID, protocol.GitPushRequest{
			Workdir:       cfg.workdir,
			Branch:        cfg.branch,
			Remote:        cfg.remoteName,
			CommitMessage: prTitle,
		})
		if err != nil || !pushRes.OK {
			msg := "push failed"
			if err != nil {
				msg = err.Error()
			} else if pushRes.Error != "" {
				msg = pushRes.Error
			}
			_, _ = h.saveReview(ctx, t.ID, "", func(r *store.Review) {
				r.DeliveryMode = store.DeliveryModePRMerge
				r.DeliveryStatus = store.DeliveryStatusFailed
				r.DeliveryMessage = "Could not push branch for review: " + msg
				r.PRRemoteURL = cfg.remoteURL
			})
			h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusFailed, "Could not push branch for review: "+msg, nil)
			return
		}
		branch := firstNonEmpty(pushRes.Branch, cfg.branch)
		cmpURL := compareURLFallback(cfg.remoteURL, branch, cfg.defaultBranch)
		msg := "Branch pushed; open a PR/MR by hand (no forge credential)."
		if forgeErr != nil && !errors.Is(forgeErr, forge.ErrNoCredential) && !errors.Is(forgeErr, forge.ErrUnknownForge) {
			msg = fmt.Sprintf("Branch pushed; open a PR/MR by hand (%v).", forgeErr)
		}
		_, _ = h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePRMerge
			r.DeliveryStatus = store.DeliveryStatusManual
			r.DeliveryMessage = msg
			r.CompareURL = cmpURL
			r.PRRemoteURL = cfg.remoteURL
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusManual, msg, map[string]any{"compare_url": cmpURL})
		return
	}

	pr, err := h.pushAndOpenPR(ctx, t, cfg, fRepo, client, prTitle)
	if err != nil {
		_, _ = h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePRMerge
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = "Could not open PR/MR: " + err.Error()
			r.PRRemoteURL = cfg.remoteURL
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusFailed, "Could not open PR/MR: "+err.Error(), nil)
		return
	}
	msg := fmt.Sprintf("Opened PR #%d for review (%s).", pr.Number, pr.URL)
	_, _ = h.saveReview(ctx, t.ID, "", func(r *store.Review) {
		r.DeliveryMode = store.DeliveryModePRMerge
		r.DeliveryStatus = store.DeliveryStatusPROpen
		r.DeliveryMessage = msg
		r.PRNumber = pr.Number
		r.PRURL = pr.URL
		r.PRState = string(pr.State)
		r.PRChecks = string(pr.Checks)
		r.PRAutoMerge = pr.AutoMerge
		r.PRRemoteURL = cfg.remoteURL
	})
	h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusPROpen, msg, map[string]any{"pr_number": pr.Number, "pr_url": pr.URL})
}

// deliverApproved runs the repo's delivery mode when the user clicks Approve.
func (h *Hub) deliverApproved(ctx context.Context, svc *FleetService, t store.Task, cur store.Review) (ReviewResult, error) {
	var res ReviewResult
	cfg := h.resolveDelivery(ctx, t, true)
	title := firstNonEmpty(t.Title, firstLine(t.Prompt), "Task "+t.SID)

	switch cfg.mode {
	case store.DeliveryModeKeepBranch:
		msg := "Accepted; branch kept."
		if cfg.branch != "" {
			msg = fmt.Sprintf("Accepted; branch %s kept.", cfg.branch)
		}
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModeKeepBranch
			r.DeliveryStatus = store.DeliveryStatusDelivered
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModeKeepBranch, store.DeliveryStatusDelivered, msg, map[string]any{"branch": cfg.branch})
		res.Review = rv
		return res, err

	case store.DeliveryModeDirect:
		return h.deliverDirect(ctx, svc, t, cfg, title)

	case store.DeliveryModePRMerge:
		return h.deliverPRMerge(ctx, t, cur, cfg, title)

	case store.DeliveryModePROpen:
		return h.deliverPROpen(ctx, t, cur, cfg, title)

	default:
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = cfg.mode
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = fmt.Sprintf("Unknown delivery mode %q", cfg.mode)
		})
		res.Review = rv
		return res, err
	}
}

func (h *Hub) deliverDirect(ctx context.Context, svc *FleetService, t store.Task, cfg resolvedDelivery, title string) (ReviewResult, error) {
	var res ReviewResult
	if cfg.workdir == "" || t.WorkerID == "" {
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModeDirect
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = "The task has no checkout to merge."
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModeDirect, store.DeliveryStatusFailed, rv.DeliveryMessage, nil)
		res.Review = rv
		return res, err
	}
	if !h.IsWorkerConnected(t.WorkerID) {
		msg := h.workerLabel(ctx, t.WorkerID) + " is offline; cannot merge right now."
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModeDirect
			r.DeliveryStatus = store.DeliveryStatusOffline
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModeDirect, store.DeliveryStatusOffline, msg, nil)
		res.Review = rv
		return res, err
	}

	mRes, err := h.workerGitMerge(ctx, t.WorkerID, protocol.GitMergeRequest{
		Workdir:       cfg.workdir,
		SourceBranch:  cfg.branch,
		TargetBranch:  cfg.defaultBranch,
		Style:         cfg.mergeStyle,
		Push:          cfg.hasRemote,
		Remote:        cfg.remoteName,
		CommitMessage: title,
		TaskSID:       firstNonEmpty(t.SID, t.ID),
	})
	if err != nil {
		msg := "Asking " + h.workerLabel(ctx, t.WorkerID) + " to merge failed: " + err.Error()
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModeDirect
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModeDirect, store.DeliveryStatusFailed, msg, nil)
		res.Review = rv
		return res, sErr
	}

	if mRes.Conflict {
		conflictNote := fmt.Sprintf(
			"Merge conflict while delivering %s into %s (%s). Conflicting files: %s.\nPlease resolve the conflicts against %s and ensure tests pass.",
			firstNonEmpty(cfg.branch, "your branch"),
			firstNonEmpty(mRes.TargetBranch, cfg.defaultBranch),
			cfg.mergeStyle,
			strings.Join(mRes.ConflictFiles, ", "),
			firstNonEmpty(mRes.TargetBranch, cfg.defaultBranch),
		)
		res.Delivered, res.FollowUpTaskID, res.FollowUpTaskSID, err = h.deliverChanges(ctx, svc, t, conflictNote)
		msg := firstNonEmpty(mRes.Error, "Merge conflict; sent back to the agent to resolve.")
		if err != nil {
			h.logger.Warn("could not deliver conflict note to agent", "task_id", t.ID, "err", err)
			msg += " (Could not notify agent: " + err.Error() + ")"
		}
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.Decision = store.ReviewChangesRequested
			r.Note = conflictNote
			r.DeliveryMode = store.DeliveryModeDirect
			r.DeliveryStatus = store.DeliveryStatusConflict
			r.DeliveryMessage = msg
			r.FollowUpTaskID = res.FollowUpTaskID
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModeDirect, store.DeliveryStatusConflict, msg, map[string]any{
			"conflict_files":    mRes.ConflictFiles,
			"follow_up_task_id": res.FollowUpTaskID,
		})
		res.Review = rv
		return res, sErr
	}

	if !mRes.OK {
		msg := firstNonEmpty(mRes.Error, "Direct merge failed.")
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModeDirect
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModeDirect, store.DeliveryStatusFailed, msg, nil)
		res.Review = rv
		return res, sErr
	}

	target := firstNonEmpty(mRes.TargetBranch, cfg.defaultBranch)
	verb := mergeStylePastTense(cfg.mergeStyle)
	msg := fmt.Sprintf("%s into %s (local only).", verb, target)
	if mRes.Pushed {
		msg = fmt.Sprintf("%s into %s and pushed.", verb, target)
	}
	if mRes.Message != "" {
		msg = mRes.Message
	}
	rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
		r.DeliveryMode = store.DeliveryModeDirect
		r.DeliveryStatus = store.DeliveryStatusDelivered
		r.DeliveryMessage = msg
		r.MergeMethod = cfg.mergeStyle
		r.MergeSHA = mRes.MergeSHA
	})
	meta := map[string]any{
		"target_branch": target,
		"merge_method":  cfg.mergeStyle,
		"merge_sha":     mRes.MergeSHA,
		"pushed":        mRes.Pushed,
	}
	if mRes.MergedBranch != "" {
		meta["merged_branch"] = mRes.MergedBranch
	}
	if mRes.LocalBehind {
		meta["local_behind"] = true
	}
	h.recordDeliveryOutcome(ctx, t, store.DeliveryModeDirect, store.DeliveryStatusDelivered, msg, meta)
	res.Review = rv
	return res, sErr
}

func (h *Hub) deliverPRMerge(ctx context.Context, t store.Task, cur store.Review, cfg resolvedDelivery, title string) (ReviewResult, error) {
	var res ReviewResult
	remoteURL := firstNonEmpty(cur.PRRemoteURL, cfg.remoteURL)
	if remoteURL == "" {
		msg := "No git remote configured; cannot open or merge a PR/MR."
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePRMerge
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusFailed, msg, nil)
		res.Review = rv
		return res, err
	}

	fRepo, client, _ := h.resolveForgeClient(ctx, remoteURL)
	if client == nil {
		// No forge / no credential: push branch (if connected) and show CompareURL.
		branch := cfg.branch
		if cfg.workdir != "" && t.WorkerID != "" && h.IsWorkerConnected(t.WorkerID) {
			pushRes, err := h.workerGitPush(ctx, t.WorkerID, protocol.GitPushRequest{
				Workdir:       cfg.workdir,
				Branch:        cfg.branch,
				Remote:        cfg.remoteName,
				CommitMessage: title,
			})
			if err != nil || !pushRes.OK {
				msg := "Push failed"
				if err != nil {
					msg += ": " + err.Error()
				} else if pushRes.Error != "" {
					msg += ": " + pushRes.Error
				}
				rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
					r.DeliveryMode = store.DeliveryModePRMerge
					r.DeliveryStatus = store.DeliveryStatusFailed
					r.DeliveryMessage = msg
					r.PRRemoteURL = remoteURL
				})
				h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusFailed, msg, nil)
				res.Review = rv
				return res, sErr
			}
			branch = firstNonEmpty(pushRes.Branch, branch)
		} else if cur.CompareURL == "" {
			msg := h.workerLabel(ctx, t.WorkerID) + " is offline; cannot push branch."
			rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
				r.DeliveryMode = store.DeliveryModePRMerge
				r.DeliveryStatus = store.DeliveryStatusOffline
				r.DeliveryMessage = msg
			})
			h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusOffline, msg, nil)
			res.Review = rv
			return res, sErr
		}
		cmpURL := firstNonEmpty(cur.CompareURL, compareURLFallback(remoteURL, branch, cfg.defaultBranch))
		msg := "Branch pushed; open and merge the PR/MR by hand (no forge credential)."
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePRMerge
			r.DeliveryStatus = store.DeliveryStatusManual
			r.DeliveryMessage = msg
			r.CompareURL = cmpURL
			r.PRRemoteURL = remoteURL
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusManual, msg, map[string]any{"compare_url": cmpURL})
		res.Review = rv
		return res, sErr
	}

	pr := forge.PR{
		Number:    cur.PRNumber,
		URL:       cur.PRURL,
		State:     forge.PRState(cur.PRState),
		Checks:    forge.ChecksState(cur.PRChecks),
		AutoMerge: cur.PRAutoMerge,
	}
	if pr.Number == 0 {
		if cfg.workdir == "" || t.WorkerID == "" || !h.IsWorkerConnected(t.WorkerID) {
			msg := h.workerLabel(ctx, t.WorkerID) + " is offline; cannot push branch to open PR."
			rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
				r.DeliveryMode = store.DeliveryModePRMerge
				r.DeliveryStatus = store.DeliveryStatusOffline
				r.DeliveryMessage = msg
			})
			h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusOffline, msg, nil)
			res.Review = rv
			return res, sErr
		}
		opened, err := h.pushAndOpenPR(ctx, t, cfg, fRepo, client, title)
		if err != nil {
			msg := "Could not open PR/MR: " + err.Error()
			rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
				r.DeliveryMode = store.DeliveryModePRMerge
				r.DeliveryStatus = store.DeliveryStatusFailed
				r.DeliveryMessage = msg
				r.PRRemoteURL = remoteURL
			})
			h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusFailed, msg, nil)
			res.Review = rv
			return res, sErr
		}
		pr = opened
	}

	mergeRes, fallbackNote, err := mergePRWithFallback(ctx, client, fRepo, pr.Number, forge.MergeMethod(cfg.mergeStyle), title)
	if err != nil {
		msg := fmt.Sprintf("PR #%d could not be merged: %v", pr.Number, err)
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePRMerge
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = msg
			r.PRNumber = pr.Number
			r.PRURL = pr.URL
			r.PRState = string(firstPRState(pr.State, forge.PROpen))
			r.PRChecks = string(pr.Checks)
			r.PRRemoteURL = remoteURL
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, store.DeliveryStatusFailed, msg, map[string]any{"pr_number": pr.Number, "pr_url": pr.URL})
		res.Review = rv
		return res, sErr
	}

	status := store.DeliveryStatusDelivered
	prState := forge.PRMerged
	msg := mergeRes.Message
	if mergeRes.Merged {
		if msg == "" {
			msg = fmt.Sprintf("Merged PR #%d (%s).", pr.Number, firstNonEmpty(string(mergeRes.Method), cfg.mergeStyle))
		}
	} else if mergeRes.AutoMergeSet {
		status = store.DeliveryStatusAutoMerge
		prState = forge.PROpen
		if msg == "" {
			msg = fmt.Sprintf("Auto-merge enabled on PR #%d (%s) when checks pass.", pr.Number, firstNonEmpty(string(mergeRes.Method), cfg.mergeStyle))
		}
	}
	if fallbackNote != "" {
		msg = strings.TrimSpace(msg + " " + fallbackNote)
	}

	rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
		r.DeliveryMode = store.DeliveryModePRMerge
		r.DeliveryStatus = status
		r.DeliveryMessage = msg
		r.MergeMethod = firstNonEmpty(string(mergeRes.Method), cfg.mergeStyle)
		r.MergeSHA = mergeRes.SHA
		r.PRNumber = pr.Number
		r.PRURL = pr.URL
		r.PRState = string(prState)
		r.PRChecks = string(pr.Checks)
		r.PRAutoMerge = mergeRes.AutoMergeSet
		r.PRRemoteURL = remoteURL
	})
	h.recordDeliveryOutcome(ctx, t, store.DeliveryModePRMerge, status, msg, map[string]any{
		"pr_number":    pr.Number,
		"pr_url":       pr.URL,
		"merge_method": rv.MergeMethod,
		"merge_sha":    mergeRes.SHA,
		"auto_merge":   mergeRes.AutoMergeSet,
	})
	res.Review = rv
	return res, sErr
}

func (h *Hub) deliverPROpen(ctx context.Context, t store.Task, cur store.Review, cfg resolvedDelivery, title string) (ReviewResult, error) {
	var res ReviewResult
	remoteURL := firstNonEmpty(cur.PRRemoteURL, cfg.remoteURL)
	if remoteURL == "" {
		msg := "No git remote configured; cannot open a PR/MR."
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePROpen
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePROpen, store.DeliveryStatusFailed, msg, nil)
		res.Review = rv
		return res, err
	}
	if cfg.workdir == "" || t.WorkerID == "" || !h.IsWorkerConnected(t.WorkerID) {
		msg := h.workerLabel(ctx, t.WorkerID) + " is offline; cannot push branch."
		rv, err := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePROpen
			r.DeliveryStatus = store.DeliveryStatusOffline
			r.DeliveryMessage = msg
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePROpen, store.DeliveryStatusOffline, msg, nil)
		res.Review = rv
		return res, err
	}

	fRepo, client, _ := h.resolveForgeClient(ctx, remoteURL)
	if client == nil {
		pushRes, err := h.workerGitPush(ctx, t.WorkerID, protocol.GitPushRequest{
			Workdir:       cfg.workdir,
			Branch:        cfg.branch,
			Remote:        cfg.remoteName,
			CommitMessage: title,
		})
		if err != nil || !pushRes.OK {
			msg := "Push failed"
			if err != nil {
				msg += ": " + err.Error()
			} else if pushRes.Error != "" {
				msg += ": " + pushRes.Error
			}
			rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
				r.DeliveryMode = store.DeliveryModePROpen
				r.DeliveryStatus = store.DeliveryStatusFailed
				r.DeliveryMessage = msg
				r.PRRemoteURL = remoteURL
			})
			h.recordDeliveryOutcome(ctx, t, store.DeliveryModePROpen, store.DeliveryStatusFailed, msg, nil)
			res.Review = rv
			return res, sErr
		}
		branch := firstNonEmpty(pushRes.Branch, cfg.branch)
		cmpURL := compareURLFallback(remoteURL, branch, cfg.defaultBranch)
		msg := "Branch pushed; open a PR/MR by hand (no forge credential)."
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePROpen
			r.DeliveryStatus = store.DeliveryStatusManual
			r.DeliveryMessage = msg
			r.CompareURL = cmpURL
			r.PRRemoteURL = remoteURL
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePROpen, store.DeliveryStatusManual, msg, map[string]any{"compare_url": cmpURL})
		res.Review = rv
		return res, sErr
	}

	pr, err := h.pushAndOpenPR(ctx, t, cfg, fRepo, client, title)
	if err != nil {
		msg := "Could not open PR/MR: " + err.Error()
		rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
			r.DeliveryMode = store.DeliveryModePROpen
			r.DeliveryStatus = store.DeliveryStatusFailed
			r.DeliveryMessage = msg
			r.PRRemoteURL = remoteURL
		})
		h.recordDeliveryOutcome(ctx, t, store.DeliveryModePROpen, store.DeliveryStatusFailed, msg, nil)
		res.Review = rv
		return res, sErr
	}

	msg := fmt.Sprintf("Delivered: PR #%d open (%s).", pr.Number, pr.URL)
	rv, sErr := h.saveReview(ctx, t.ID, "", func(r *store.Review) {
		r.DeliveryMode = store.DeliveryModePROpen
		r.DeliveryStatus = store.DeliveryStatusPROpen
		r.DeliveryMessage = msg
		r.PRNumber = pr.Number
		r.PRURL = pr.URL
		r.PRState = string(firstPRState(pr.State, forge.PROpen))
		r.PRChecks = string(pr.Checks)
		r.PRAutoMerge = pr.AutoMerge
		r.PRRemoteURL = remoteURL
	})
	h.recordDeliveryOutcome(ctx, t, store.DeliveryModePROpen, store.DeliveryStatusPROpen, msg, map[string]any{
		"pr_number": pr.Number,
		"pr_url":    pr.URL,
	})
	res.Review = rv
	return res, sErr
}

// pushAndOpenPR pushes the task branch (to the user's fork when RepoInfo.CanPush is false)
// and finds or opens a PR/MR on the forge.
func (h *Hub) pushAndOpenPR(ctx context.Context, t store.Task, cfg resolvedDelivery, fRepo forge.Repo, client forge.Client, title string) (forge.PR, error) {
	info, infoErr := client.RepoInfo(ctx, fRepo)
	base := cfg.defaultBranch
	if infoErr == nil && info.DefaultBranch != "" {
		base = info.DefaultBranch
	}

	pushReq := protocol.GitPushRequest{
		Workdir:       cfg.workdir,
		Branch:        cfg.branch,
		Remote:        cfg.remoteName,
		CommitMessage: title,
	}
	var headOwner string
	if infoErr == nil && !info.CanPush {
		if info.Fork == nil {
			return forge.PR{}, fmt.Errorf("cannot push to %s and no fork is available", fRepo.FullName())
		}
		pushReq.RemoteURL = forkRemoteURL(cfg.remoteURL, *info.Fork)
		headOwner = info.Fork.Owner
	}

	pushRes, err := h.workerGitPush(ctx, t.WorkerID, pushReq)
	if err != nil {
		return forge.PR{}, fmt.Errorf("push branch: %w", err)
	}
	if !pushRes.OK {
		return forge.PR{}, fmt.Errorf("push branch: %s", firstNonEmpty(pushRes.Error, "git push failed"))
	}
	branch := firstNonEmpty(pushRes.Branch, cfg.branch)

	findHead := branch
	if headOwner != "" {
		findHead = headOwner + ":" + branch
	}
	if existing, err := client.FindPR(ctx, fRepo, findHead, base); err == nil && existing.Number > 0 {
		return existing, nil
	} else if err != nil && !errors.Is(err, forge.ErrNotFound) {
		return forge.PR{}, err
	}

	body := h.buildPRBody(ctx, t)
	return client.OpenPR(ctx, fRepo, forge.OpenPRRequest{
		Head:      branch,
		HeadOwner: headOwner,
		Base:      base,
		Title:     title,
		Body:      body,
	})
}

func (h *Hub) buildPRBody(ctx context.Context, t store.Task) string {
	var b strings.Builder
	if s := strings.TrimSpace(t.Summary); s != "" {
		b.WriteString("## Summary\n\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	acc := h.TaskAcceptance(ctx, t)
	if len(acc.Items) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## Acceptance\n\n")
		for _, it := range acc.Items {
			box := "[ ]"
			if it.Done {
				box = "[x]"
			}
			fmt.Fprintf(&b, "- %s %s\n", box, it.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// mergePRWithFallback merges prNumber with preferred method and WhenChecksPass=true.
// If the repo disallows the preferred method (either via RepoInfo.AllowedMethods or
// ErrMethodNotAllowed), it falls back to an allowed method and returns a note explaining it.
func mergePRWithFallback(ctx context.Context, client forge.Client, fRepo forge.Repo, prNumber int, preferred forge.MergeMethod, title string) (forge.MergeResult, string, error) {
	if preferred == "" {
		preferred = forge.Squash
	}
	method := preferred
	var fallbackNote string

	var allowed []forge.MergeMethod
	if info, err := client.RepoInfo(ctx, fRepo); err == nil && len(info.AllowedMethods) > 0 {
		allowed = info.AllowedMethods
		if !slices.Contains(allowed, method) {
			method = allowed[0]
			fallbackNote = fmt.Sprintf("(used %s because %s is not allowed by this repository)", method, preferred)
		}
	}

	res, err := client.MergePR(ctx, fRepo, prNumber, forge.MergeRequest{
		Method:         method,
		CommitTitle:    title,
		WhenChecksPass: true,
	})
	if !errors.Is(err, forge.ErrMethodNotAllowed) {
		return res, fallbackNote, err
	}

	// Retry with alternative allowed methods (or standard fallback order).
	candidates := allowed
	if len(candidates) == 0 {
		candidates = []forge.MergeMethod{forge.Squash, forge.Merge, forge.Rebase}
	}
	for _, cand := range candidates {
		if cand == method {
			continue
		}
		res2, err2 := client.MergePR(ctx, fRepo, prNumber, forge.MergeRequest{
			Method:         cand,
			CommitTitle:    title,
			WhenChecksPass: true,
		})
		if err2 == nil {
			note := fmt.Sprintf("(used %s because %s is not allowed by this repository)", cand, preferred)
			return res2, note, nil
		}
		if !errors.Is(err2, forge.ErrMethodNotAllowed) {
			return res2, "", err2
		}
	}
	return res, "", err
}

func (h *Hub) resolveForgeClient(ctx context.Context, remoteURL string) (forge.Repo, forge.Client, error) {
	resolver := h.forgeResolver()
	if resolver == nil || strings.TrimSpace(remoteURL) == "" {
		return forge.Repo{}, nil, forge.ErrNoCredential
	}
	fRepo, err := resolver.Detect(ctx, remoteURL)
	if err != nil {
		return forge.Repo{}, nil, err
	}
	client, err := resolver.Client(ctx, fRepo)
	if err != nil {
		return fRepo, nil, err
	}
	return fRepo, client, nil
}

func (h *Hub) workerGitPush(ctx context.Context, workerID string, req protocol.GitPushRequest) (protocol.GitPushResult, error) {
	rctx, cancel := context.WithTimeout(ctx, gitDeliveryTimeout)
	defer cancel()
	env, err := h.RequestWorker(rctx, workerID, protocol.MsgGitPush, req)
	if err != nil {
		return protocol.GitPushResult{}, err
	}
	return protocol.DecodePayload[protocol.GitPushResult](env)
}

func (h *Hub) workerGitMerge(ctx context.Context, workerID string, req protocol.GitMergeRequest) (protocol.GitMergeResult, error) {
	rctx, cancel := context.WithTimeout(ctx, gitDeliveryTimeout)
	defer cancel()
	env, err := h.RequestWorker(rctx, workerID, protocol.MsgGitMerge, req)
	if err != nil {
		return protocol.GitMergeResult{}, err
	}
	return protocol.DecodePayload[protocol.GitMergeResult](env)
}

func (h *Hub) recordDeliveryOutcome(ctx context.Context, t store.Task, mode, status, msg string, extra map[string]any) {
	h.appendTaskEvent(ctx, t.ID, "delivery", msg)
	meta := map[string]any{"mode": mode, "status": status}
	for k, v := range extra {
		meta[k] = v
	}
	h.recordAudit(ctx, store.AuditEvent{
		Action:   AuditTaskDeliver,
		WorkerID: t.WorkerID,
		TaskID:   t.ID,
		Message:  msg,
		MetaJSON: auditMeta(meta),
	})
}

// --- Periodic PR state tracking & review comment feedback -----------------------------

func (h *Hub) deliveryPollLoop(ctx context.Context) {
	every := h.DeliveryPollEvery
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			h.PollOpenPRs(pctx)
			cancel()
		}
	}
}

// PollOpenPRs checks every review with an open PR/MR: updates its state/checks (until
// merged or closed) and delivers any new change-request comments to the agent, deduped
// persistently by comment ID.
func (h *Hub) PollOpenPRs(ctx context.Context) {
	resolver := h.forgeResolver()
	if resolver == nil {
		return
	}
	reviews, err := h.store.ListReviews(ctx)
	if err != nil {
		return
	}
	for _, r := range reviews {
		if r.TaskID == "" || r.PRNumber <= 0 || r.PRRemoteURL == "" {
			continue
		}
		if r.PRState != "" && r.PRState != string(forge.PROpen) {
			continue
		}
		h.pollOnePR(ctx, resolver, r)
	}
}

func (h *Hub) pollOnePR(ctx context.Context, resolver forge.Resolver, r store.Review) {
	fRepo, err := resolver.Detect(ctx, r.PRRemoteURL)
	if err != nil {
		return
	}
	client, err := resolver.Client(ctx, fRepo)
	if err != nil {
		return
	}
	pr, err := client.GetPR(ctx, fRepo, r.PRNumber)
	if err != nil {
		return
	}

	task, taskErr := h.store.GetTask(ctx, r.TaskID)

	stateChanged := string(pr.State) != r.PRState || string(pr.Checks) != r.PRChecks || pr.AutoMerge != r.PRAutoMerge
	if stateChanged {
		var status, msg string
		switch pr.State {
		case forge.PRMerged:
			status = store.DeliveryStatusDelivered
			msg = fmt.Sprintf("PR #%d merged.", pr.Number)
		case forge.PRClosed:
			status = store.DeliveryStatusClosed
			msg = fmt.Sprintf("PR #%d closed without merging.", pr.Number)
		}
		_, _ = h.saveReview(ctx, r.TaskID, "", func(rv *store.Review) {
			rv.PRState = string(pr.State)
			rv.PRChecks = string(pr.Checks)
			rv.PRAutoMerge = pr.AutoMerge
			if pr.URL != "" {
				rv.PRURL = pr.URL
			}
			if status != "" {
				rv.DeliveryStatus = status
				rv.DeliveryMessage = msg
			}
		})
		if msg != "" && taskErr == nil {
			h.recordDeliveryOutcome(ctx, task, r.DeliveryMode, status, msg, map[string]any{"pr_number": pr.Number, "pr_url": pr.URL})
		}
	}

	if pr.State != forge.PROpen || taskErr != nil {
		return
	}

	var since time.Time
	if r.PRLastCommentAt != nil {
		since = *r.PRLastCommentAt
	}
	comments, err := client.ReviewComments(ctx, fRepo, r.PRNumber, since)
	if err != nil || len(comments) == 0 {
		return
	}

	now := h.deliveryClock()
	var fresh []forge.ReviewComment
	var latest time.Time
	for _, c := range comments {
		if !c.ChangesRequested || strings.TrimSpace(c.Body) == "" || c.ID == "" {
			continue
		}
		inserted, err := h.store.MarkReviewCommentSeen(ctx, r.TaskID, c.ID, now)
		if err != nil || !inserted {
			continue
		}
		fresh = append(fresh, c)
		if c.CreatedAt.After(latest) {
			latest = c.CreatedAt
		}
	}
	if len(fresh) == 0 {
		return
	}

	note := formatPRReviewComments(r.PRNumber, fresh)
	svc := h.deliveryService()
	_, followID, _, delErr := h.deliverChanges(ctx, svc, task, note)
	if delErr != nil {
		h.logger.Warn("could not deliver PR review comments to agent", "task_id", task.ID, "pr", r.PRNumber, "err", delErr)
	}
	h.appendTaskEvent(ctx, task.ID, "review", "PR review changes requested: "+oneLine(note, 200))
	_, _ = h.saveReview(ctx, r.TaskID, "", func(rv *store.Review) {
		rv.Decision = store.ReviewChangesRequested
		rv.Note = note
		if followID != "" {
			rv.FollowUpTaskID = followID
		}
		if !latest.IsZero() {
			t := latest.UTC()
			rv.PRLastCommentAt = &t
		}
	})
}

func formatPRReviewComments(prNumber int, comments []forge.ReviewComment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review comments on PR #%d requesting changes:\n", prNumber)
	for _, c := range comments {
		loc := ""
		if c.Path != "" {
			loc = c.Path
			if c.Line > 0 {
				loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
			}
			loc = " (" + loc + ")"
		}
		author := firstNonEmpty(c.Author, "reviewer")
		fmt.Fprintf(&b, "- %s%s: %s\n", author, loc, strings.TrimSpace(c.Body))
	}
	return strings.TrimSpace(b.String())
}

// --- URL helpers ----------------------------------------------------------------------

func firstPRState(s, fallback forge.PRState) forge.PRState {
	if s != "" {
		return s
	}
	return fallback
}

// forkRemoteURL builds a pushable remote URL for fork matching the scheme (SSH vs HTTPS)
// of upstreamURL.
func forkRemoteURL(upstreamURL string, fork forge.Repo) string {
	u := strings.TrimSpace(upstreamURL)
	if strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "ssh://") {
		return fmt.Sprintf("git@%s:%s/%s.git", fork.Host, fork.Owner, fork.Name)
	}
	return fmt.Sprintf("https://%s/%s/%s.git", fork.Host, fork.Owner, fork.Name)
}

// compareURLFallback builds a best-effort web compare URL from a git remote URL when no
// forge Client is available.
func compareURLFallback(remoteURL, head, base string) string {
	host, path := parseRemoteHostPath(remoteURL)
	if host == "" || path == "" || head == "" {
		return ""
	}
	if base == "" {
		base = "main"
	}
	if strings.Contains(strings.ToLower(host), "gitlab") {
		return fmt.Sprintf("https://%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s",
			host, path, url.QueryEscape(head), url.QueryEscape(base))
	}
	if strings.EqualFold(host, "github.com") {
		return fmt.Sprintf("https://%s/%s/compare/%s...%s?expand=1",
			host, path, escapeBranchPath(base), escapeBranchPath(head))
	}
	return fmt.Sprintf("https://%s/%s/compare/%s...%s",
		host, path, escapeBranchPath(base), escapeBranchPath(head))
}

func escapeBranchPath(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func parseRemoteHostPath(remoteURL string) (host, repoPath string) {
	u := strings.TrimSpace(remoteURL)
	if u == "" {
		return "", ""
	}
	// SCP-like SSH: git@github.com:owner/repo.git
	if !strings.Contains(u, "://") {
		if at := strings.IndexByte(u, '@'); at >= 0 {
			u = u[at+1:]
		}
		colon := strings.IndexByte(u, ':')
		if colon <= 0 {
			return "", ""
		}
		host = u[:colon]
		repoPath = strings.TrimSuffix(strings.Trim(u[colon+1:], "/"), ".git")
		return host, repoPath
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return "", ""
	}
	if parsed.Scheme == "file" {
		return "", ""
	}
	host = parsed.Host
	repoPath = strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	return host, repoPath
}

// GET /api/tasks/{id}/delivery
func (s *Server) handleTaskDeliveryPlan(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.hub.TaskDeliveryPlan(r.Context(), t))
}
