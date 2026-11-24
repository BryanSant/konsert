package store

import "time"

// Review decisions (Review.Decision).
const (
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes_requested"
)

// Review is the user's review decision on a finished task and the Expedite flag of a task
// or chat: one per session (TaskID or ChatID), shared by every client.
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
	// GitHub* say what became of the decision on the task's pull request (gh):
	// protocol.PRReview* status ("" = not tried), gh's message, the PR URL.
	GitHubStatus  string `json:"github_status,omitempty"`
	GitHubMessage string `json:"github_message,omitempty"`
	GitHubURL     string `json:"github_url,omitempty"`
	// FollowUpTaskID is the session that received requested changes.
	FollowUpTaskID string     `json:"follow_up_task_id,omitempty"`
	Expedite       bool       `json:"expedite"`
	ExpeditedAt    *time.Time `json:"expedited_at,omitzero"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
