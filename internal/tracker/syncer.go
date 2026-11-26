package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// deletedVersion marks the ref of an item the tracker deleted (a tombstone).
const deletedVersion = "deleted"

// Factory builds the adapter for a tracker row (its Kind and ConfigJSON).
type Factory func(ctx context.Context, t store.Tracker) (Tracker, error)

// Syncer runs pull-then-push cycles for every enabled tracker.
type Syncer struct {
	Store     store.Store
	Factories map[string]Factory
	Logger    *slog.Logger
	// Interval between cycles (default 30s). Kick runs a cycle sooner.
	Interval time.Duration
	// OnTicket is called for every ticket the syncer creates or changes (broadcasts).
	OnTicket func(store.Ticket)
	// Now is the clock (tests).
	Now func() time.Time

	mu       sync.Mutex
	adapters map[string]Tracker
	kick     chan struct{}
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Kick asks for a cycle soon (e.g. after Syre changed a ticket).
func (s *Syncer) Kick() {
	s.mu.Lock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	ch := s.kick
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run cycles until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.mu.Lock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	kick := s.kick
	s.mu.Unlock()
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
			// Debounce bursts of ticket changes into one cycle.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		s.SyncAll(ctx)
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(interval)
	}
}

// SyncAll runs one cycle for every enabled tracker.
func (s *Syncer) SyncAll(ctx context.Context) {
	trackers, err := s.Store.ListTrackers(ctx, "")
	if err != nil {
		s.logger().Warn("list trackers failed", "err", err)
		return
	}
	for _, t := range trackers {
		if !t.Enabled || ctx.Err() != nil {
			continue
		}
		if err := s.SyncOne(ctx, t); err != nil {
			s.logger().Warn("tracker sync failed", "tracker", t.ID, "kind", t.Kind, "err", err)
		}
	}
}

func (s *Syncer) adapter(ctx context.Context, t store.Tracker) (Tracker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adapters == nil {
		s.adapters = map[string]Tracker{}
	}
	if a, ok := s.adapters[t.ID]; ok {
		return a, nil
	}
	f, ok := s.Factories[t.Kind]
	if !ok {
		return nil, fmt.Errorf("no adapter for tracker kind %q", t.Kind)
	}
	a, err := f(ctx, t)
	if err != nil {
		return nil, err
	}
	s.adapters[t.ID] = a
	return a, nil
}

// Forget drops a cached adapter (after its tracker row changed or was deleted).
func (s *Syncer) Forget(trackerID string) {
	s.mu.Lock()
	delete(s.adapters, trackerID)
	s.mu.Unlock()
}

// SyncOne pulls then pushes one tracker and records the outcome on its row.
func (s *Syncer) SyncOne(ctx context.Context, t store.Tracker) error {
	a, err := s.adapter(ctx, t)
	if err == nil {
		err = s.pull(ctx, &t, a)
	}
	if err == nil {
		err = s.push(ctx, &t, a)
	}
	t.LastError = ""
	if err != nil {
		t.LastError = err.Error()
	}
	if uerr := s.Store.UpdateTracker(ctx, t); uerr != nil && err == nil {
		err = uerr
	}
	return err
}

func (s *Syncer) pull(ctx context.Context, t *store.Tracker, a Tracker) error {
	items, next, err := a.Pull(ctx, t.Cursor)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	caps := a.Capabilities()
	byExt := map[string]store.Ticket{}
	for _, it := range items {
		tk, err := s.applyPulled(ctx, t, it, caps)
		if err != nil {
			s.logger().Warn("apply pulled item failed", "tracker", t.ID, "external_id", it.ExternalID, "err", err)
			continue
		}
		if tk.ID != "" {
			byExt[it.ExternalID] = tk
		}
	}
	// Second pass: parent and blocked_by reference other items by external ID.
	for _, it := range items {
		tk, ok := byExt[it.ExternalID]
		if !ok || it.Deleted {
			continue
		}
		if caps.Hierarchy && it.ParentExternalID != "" {
			if parent, err := s.lookup(ctx, t.ID, it.ParentExternalID, byExt); err == nil && parent.ID != tk.ParentID {
				tk.ParentID = parent.ID
				if updated, err := s.Store.UpdateTicket(ctx, tk); err == nil {
					s.markSynced(ctx, t.ID, it, updated)
					s.emit(updated)
				}
			}
		}
		if caps.DAG {
			for _, ext := range it.BlockedBy {
				if blk, err := s.lookup(ctx, t.ID, ext, byExt); err == nil {
					if err := s.Store.AddTicketEdge(ctx, tk.ID, blk.ID); err != nil && !errors.Is(err, store.ErrCycle) {
						s.logger().Warn("add pulled edge failed", "ticket", tk.ID, "blocked_by", blk.ID, "err", err)
					}
				}
			}
		}
	}
	t.Cursor = next
	t.LastPullAt = s.now()
	return nil
}

