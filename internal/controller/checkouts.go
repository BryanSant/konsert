package controller

import (
	"context"
	"strings"
	"sync"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

type checkoutKey struct {
	workerID  string
	remoteURL string // normalized remote URL
}

// checkoutState holds the Hub's in-memory derived table of reported git checkouts
// keyed by (worker_id, remote_url_normalized), plus per-worker lists for path matching.
type checkoutState struct {
	mu       sync.RWMutex
	byKey    map[checkoutKey]protocol.ReportedCheckout
	byWorker map[string][]protocol.ReportedCheckout
}

func (c *checkoutState) set(workerID string, checkouts []protocol.ReportedCheckout) {
	if workerID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = make(map[checkoutKey]protocol.ReportedCheckout)
	}
	if c.byWorker == nil {
		c.byWorker = make(map[string][]protocol.ReportedCheckout)
	}
	// Remove previous keys for this worker.
	for _, old := range c.byWorker[workerID] {
		delete(c.byKey, checkoutKey{workerID: workerID, remoteURL: old.RemoteURL})
	}
	clean := make([]protocol.ReportedCheckout, 0, len(checkouts))
	for _, co := range checkouts {
		norm := gitutil.NormalizeRemoteURL(co.RemoteURL)
		if norm == "" || strings.TrimSpace(co.Path) == "" {
			continue
		}
		co.RemoteURL = norm
		clean = append(clean, co)
		k := checkoutKey{workerID: workerID, remoteURL: norm}
		if _, exists := c.byKey[k]; !exists {
			c.byKey[k] = co
		}
	}
	c.byWorker[workerID] = clean
}

func (c *checkoutState) getByWorker(workerID string) ([]protocol.ReportedCheckout, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list, ok := c.byWorker[workerID]
	if !ok {
		return nil, false
	}
	out := make([]protocol.ReportedCheckout, len(list))
	copy(out, list)
	return out, true
}

func (c *checkoutState) lookup(workerID, normalizedRemoteURL string) (protocol.ReportedCheckout, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	co, ok := c.byKey[checkoutKey{workerID: workerID, remoteURL: normalizedRemoteURL}]
	return co, ok
}

// setWorkerCheckouts updates the hub's derived checkout table for workerID.
func (h *Hub) setWorkerCheckouts(workerID string, checkouts []protocol.ReportedCheckout) {
	h.checkouts.set(workerID, checkouts)
}

// workerCheckouts returns the reported checkouts for workerID, lazily populating from
// persisted Worker.FactsJSON if not yet in memory (e.g. after a hub restart or in tests).
func (h *Hub) workerCheckouts(ctx context.Context, workerID string) []protocol.ReportedCheckout {
	if workerID == "" {
		return nil
	}
	if list, ok := h.checkouts.getByWorker(workerID); ok {
		return list
	}
	if h.store == nil {
		return nil
	}
	w, err := h.store.GetWorker(ctx, workerID)
	if err != nil {
		return nil
	}
	f := decodeWorkerFacts(w.FactsJSON)
	if f == nil {
		return nil
	}
	h.checkouts.set(workerID, f.Checkouts)
	list, _ := h.checkouts.getByWorker(workerID)
	return list
}

// WorkerCheckoutPath returns the reported local checkout path on workerID for remoteURL, if any.
func (h *Hub) WorkerCheckoutPath(ctx context.Context, workerID, remoteURL string) (string, bool) {
	norm := gitutil.NormalizeRemoteURL(remoteURL)
	if workerID == "" || norm == "" {
		return "", false
	}
	if _, ok := h.checkouts.getByWorker(workerID); !ok {
		_ = h.workerCheckouts(ctx, workerID)
	}
	if co, ok := h.checkouts.lookup(workerID, norm); ok {
		return co.Path, true
	}
	return "", false
}

// repoForPath maps a working directory on workerID to the registered store.Repo whose
// reported checkout on that machine contains cwd (longest path prefix match).
func (h *Hub) repoForPath(ctx context.Context, workerID, cwd string) (store.Repo, bool) {
	if workerID == "" || strings.TrimSpace(cwd) == "" || h.store == nil {
		return store.Repo{}, false
	}
	checkouts := h.workerCheckouts(ctx, workerID)
	if len(checkouts) == 0 {
		return store.Repo{}, false
	}
	repos, err := h.store.ListRepos(ctx, "")
	if err != nil || len(repos) == 0 {
		return store.Repo{}, false
	}
	// Index registered repos by normalized remote URL. Prefer non-default projects on collision.
	byRemote := make(map[string]store.Repo, len(repos))
	for _, r := range repos {
		norm := gitutil.NormalizeRemoteURL(r.RemoteURL)
		if norm == "" {
			continue
		}
		if existing, ok := byRemote[norm]; !ok || (existing.ProjectID == store.DefaultProjectID && r.ProjectID != store.DefaultProjectID) {
			byRemote[norm] = r
		}
	}

	var (
		bestRepo store.Repo
		bestLen  int
		matched  bool
	)
	for _, co := range checkouts {
		if !gitutil.PathContains(co.Path, cwd) {
			continue
		}
		r, ok := byRemote[co.RemoteURL]
		if !ok {
			continue
		}
		if !matched || len(co.Path) > bestLen {
			bestRepo = r
			bestLen = len(co.Path)
			matched = true
		}
	}
	return bestRepo, matched
}
