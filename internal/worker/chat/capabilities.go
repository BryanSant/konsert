package chat

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// StatusChecker reports an account's login state with a cheap CLI status command, for
// agents without a registered engine.
type StatusChecker func(ctx context.Context, acct Account) protocol.AgentAccount

// Defaults for CapabilitiesOptions.
const (
	DefaultProbeTimeout = 30 * time.Second
	DefaultProbeTTL     = 5 * time.Minute
	// DefaultForceInterval is the minimum time between failure-triggered re-probes of
	// one account, so a failing account is never probed in a loop.
	DefaultForceInterval = 2 * time.Minute
)

// CapabilitiesOptions configures a Capabilities prober.
type CapabilitiesOptions struct {
	Accounts *AccountRegistry
	// Engine returns the registered engine for an agent (Manager.Engine).
	Engine func(agent string) (Engine, bool)
	// Status checks agents without an engine (default CLIStatus).
	Status        StatusChecker
	Timeout       time.Duration
	TTL           time.Duration
	ForceInterval time.Duration
	Logger        *slog.Logger
	Now           func() time.Time
	// PersistPath, when set, stores exhausted usage windows (no secrets: window, percent,
	// reset time) so a restarted syred still knows an account is out of usage before
	// the user sends anything.
	PersistPath string
	// Sandboxed marks approval modes above protocol.SandboxMaxRisk as disabled (syred
	// --sandbox refuses them).
	Sandboxed bool
}

// Capabilities probes every account concurrently (with timeouts and per-account caching)
// and assembles the AgentCapabilitiesReport. Usage seen in live chats (rate_limit events,
// usage_limit errors) is overlaid on the cached probe results.
type Capabilities struct {
	opts CapabilitiesOptions

	mu       sync.Mutex
	cache    map[string]capEntry
	forcedAt map[string]time.Time
	// overlays hold window state seen in chats, per account and window. Exhausted windows
	// stick until ResetsAt even if a probe (which may lag) says otherwise.
	overlays map[string]map[string]overlay
	probing  map[string]chan struct{}
}

type capEntry struct {
	acct protocol.AgentAccount
	at   time.Time
}

type overlay struct {
	rl    protocol.AgentRateLimit
	seen  time.Time
	until time.Time // exhausted: sticky until here
}

// NewCapabilities creates a prober.
func NewCapabilities(opts CapabilitiesOptions) *Capabilities {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultProbeTimeout
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultProbeTTL
	}
	if opts.ForceInterval <= 0 {
		opts.ForceInterval = DefaultForceInterval
	}
	if opts.Status == nil {
		opts.Status = CLIStatus
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Engine == nil {
		opts.Engine = func(string) (Engine, bool) { return nil, false }
	}
	c := &Capabilities{
		opts:     opts,
		cache:    map[string]capEntry{},
		forcedAt: map[string]time.Time{},
		overlays: map[string]map[string]overlay{},
		probing:  map[string]chan struct{}{},
	}
	c.loadOverlays()
	return c
}

// persistedOverlay is one exhausted window saved across restarts.
type persistedOverlay struct {
	Account string                  `json:"account"`
	Limit   protocol.AgentRateLimit `json:"limit"`
	Until   time.Time               `json:"until"`
}

// loadOverlays restores exhausted windows that have not reset yet.
func (c *Capabilities) loadOverlays() {
	if c.opts.PersistPath == "" {
		return
	}
	raw, err := os.ReadFile(c.opts.PersistPath)
	if err != nil {
		return
	}
	var saved []persistedOverlay
	if err := unmarshal(raw, &saved); err != nil {
		c.opts.Logger.Warn("ignoring unreadable usage cache", "path", c.opts.PersistPath, "err", err)
		return
	}
	now := c.opts.Now()
	for _, p := range saved {
		if p.Account == "" || p.Limit.Window == "" || !now.Before(p.Until) {
			continue
		}
		m := c.overlays[p.Account]
		if m == nil {
			m = map[string]overlay{}
			c.overlays[p.Account] = m
		}
		m[p.Limit.Window] = overlay{rl: p.Limit, seen: now, until: p.Until}
	}
}

// saveOverlaysLocked writes the exhausted windows (atomically, 0600). Caller holds c.mu.
func (c *Capabilities) saveOverlaysLocked() {
	if c.opts.PersistPath == "" {
		return
	}
	now := c.opts.Now()
	saved := []persistedOverlay{}
	for acct, m := range c.overlays {
		for _, o := range m {
			if o.rl.State == "exhausted" && now.Before(o.until) {
				saved = append(saved, persistedOverlay{Account: acct, Limit: o.rl, Until: o.until})
			}
		}
	}
	b, err := json.Marshal(saved)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.opts.PersistPath), 0o700); err != nil {
		return
	}
	tmp := c.opts.PersistPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.opts.PersistPath)
}

// Invalidate marks an account (or every account when id is "") for re-probing on the next
// Report. Repeated invalidations of one account within ForceInterval are ignored, so
// failures can never cause a probe loop. It reports whether the cache was invalidated.
func (c *Capabilities) Invalidate(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.opts.Now()
	if id == "" {
		c.cache = map[string]capEntry{}
		return true
	}
	if t, ok := c.forcedAt[id]; ok && now.Sub(t) < c.opts.ForceInterval {
		return false
	}
	c.forcedAt[id] = now
	delete(c.cache, id)
	return true
}

