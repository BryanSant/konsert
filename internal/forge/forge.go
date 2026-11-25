// Package forge talks to code forges (GitHub, GitLab, Forgejo/Gitea) through their HTTP
// APIs, with no CLI dependency. It is the contract between the delivery modes (what
// Approve does with an agent's branch, KON-H50YW) and the per-forge adapters
// (KON-QHGWZ). Plain git work (merge, push, branches) is not here: it runs on the
// daemon that owns the checkout.
package forge

import (
	"context"
	"errors"
	"time"
)

// Kind names a forge implementation.
type Kind string

const (
	GitHub  Kind = "github"
	GitLab  Kind = "gitlab"
	Forgejo Kind = "forgejo" // also Gitea and Codeberg
)

// MergeMethod is how a PR/MR is merged.
type MergeMethod string

const (
	Squash MergeMethod = "squash"
	Merge  MergeMethod = "merge"
	Rebase MergeMethod = "rebase"
)

// Repo identifies a repository on a forge, parsed from a git remote URL.
type Repo struct {
	Kind  Kind
	Host  string // e.g. "github.com", "gitlab.example.com", "codeberg.org"
	Owner string // user, organisation or (GitLab) full group path
	Name  string
	// APIBase is the API root ("https://api.github.com", "https://host/api/v4",
	// "https://host/api/v1"); set by detection.
	APIBase string
}

// FullName is "owner/name".
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// RepoInfo is what delivery needs to know about a repository.
type RepoInfo struct {
	DefaultBranch string
	// AllowedMethods lists the merge methods the repository allows (empty = unknown).
	AllowedMethods []MergeMethod
	// CanPush reports whether the authenticated user can push branches here; when false,
	// delivery pushes to the user's fork (Fork) and opens the PR/MR against this repo.
	CanPush bool
	Fork    *Repo // the user's fork, when it exists
}

// PRState is the lifecycle state of a PR/MR.
type PRState string

const (
	PROpen   PRState = "open"
	PRMerged PRState = "merged"
	PRClosed PRState = "closed"
)

// ChecksState summarises CI checks on a PR/MR head.
type ChecksState string

const (
	ChecksNone    ChecksState = "none"
	ChecksPending ChecksState = "pending"
	ChecksPassed  ChecksState = "passed"
	ChecksFailed  ChecksState = "failed"
)

// PR is a pull request (GitHub, Forgejo) or merge request (GitLab).
type PR struct {
	Number    int // PR number / MR iid
	URL       string
	State     PRState
	Draft     bool
	Head      string // source branch
	Base      string // target branch
	Checks    ChecksState
	AutoMerge bool // the forge will merge it when checks pass
	MergedAt  *time.Time
}

// OpenPRRequest opens a PR/MR. HeadOwner is set when the head branch lives in a fork.
type OpenPRRequest struct {
	Head      string
	HeadOwner string
	Base      string
	Title     string
	Body      string // markdown: the agent's summary and acceptance list
	Draft     bool
}

// MergeRequest merges a PR/MR. When checks are pending and WhenChecksPass is true, the
// adapter enables the forge's auto-merge instead of failing.
type MergeRequest struct {
	Method         MergeMethod
	CommitTitle    string
	CommitMessage  string
	WhenChecksPass bool
}

// MergeResult reports what happened.
type MergeResult struct {
	Merged       bool // merged now
	AutoMergeSet bool // will merge when checks pass
	Method       MergeMethod
	SHA          string
	Message      string // human-readable outcome for the timeline
}

// ReviewComment is review feedback on a PR/MR to hand back to the agent.
type ReviewComment struct {
	ID               string
	Author           string
	Body             string
	Path             string // file, for inline comments
	Line             int
	ChangesRequested bool // part of a "request changes" review
	CreatedAt        time.Time
}

// Client is one authenticated connection to a forge. Implementations must be safe for
// concurrent use and must never log or return the token in errors.
type Client interface {
	Kind() Kind
	RepoInfo(ctx context.Context, r Repo) (RepoInfo, error)
	OpenPR(ctx context.Context, r Repo, req OpenPRRequest) (PR, error)
	// FindPR returns the open PR/MR for head → base, or ErrNotFound.
	FindPR(ctx context.Context, r Repo, head, base string) (PR, error)
	GetPR(ctx context.Context, r Repo, number int) (PR, error)
	MergePR(ctx context.Context, r Repo, number int, req MergeRequest) (MergeResult, error)
	// ReviewComments returns review feedback newer than since.
	ReviewComments(ctx context.Context, r Repo, number int, since time.Time) ([]ReviewComment, error)
	// CompareURL is the web page to open a PR/MR by hand (used when no credential exists).
	CompareURL(r Repo, head, base string) string
}

// Errors adapters return (wrapped with detail).
var (
	ErrNotFound         = errors.New("forge: not found")
	ErrNoCredential     = errors.New("forge: no credential for this host")
	ErrForbidden        = errors.New("forge: permission denied")
	ErrMethodNotAllowed = errors.New("forge: merge method not allowed by the repository")
	ErrNotMergeable     = errors.New("forge: not mergeable (conflicts or failing required checks)")
	ErrUnknownForge     = errors.New("forge: unknown forge")
)

// Credential is a token for one forge host and where it came from.
type Credential struct {
	Host   string
	Token  string
	Source string // "git-credential" | "gh" | "glab" | "device" | "pasted"
}

// Resolver finds forges and credentials and builds clients. The hub owns one.
type Resolver interface {
	// Detect parses a git remote URL (https or ssh) into a Repo, identifying the forge by
	// host and, for unknown hosts, by probing the version endpoints.
	Detect(ctx context.Context, remoteURL string) (Repo, error)
	// Client returns an authenticated client for r's host, or ErrNoCredential.
	Client(ctx context.Context, r Repo) (Client, error)
}
