// Package tracker syncs external trackers (ka, GitHub Projects, Jira, Linear, ...) into
// Syre's work model. The hub owns live state; a tracker is a sync adapter. Conflicts are
// settled by field ownership (docs/design/work.md#tracker-sync):
//
//   - Human-owned fields (title, body, kind, acceptance text, labels, priority, parent)
//     take the tracker's value on pull.
//   - Syre-owned fields (status while Syre is working the ticket, the lease/claim) take
//     Syre's value; Syre pushes them.
//   - Acceptance "done" flags merge with OR; blocked_by merges as a set union.
//
// Each cycle pulls first, then pushes tickets Syre changed since their last sync.
package tracker

import (
	"context"
	"slices"
	"strings"

	"github.com/BryanSant/syre/internal/store"
)

// Item is a ticket as an external tracker represents it, in Syre's vocabulary (the
// adapter maps statuses, kinds and priorities to and from its own).
type Item struct {
	ExternalID string `json:"external_id"`
	URL        string `json:"url,omitempty"`
	Version    string `json:"version,omitempty"`

	Kind       string                 `json:"kind,omitempty"`
	Title      string                 `json:"title"`
	Body       string                 `json:"body,omitempty"`
	Acceptance []store.AcceptanceItem `json:"acceptance,omitempty"`
	Labels     []string               `json:"labels,omitempty"`
	Priority   string                 `json:"priority,omitempty"`
	Status     string                 `json:"status,omitempty"`

	ParentExternalID string   `json:"parent_external_id,omitempty"`
	BlockedBy        []string `json:"blocked_by,omitempty"` // external IDs

	// Claim is who holds the ticket in the tracker. On push, Syre sends its lease as a
	// claim with Authority "syre" (nil releases a claim Syre held).
	Claim *Claim `json:"claim,omitempty"`
	// Deleted reports an item removed from the tracker (pull only).
	Deleted bool `json:"deleted,omitempty"`
}

// Claim is a tracker-side claim.
type Claim struct {
	By        string `json:"by"`
	Session   string `json:"session,omitempty"`
	Authority string `json:"authority,omitempty"`
}

// PushItem is one ticket Syre sends to a tracker. ExternalID is empty for a ticket the
// tracker hasn't seen yet (create).
type PushItem struct {
	TicketID   string
	ExternalID string
	Item       Item
}

// PushResult reports the outcome of one PushItem.
type PushResult struct {
	TicketID   string
	ExternalID string
	URL        string
	Version    string
	Err        error
}

// Caps describes what a tracker can represent; the syncer skips what it can't.
type Caps struct {
	Hierarchy  bool // parent links (initiative > epic > ticket)
	DAG        bool // blocked_by
	Acceptance bool // acceptance items with done flags
	Claims     bool // claims with an authority
}

// Tracker is the adapter interface every external tracker implements.
type Tracker interface {
	Kind() string
	Capabilities() Caps
	// Pull returns items changed since cursor ("" = everything) and the next cursor.
	Pull(ctx context.Context, cursor string) (items []Item, next string, err error)
	// Push creates or updates items, returning one result per input.
	Push(ctx context.Context, items []PushItem) ([]PushResult, error)
}

// SyreAuthority is the claim authority Syre uses in trackers that support it.
const SyreAuthority = "syre"

// SyreActor is the actor Syre claims as.
const SyreActor = "agent/syre"

// syreActive reports whether Syre is working tk, so its status is Syre-owned.
func syreActive(tk store.Ticket) bool {
	if tk.Lease != nil {
		return true
	}
	switch tk.Status {
	case store.TicketStatusRunning, store.TicketStatusNeedsYou, store.TicketStatusReview:
		return true
	}
	return false
}

// MergePulled applies a pulled item to tk by field ownership and reports whether tk
// changed. Parent and blocked_by are resolved by the syncer (they need ID lookups).
func MergePulled(tk store.Ticket, it Item, caps Caps) (store.Ticket, bool) {
	changed := false
	setStr := func(dst *string, v string) {
		if v != "" && *dst != v {
			*dst, changed = v, true
		}
	}
	setStr(&tk.Title, strings.TrimSpace(it.Title))
	if it.Body != tk.Body && strings.TrimSpace(it.Body) != "" {
		tk.Body, changed = it.Body, true
	}
	if it.Kind != "" && it.Kind != tk.Kind && store.ValidTicketKind(it.Kind) {
		tk.Kind, changed = it.Kind, true
	}
	setStr(&tk.Priority, it.Priority)
	if it.Labels != nil && !slices.Equal(sortedCopy(it.Labels), sortedCopy(tk.Labels)) {
		tk.Labels, changed = append([]string(nil), it.Labels...), true
	}
	if caps.Acceptance && it.Acceptance != nil {
		if merged := mergeAcceptance(tk.Acceptance, it.Acceptance); !slices.Equal(merged, tk.Acceptance) {
			tk.Acceptance, changed = merged, true
		}
	}
	if it.Status != "" && it.Status != tk.Status && !syreActive(tk) && store.ValidTicketStatus(it.Status) {
		tk.Status, changed = it.Status, true
	}
	return tk, changed
}

// mergeAcceptance takes the tracker's items (text is human-owned) and ORs the done flag
// with Syre's flag for the same text.
func mergeAcceptance(syre, theirs []store.AcceptanceItem) []store.AcceptanceItem {
	done := map[string]bool{}
	for _, a := range syre {
		if a.Done {
			done[strings.TrimSpace(a.Text)] = true
		}
	}
	out := make([]store.AcceptanceItem, 0, len(theirs))
	for _, a := range theirs {
		a.Done = a.Done || done[strings.TrimSpace(a.Text)]
		out = append(out, a)
	}
	return out
}

func sortedCopy(l []string) []string {
	c := append([]string(nil), l...)
	slices.Sort(c)
	return c
}

// ItemFromTicket builds the push form of tk. parentExt and blockedExt are the external
// IDs of its parent and blockers that the tracker already knows.
func ItemFromTicket(tk store.Ticket, parentExt string, blockedExt []string) Item {
	it := Item{
		Kind: tk.Kind, Title: tk.Title, Body: tk.Body, Priority: tk.Priority, Status: tk.Status,
		Acceptance:       append([]store.AcceptanceItem(nil), tk.Acceptance...),
		Labels:           append([]string(nil), tk.Labels...),
		ParentExternalID: parentExt, BlockedBy: blockedExt,
	}
	if tk.Lease != nil {
		it.Claim = &Claim{By: SyreActor, Session: tk.Lease.TaskID, Authority: SyreAuthority}
	}
	return it
}