// Forget drops the cached result of one account (every account when id is ""), without
// the rate limit of Invalidate: used for explicit user refreshes.
func (c *Capabilities) Forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" {
		c.cache = map[string]capEntry{}
		return
	}
	delete(c.cache, id)
}

// NoteRateLimit records subscription window state seen in a chat for an account and
// reports whether the account's reported usage changed (the caller then pushes a report).
func (c *Capabilities) NoteRateLimit(accountID string, rl protocol.AgentRateLimit) bool {
	if accountID == "" || rl.Window == "" {
		return false
	}
	now := c.opts.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.overlays[accountID]
	if m == nil {
		m = map[string]overlay{}
		c.overlays[accountID] = m
	}
	prev, had := m[rl.Window]
	o := overlay{rl: rl, seen: now}
	if rl.State == "exhausted" {
		if rl.UsedPercent == 0 {
			o.rl.UsedPercent = 100
		}
		if rl.ResetsAt != nil {
			o.until = *rl.ResetsAt
		} else {
			// Unknown reset: keep showing it for a while; the next chat result updates it.
			o.until = now.Add(time.Hour)
		}
	}
	m[rl.Window] = o
	changed := !had || !sameRateLimit(prev.rl, o.rl)
	if changed && (o.rl.State == "exhausted" || prev.rl.State == "exhausted") {
		c.saveOverlaysLocked()
	}
	return changed
}

func sameRateLimit(a, b protocol.AgentRateLimit) bool {
	if a.Window != b.Window || a.State != b.State || a.UsedPercent != b.UsedPercent {
		return false
	}
	if (a.ResetsAt == nil) != (b.ResetsAt == nil) {
		return false
	}
	return a.ResetsAt == nil || a.ResetsAt.Equal(*b.ResetsAt)
}

// Exhausted returns the latest reset time of an exhausted window of the account, if any.
func (c *Capabilities) Exhausted(accountID string) (time.Time, bool) {
	now := c.opts.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var until time.Time
	for _, o := range c.overlays[accountID] {
		if o.rl.State == "exhausted" && now.Before(o.until) && o.until.After(until) {
			until = o.until
		}
	}
	return until, !until.IsZero()
}

// Cached assembles a report from cached results only (no probes); accounts never probed
// are reported with AuthUnknown.
func (c *Capabilities) Cached() protocol.AgentCapabilitiesReport {
	entries := c.opts.Accounts.List()
	out := protocol.AgentCapabilitiesReport{Accounts: make([]protocol.AgentAccount, 0, len(entries))}
	c.mu.Lock()
	for _, e := range entries {
		a, ok := c.cache[e.ID]
		acct := a.acct
		if !ok {
			acct = c.skeleton(e)
			acct.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: "not checked yet"}
		}
		out.Accounts = append(out.Accounts, c.applyOverlaysLocked(acct))
	}
	c.mu.Unlock()
	return out
}

// Report reloads the account list and returns every account's capabilities, probing
// accounts whose cache is stale (all of them when force is set).
func (c *Capabilities) Report(ctx context.Context, force bool) protocol.AgentCapabilitiesReport {
	c.opts.Accounts.Load()
	entries := c.opts.Accounts.List()
	results := make([]protocol.AgentAccount, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.account(ctx, e, force)
		}()
	}
	wg.Wait()
	c.mu.Lock()
	for i := range results {
		results[i] = c.applyOverlaysLocked(results[i])
		if c.opts.Sandboxed {
			results[i] = markSandboxedModes(results[i])
		}
	}
	// Forget cache entries of accounts that disappeared.
	keep := map[string]bool{}
	for _, e := range entries {
		keep[e.ID] = true
	}
	for id := range c.cache {
		if !keep[id] {
			delete(c.cache, id)
		}
	}
	c.mu.Unlock()
	return protocol.AgentCapabilitiesReport{Accounts: results}
}

func (c *Capabilities) skeleton(e AccountEntry) protocol.AgentAccount {
	_, hasEngine := c.opts.Engine(e.Agent)
	return protocol.AgentAccount{ID: e.ID, Agent: e.Agent, Label: e.Label, IsDefault: e.IsDefault, ChatSupported: hasEngine}
}

// account returns one account's capabilities from cache or a fresh probe. Concurrent
// callers share one probe per account.
func (c *Capabilities) account(ctx context.Context, e AccountEntry, force bool) protocol.AgentAccount {
	for {
		c.mu.Lock()
		if ent, ok := c.cache[e.ID]; ok && !force && c.opts.Now().Sub(ent.at) < c.opts.TTL {
			c.mu.Unlock()
			return ent.acct
		}
		if wait, busy := c.probing[e.ID]; busy {
			c.mu.Unlock()
			select {
			case <-wait:
				force = false // use the result the other caller just stored
				continue
			case <-ctx.Done():
				return c.failed(e, "capability check cancelled")
			}
		}
		done := make(chan struct{})
		c.probing[e.ID] = done
		c.mu.Unlock()

		res := c.probe(ctx, e)
		c.mu.Lock()
		c.cache[e.ID] = capEntry{acct: res, at: c.opts.Now()}
		delete(c.probing, e.ID)
		close(done)
		c.mu.Unlock()
		return res
	}
}