func (s *Syncer) lookup(ctx context.Context, trackerID, ext string, byExt map[string]store.Ticket) (store.Ticket, error) {
	if tk, ok := byExt[ext]; ok {
		return tk, nil
	}
	return s.Store.TicketByExternalRef(ctx, trackerID, ext)
}

// applyPulled creates or updates the ticket for one pulled item.
func (s *Syncer) applyPulled(ctx context.Context, t *store.Tracker, it Item, caps Caps) (store.Ticket, error) {
	existing, err := s.Store.TicketByExternalRef(ctx, t.ID, it.ExternalID)
	if errors.Is(err, store.ErrNotFound) {
		if it.Deleted {
			return store.Ticket{}, nil
		}
		status := it.Status
		if !store.ValidTicketStatus(status) {
			status = store.TicketStatusBacklog
		}
		kind := it.Kind
		if !store.ValidTicketKind(kind) {
			kind = store.TicketKindFeature
		}
		created, err := s.Store.CreateTicket(ctx, store.Ticket{
			ProjectID: t.ProjectID, Kind: kind, Title: it.Title, Body: it.Body,
			Acceptance: it.Acceptance, Labels: it.Labels, Priority: it.Priority, Status: status,
		})
		if err != nil {
			return store.Ticket{}, err
		}
		s.markSynced(ctx, t.ID, it, created)
		s.emit(created)
		return created, nil
	}
	if err != nil {
		return store.Ticket{}, err
	}
	if it.Deleted {
		// The tracker deleted it: cancel it unless Syre is working it, and keep the ref as
		// a tombstone so the ticket is never pushed back as a new item.
		tk := existing
		if !syreActive(existing) && existing.Status != store.TicketStatusDone {
			existing.Status = store.TicketStatusCanceled
			if updated, err := s.Store.UpdateTicket(ctx, existing); err == nil {
				tk = updated
				s.emit(updated)
			}
		}
		it.Version = deletedVersion
		s.markSynced(ctx, t.ID, it, tk)
		return tk, nil
	}
	dirty := s.isDirty(ctx, t.ID, existing)
	merged, changed := MergePulled(existing, it, caps)
	if !changed {
		if !dirty {
			s.markSynced(ctx, t.ID, it, existing)
		}
		return existing, nil
	}
	updated, err := s.Store.UpdateTicket(ctx, merged)
	if err != nil {
		return store.Ticket{}, err
	}
	if dirty {
		// Syre has unpushed changes: record the new version but keep the ref dirty.
		s.putRef(ctx, t.ID, updated.ID, it, time.Time{}, true)
	} else {
		s.markSynced(ctx, t.ID, it, updated)
	}
	s.emit(updated)
	return updated, nil
}

func (s *Syncer) refFor(ctx context.Context, trackerID, ticketID string) (store.ExternalRef, bool) {
	refs, err := s.Store.ListExternalRefs(ctx, ticketID)
	if err != nil {
		return store.ExternalRef{}, false
	}
	for _, r := range refs {
		if r.Tracker == trackerID {
			return r, true
		}
	}
	return store.ExternalRef{}, false
}

// isDirty reports whether Syre changed tk since its last sync with trackerID.
func (s *Syncer) isDirty(ctx context.Context, trackerID string, tk store.Ticket) bool {
	ref, ok := s.refFor(ctx, trackerID, tk.ID)
	return !ok || tk.UpdatedAt.After(ref.SyncedAt)
}

