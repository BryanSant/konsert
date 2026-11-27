package tracker

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// Fake is an in-memory Tracker for tests: items keyed by external ID, a revision counter
// as the cursor, and every push recorded.
type Fake struct {
	Caps Caps
	// EchoPushes makes pushed items show up in the next Pull (with the version the push
	// returned), as real trackers like ka do.
	EchoPushes bool

	mu     sync.Mutex
	items  map[string]Item
	rev    map[string]int
	clock  int
	nextID int
	Pushes [][]PushItem
}

// NewFake returns a Fake with every capability.
func NewFake() *Fake {
	return &Fake{Caps: Caps{Hierarchy: true, DAG: true, Acceptance: true, Claims: true}, items: map[string]Item{}, rev: map[string]int{}}
}

func (f *Fake) Kind() string       { return "fake" }
func (f *Fake) Capabilities() Caps { return f.Caps }

// Put adds or changes an item as if a human edited it in the tracker.
func (f *Fake) Put(it Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock++
	it.Version = strconv.Itoa(f.clock)
	f.items[it.ExternalID] = it
	f.rev[it.ExternalID] = f.clock
}

// Get returns an item.
func (f *Fake) Get(id string) (Item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	return it, ok
}

// Delete removes an item (reported as Deleted on the next pull).
func (f *Fake) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock++
	it := f.items[id]
	it.Deleted = true
	f.items[id] = it
	f.rev[id] = f.clock
}

func (f *Fake) Pull(_ context.Context, cursor string) ([]Item, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	since, _ := strconv.Atoi(cursor)
	var out []Item
	for id, it := range f.items {
		if f.rev[id] > since {
			out = append(out, it)
		}
	}
	return out, strconv.Itoa(f.clock), nil
}

func (f *Fake) Push(_ context.Context, items []PushItem) ([]PushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Pushes = append(f.Pushes, items)
	out := make([]PushResult, 0, len(items))
	for _, p := range items {
		id := p.ExternalID
		if id == "" {
			f.nextID++
			id = fmt.Sprintf("EXT-%d", f.nextID)
		}
		it := p.Item
		it.ExternalID = id
		f.clock++
		it.Version = strconv.Itoa(f.clock)
		f.items[id] = it
		// A push is the tracker catching up with Syre, not a human change; real trackers
		// still report it as changed (EchoPushes).
		f.rev[id] = 0
		if f.EchoPushes {
			f.rev[id] = f.clock
		}
		out = append(out, PushResult{TicketID: p.TicketID, ExternalID: id, URL: "fake://" + id, Version: it.Version})
	}
	return out, nil
}