func (c *Capabilities) failed(e AccountEntry, detail string) protocol.AgentAccount {
	a := c.skeleton(e)
	a.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: detail}
	a.CheckedAt = c.opts.Now().UTC()
	return a
}

func (c *Capabilities) probe(ctx context.Context, e AccountEntry) protocol.AgentAccount {
	if e.Problem != "" {
		a := c.skeleton(e)
		a.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Detail: e.Problem}
		a.CheckedAt = c.opts.Now().UTC()
		a.ChatSupported = false
		return withNote(a, e.Note)
	}
	acct, err := c.opts.Accounts.Resolve(e.Agent, e.ID)
	if err != nil {
		a := c.skeleton(e)
		a.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Detail: err.Error()}
		a.CheckedAt = c.opts.Now().UTC()
		return withNote(a, e.Note)
	}
	eng, hasEngine := c.opts.Engine(e.Agent)
	pctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	resCh := make(chan protocol.AgentAccount, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.opts.Logger.Error("agent capability probe panicked", "account", e.ID, "panic", r)
				resCh <- c.failed(e, "capability check failed")
			}
		}()
		if hasEngine {
			resCh <- eng.Probe(pctx, acct)
		} else {
			resCh <- c.opts.Status(pctx, acct)
		}
	}()
	var a protocol.AgentAccount
	select {
	case a = <-resCh:
	case <-pctx.Done():
		a = c.failed(e, "capability check timed out")
		c.opts.Logger.Warn("agent capability probe timed out", "account", e.ID, "agent", e.Agent)
	}
	// The registry is authoritative for identity; the engine decides the rest.
	a.ID, a.Agent, a.Label, a.IsDefault = e.ID, e.Agent, e.Label, e.IsDefault
	a.ChatSupported = hasEngine
	if a.Auth.State == "" {
		a.Auth.State = protocol.AuthUnknown
	}
	if a.CheckedAt.IsZero() {
		a.CheckedAt = c.opts.Now().UTC()
	}
	return withNote(a, e.Note)
}

// withNote adds an account caveat (keychain-bound CLIs on macOS) to Auth.Detail and
// UsageNote.
func withNote(a protocol.AgentAccount, note string) protocol.AgentAccount {
	if note == "" {
		return a
	}
	if a.Auth.Detail == "" {
		a.Auth.Detail = note
	} else {
		a.Auth.Detail += " " + note
	}
	if a.UsageNote == "" {
		a.UsageNote = note
	}
	return a
}

// applyOverlaysLocked merges window state seen in chats into a probe result. c.mu held.
func (c *Capabilities) applyOverlaysLocked(a protocol.AgentAccount) protocol.AgentAccount {
	m := c.overlays[a.ID]
	if len(m) == 0 {
		return a
	}
	now := c.opts.Now()
	usage := append([]protocol.AgentRateLimit(nil), a.Usage...)
	idx := map[string]int{}
	for i, u := range usage {
		idx[u.Window] = i
	}
	windows := make([]string, 0, len(m))
	for w := range m {
		windows = append(windows, w)
	}
	sort.Strings(windows)
	for _, w := range windows {
		o := m[w]
		if o.rl.State == "exhausted" {
			if !now.Before(o.until) {
				delete(m, w) // reset time passed: trust the probe again
				continue
			}
		} else if !o.seen.After(a.CheckedAt) {
			continue // the probe is newer than this chat observation
		}
		rl := o.rl
		if i, ok := idx[w]; ok {
			usage[i] = rl
		} else {
			idx[w] = len(usage)
			usage = append(usage, rl)
		}
	}
	a.Usage = usage
	return a
}

// markSandboxedModes disables approval modes a --sandbox worker refuses, and moves the
// default to the highest allowed mode if needed.
func markSandboxedModes(a protocol.AgentAccount) protocol.AgentAccount {
	if len(a.Approvals) == 0 {
		return a
	}
	modes := append([]protocol.AgentApprovalMode(nil), a.Approvals...)
	best := ""
	bestRisk := -1
	for i := range modes {
		if modes[i].Risk > protocol.SandboxMaxRisk {
			modes[i].Disabled = protocol.SandboxDisabledReason
		} else if modes[i].Risk > bestRisk {
			best, bestRisk = modes[i].ID, modes[i].Risk
		}
	}
	a.Approvals = modes
	for _, m := range modes {
		if m.ID == a.DefaultApproval && m.Disabled != "" {
			a.DefaultApproval = best
		}
	}
	return a
}

// ModeRisk returns the risk of approval mode id for an account from the cached report
// (-1 when unknown).
func (c *Capabilities) ModeRisk(accountID, mode string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[accountID]; ok {
		for _, m := range e.acct.Approvals {
			if m.ID == mode {
				return m.Risk
			}
		}
	}
	return -1
}
