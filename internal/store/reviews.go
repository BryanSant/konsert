package store

import "time"

// RepoPR is one repository's PR/MR in a multi-repo delivery.
type RepoPR struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	URL       string `json:"url,omitempty"`
	RemoteURL string `json:"remote_url,omitempty"`
	State     string `json:"state,omitempty"`
	Checks    string `json:"checks,omitempty"`
}

// Review decisions (Review.Decision).
const (
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes_requested"
)

// Delivery modes (SYR-H50YW): what Approve does with an agent's branch.
// Empty string on a project/repo/global setting means "inherit / auto default".
const (
	DeliveryModeDirect     = "direct"
	DeliveryModePRMerge    = "pr_merge"
	DeliveryModePROpen     = "pr_open"
	DeliveryModeKeepBranch = "keep_branch"
)

// Merge styles (SYR-H50YW). Empty string means "inherit / default (squash)".
const (
	MergeStyleSquash = "squash"
	MergeStyleMerge  = "merge"
	MergeStyleRebase = "rebase"
)

// Delivery statuses (Review.DeliveryStatus).
const (
	DeliveryStatusDelivered = "delivered"  // merged (direct or PR) or keep_branch accepted
	DeliveryStatusPROpen    = "pr_open"    // PR/MR is open (pre-approval in pr_merge, or delivered in pr_open)
	DeliveryStatusAutoMerge = "auto_merge" // PR/MR has auto-merge enabled when checks pass
	DeliveryStatusManual    = "manual"     // branch pushed; no forge credential, CompareURL shown
	DeliveryStatusConflict  = "conflict"   // direct merge conflicted; sent back to the agent
	DeliveryStatusClosed    = "closed"     // tracked PR/MR was closed without merging
	DeliveryStatusFailed    = "failed"     // delivery failed; DeliveryMessage explains why
	DeliveryStatusOffline   = "offline"    // owning machine was offline
)

// ValidDeliveryMode reports whether m is empty (inherit/auto) or a known delivery mode.
func ValidDeliveryMode(m string) bool {
	switch m {
	case "", DeliveryModeDirect, DeliveryModePRMerge, DeliveryModePROpen, DeliveryModeKeepBranch:
		return true
	}
	return false
}

// ValidMergeStyle reports whether s is empty (inherit/default) or a known merge style.
func ValidMergeStyle(s string) bool {
	switch s {
	case "", MergeStyleSquash, MergeStyleMerge, MergeStyleRebase:
		return true
	}
	return false
}

// Review is the user's review decision on finished work, its delivery outcome, PR/MR
// tracking state, and the Expedite flag: one per session (TaskID or ChatID), shared by
// every client.
type Review struct {
	ID     string `json:"id"`
	SID    string `json:"sid"`
	TaskID string `json:"task_id,omitempty"`
	ChatID string `json:"chat_id,omitempty"`
	// Decision is "" (not decided), approved or changes_requested; Note is the change
	// request (or the approval comment).
	Decision  string     `json:"decision,omitempty"`
	Note      string     `json:"note,omitempty"`
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitzero"`
	// Delivery outcome and PR/MR tracking (SYR-H50YW).
	DeliveryMode    string `json:"delivery_mode,omitempty"`
	DeliveryStatus  string `json:"delivery_status,omitempty"`
	DeliveryMessage string `json:"delivery_message,omitempty"`
	MergeMethod     string `json:"merge_method,omitempty"`
	MergeSHA        string `json:"merge_sha,omitempty"`
	PRNumber        int    `json:"pr_number,omitzero"`
	PRURL           string `json:"pr_url,omitempty"`
	PRState         string `json:"pr_state,omitempty"`
	PRChecks        string `json:"pr_checks,omitempty"`
	PRAutoMerge     bool   `json:"pr_auto_merge,omitzero"`
	CompareURL      string `json:"compare_url,omitempty"`
	PRRemoteURL     string `json:"pr_remote_url,omitempty"`
	// PRBase is the PR's base branch: the default branch, or a blocker's branch while
	// the ticket is stacked on an unmerged PR (retargeted when that PR merges).
	PRBase          string     `json:"pr_base,omitempty"`
	PRLastCommentAt *time.Time `json:"pr_last_comment_at,omitzero"`
	// RepoPRs tracks the PRs/MRs of a multi-repo ticket's other repos (the PR* fields
	// above hold the first repo's).
	RepoPRs []RepoPR `json:"repo_prs,omitempty"`
	// FollowUpTaskID is the session that received requested changes.
	FollowUpTaskID string     `json:"follow_up_task_id,omitempty"`
	Expedite       bool       `json:"expedite"`
	ExpeditedAt    *time.Time `json:"expedited_at,omitzero"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