func (s *Syncer) markSynced(ctx context.Context, trackerID string, it Item, tk store.Ticket) {
	s.putRef(ctx, trackerID, tk.ID, it, tk.UpdatedAt, false)
}

func (s *Syncer) putRef(ctx context.Context, trackerID, ticketID string, it Item, synced time.Time, keepSynced bool) {
	ref := store.ExternalRef{TicketID: ticketID, Tracker: trackerID, ExternalID: it.ExternalID, URL: it.URL, Version: it.Version, SyncedAt: synced}
	if keepSynced {
		if old, ok := s.refFor(ctx, trackerID, ticketID); ok {
			ref.SyncedAt = old.SyncedAt
		}
	}
	if err := s.Store.PutExternalRef(ctx, ref); err != nil {
		s.logger().Warn("put external ref failed", "ticket", ticketID, "tracker", trackerID, "err", err)
	}
}

func (s *Syncer) emit(tk store.Ticket) {
	if s.OnTicket != nil {
		s.OnTicket(tk)
	}
}

// push sends tickets Syre created or changed since their last sync. Containers go first
// so the tracker knows parents before children.
func (s *Syncer) push(ctx context.Context, t *store.Tracker, a Tracker) error {
	tickets, err := s.Store.ListTickets(ctx, store.TicketFilter{ProjectID: t.ProjectID})
	if err != nil {
		return err
	}
	refs, err := s.Store.ListExternalRefsByTracker(ctx, t.ID)
	if err != nil {
		return err
	}
	extOf := map[string]store.ExternalRef{}
	for _, r := range refs {
		extOf[r.TicketID] = r
	}
	caps := a.Capabilities()
	rank := func(tk store.Ticket) int {
		switch tk.Kind {
		case store.TicketKindInitiative:
			return 0
		case store.TicketKindEpic:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(tickets, func(x, y store.Ticket) int { return rank(x) - rank(y) })

	// Push one rank at a time so children can name parents created in this cycle.
	for r := 0; r <= 2; r++ {
		var batch []PushItem
		byTicket := map[string]store.Ticket{}
		for _, tk := range tickets {
			if rank(tk) != r {
				continue
			}
			ref, linked := extOf[tk.ID]
			if linked && (ref.Version == deletedVersion || !tk.UpdatedAt.After(ref.SyncedAt)) {
				continue
			}
			parentExt := ""
			if caps.Hierarchy && tk.ParentID != "" {
				parentExt = extOf[tk.ParentID].ExternalID
			}
			var blockedExt []string
			if caps.DAG {
				edges, _ := s.Store.ListTicketEdges(ctx, tk.ID)
				for _, e := range edges {
					if ext := extOf[e.BlockedByID].ExternalID; ext != "" {
						blockedExt = append(blockedExt, ext)
					}
				}
			}
			it := ItemFromTicket(tk, parentExt, blockedExt)
			if !caps.Claims {
				it.Claim = nil
			}
			batch = append(batch, PushItem{TicketID: tk.ID, ExternalID: ref.ExternalID, Item: it})
			byTicket[tk.ID] = tk
		}
		if len(batch) == 0 {
			continue
		}
		results, err := a.Push(ctx, batch)
		if err != nil {
			return fmt.Errorf("push: %w", err)
		}
		var failed []string
		for _, res := range results {
			tk, ok := byTicket[res.TicketID]
			if !ok {
				continue
			}
			if res.Err != nil {
				failed = append(failed, tk.SID+": "+res.Err.Error())
				continue
			}
			ref := store.ExternalRef{TicketID: tk.ID, Tracker: t.ID, ExternalID: res.ExternalID, URL: res.URL, Version: res.Version, SyncedAt: tk.UpdatedAt}
			if err := s.Store.PutExternalRef(ctx, ref); err != nil {
				failed = append(failed, tk.SID+": "+err.Error())
				continue
			}
			extOf[tk.ID] = ref
		}
		if len(failed) > 0 {
			t.LastPushAt = s.now()
			return fmt.Errorf("push: %s", strings.Join(failed, "; "))
		}
	}
	t.LastPushAt = s.now()
	return nil
}
