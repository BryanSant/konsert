// Package katracker syncs a KanArtist (ka) store with a Syre project. The hub keeps its
// own git clone of the store and drives it through ka's service API (pkg/kaservice), so
// `ka` on any machine sees the same backlog and Syre's leases appear as read-only claims
// (claim.authority: syre).
package katracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/KanArtist/ka/pkg/kaservice"

	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/tracker"
)

// Kind is the tracker kind for ka stores.
const Kind = "ka"

// Config is a ka tracker's config_json.
type Config struct {
	// StoreURL is the ka store's git URL (or a local path).
	StoreURL string `json:"store_url"`
	// Project is the ka project key whose tickets sync with the Syre project.
	Project string `json:"project"`
	// CloneDir is where Syre keeps its clone (default: the user cache dir).
	CloneDir string `json:"clone_dir,omitempty"`
}

// Tracker drives one ka store clone.
type Tracker struct {
	cfg Config
	svc *kaservice.Service
	mu  sync.Mutex
}

// Factory opens the adapter for a tracker row: clone the store if needed, write Syre's
// own workspace file next to it, and open ka's service on it.
func Factory(ctx context.Context, t store.Tracker) (tracker.Tracker, error) {
	var cfg Config
	if err := json.Unmarshal([]byte(t.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("ka tracker config: %w", err)
	}
	if strings.TrimSpace(cfg.StoreURL) == "" || strings.TrimSpace(cfg.Project) == "" {
		return nil, fmt.Errorf("ka tracker config needs store_url and project")
	}
	dir := cfg.CloneDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "syre", "ka", t.ID)
	}
	return Open(ctx, cfg, dir)
}

// Open clones cfg.StoreURL into dir (once) and opens the service.
func Open(ctx context.Context, cfg Config, dir string) (*Tracker, error) {
	clone := filepath.Join(dir, "store")
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if out, err := exec.CommandContext(ctx, "git", "clone", "-q", cfg.StoreURL, clone).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("clone ka store: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	ws := filepath.Join(dir, "workspace.yml")
	// push: immediate so Syre's writes (claims, status) reach other clients at once.
	body := fmt.Sprintf("name: syre\nstores:\n  - name: syre\n    path: %s\n    push: immediate\nidentity:\n  actor: %s\n", clone, tracker.SyreActor)
	if err := os.WriteFile(ws, []byte(body), 0o644); err != nil {
		return nil, err
	}
	w, err := kaservice.Open(ctx, kaservice.OpenOptions{Workspace: ws})
	if err != nil {
		return nil, fmt.Errorf("open ka workspace: %w", err)
	}
	svc, err := kaservice.New(ctx, w)
	if err != nil {
		return nil, fmt.Errorf("open ka service: %w", err)
	}
	return &Tracker{cfg: cfg, svc: svc}, nil
}

func (k *Tracker) Kind() string { return Kind }

func (k *Tracker) Capabilities() tracker.Caps {
	return tracker.Caps{Hierarchy: true, DAG: true, Acceptance: true, Claims: true}
}

func (k *Tracker) caller(session string) kaservice.Caller {
	return kaservice.Caller{Actor: tracker.SyreActor, Session: session, Authority: tracker.SyreAuthority}
}

// Pull fetches the store and returns the project's tickets updated at or after cursor
// (an RFC 3339 timestamp; ka's updated stamps sort lexically).
func (k *Tracker) Pull(ctx context.Context, cursor string) ([]tracker.Item, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	// Force a fetch: a pull must see what other clients pushed, even right after one.
	k.svc.Sync(ctx, true)
	res, err := k.svc.Search(kaservice.SearchIn{Query: "project:" + k.cfg.Project, Limit: -1, All: true})
	if err != nil {
		return nil, cursor, err
	}
	next := cursor
	var out []tracker.Item
	for _, sm := range res.Tickets {
		// ka stamps have one-second resolution: re-read the cursor's own second so an edit
		// made in that second isn't lost (re-applying an unchanged ticket is a no-op).
		if sm.Updated < cursor {
			continue
		}
		d, err := k.svc.Get(kaservice.GetIn{ID: sm.ID})
		if err != nil {
			return nil, cursor, err
		}
		out = append(out, itemFromDetail(d))
		if sm.Updated > next {
			next = sm.Updated
		}
	}
	return out, next, nil
}

func itemFromDetail(d *kaservice.Detail) tracker.Item {
	it := tracker.Item{
		ExternalID: d.ID, URL: d.Path,
		Kind: kindFromKa(d.Kind), Title: d.Title, Body: d.Body, Labels: d.Labels,
		Priority: d.Priority, Status: StatusFromKa(d.Status),
	}
	for _, a := range d.AcceptanceAll {
		it.Acceptance = append(it.Acceptance, store.AcceptanceItem(a))
	}
	if d.ParentRef != nil {
		it.ParentExternalID = d.ParentRef.ID
	} else if d.Parent != "" {
		it.ParentExternalID = d.Parent
	}
	for _, b := range d.BlockedBy {
		it.BlockedBy = append(it.BlockedBy, b.ID)
	}
	if d.Claim != nil {
		it.Claim = &tracker.Claim{By: d.Claim.By, Authority: d.Claim.Authority}
	}
	it.Version = itemVersion(d.Updated, it)
	return it
}

// itemVersion is the ka updated stamp plus a hash of the fields Syre syncs. The stamp
// alone has one-second resolution: an edit in the same second as the last sync left it
// unchanged, and the syncer skipped the edit as an echo of what it already had.
func itemVersion(updated string, it tracker.Item) string {
	it.Version, it.URL = "", ""
	raw, err := json.Marshal(it, json.Deterministic(true))
	if err != nil {
		return updated
	}
	sum := sha256.Sum256(raw)
	return updated + "#" + hex.EncodeToString(sum[:6])
}

// Push creates or updates tickets in the ka store.
func (k *Tracker) Push(ctx context.Context, items []tracker.PushItem) ([]tracker.PushResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]tracker.PushResult, 0, len(items))
	for _, p := range items {
		res := tracker.PushResult{TicketID: p.TicketID, ExternalID: p.ExternalID}
		id, err := k.pushOne(ctx, p)
		if err != nil {
			res.Err = err
		} else {
			res.ExternalID = id
			if d, err := k.svc.Get(kaservice.GetIn{ID: id}); err == nil {
				res.URL, res.Version = d.Path, itemFromDetail(d).Version
			}
		}
		out = append(out, res)
	}
	return out, nil
}

func (k *Tracker) pushOne(ctx context.Context, p tracker.PushItem) (string, error) {
	it := p.Item
	session := ""
	if it.Claim != nil {
		session = it.Claim.Session
	}
	c := k.caller(session)
	if p.ExternalID == "" {
		in := kaservice.CreateIn{
			Project: k.cfg.Project, Title: it.Title, Kind: kindToKa(it.Kind), Priority: it.Priority,
			Status: StatusToKa(it.Status), Labels: it.Labels, Parent: it.ParentExternalID,
			BlockedBy: it.BlockedBy, Body: it.Body,
		}
		for _, a := range it.Acceptance {
			in.Acceptance = append(in.Acceptance, a.Text)
		}
		if closedStatus(in.Status) {
			in.Status = "" // closing needs a resolution; create open, then transition
		}
		created, err := k.svc.Create(ctx, c, []kaservice.CreateIn{in})
		if err != nil {
			return "", err
		}
		if len(created.Created) == 0 {
			return "", fmt.Errorf("ka created nothing")
		}
		id := created.Created[0].ID
		p.ExternalID = id
		if err := k.syncState(ctx, c, id, it, true); err != nil {
			return id, err
		}
		return id, nil
	}
	set := map[string]any{"title": it.Title}
	if it.Priority != "" {
		set["priority"] = it.Priority
	}
	if kd := kindToKa(it.Kind); kd != "" {
		set["kind"] = kd
	}
	if it.ParentExternalID != "" {
		set["parent"] = it.ParentExternalID
	}
	up := kaservice.UpdateIn{ID: p.ExternalID, Set: set}
	if it.Body != "" {
		body := it.Body
		up.Body = &body
	}
	if len(it.BlockedBy) > 0 {
		up.Add = map[string][]string{"blocked_by": it.BlockedBy}
	}
	if _, err := k.svc.Update(ctx, c, []kaservice.UpdateIn{up}); err != nil {
		return p.ExternalID, err
	}
	return p.ExternalID, k.syncState(ctx, c, p.ExternalID, it, false)
}

// syncState pushes Syre-owned state: acceptance done flags, status, and the claim.
func (k *Tracker) syncState(ctx context.Context, c kaservice.Caller, id string, it tracker.Item, created bool) error {
	d, err := k.svc.Get(kaservice.GetIn{ID: id})
	if err != nil {
		return err
	}
	var checks []kaservice.CheckItem
	for i, a := range it.Acceptance {
		if i < len(d.AcceptanceAll) && d.AcceptanceAll[i].Done != a.Done && a.Done {
			idx := i
			checks = append(checks, kaservice.CheckItem{Index: &idx, Done: true})
		}
	}
	if len(checks) > 0 {
		if _, _, err := k.svc.CheckItems(ctx, c, kaservice.CheckIn{ID: id, Items: checks}); err != nil {
			return err
		}
	}
	// Claim before moving to a started status so ka records who works it.
	held := d.Claim != nil && d.Claim.Authority == tracker.SyreAuthority
	if it.Claim != nil && !held {
		if _, err := k.svc.Claim(ctx, c, id, false); err != nil {
			return err
		}
	}
	if want := StatusToKa(it.Status); want != "" {
		if cur, _ := k.svc.Get(kaservice.GetIn{ID: id}); cur != nil && cur.Status != want {
			if it.Claim == nil && held && !closedStatus(want) {
				// Releasing with a status moves it in the same op.
				if _, err := k.svc.Release(ctx, c, id, want, false); err != nil {
					return err
				}
				held = false
			} else if _, err := k.svc.Transition(ctx, c, kaservice.TransitionIn{ID: id, Status: want, Summary: "Synced from Syre"}); err != nil {
				return err
			}
		}
	}
	if it.Claim == nil && held {
		if cur, _ := k.svc.Get(kaservice.GetIn{ID: id}); cur != nil && cur.Claim != nil && cur.Claim.Authority == tracker.SyreAuthority {
			if _, err := k.svc.Release(ctx, c, id, "", false); err != nil && !kaservice.IsCode(err, kaservice.CodeNotClaimed) {
				return err
			}
		}
	}
	return nil
}

// StatusFromKa maps ka's default workflow statuses to Syre's ("" = unknown: leave it).
func StatusFromKa(s string) string {
	switch s {
	case "triage", "backlog":
		return store.TicketStatusBacklog
	case "ready", "todo":
		return store.TicketStatusReady
	case "in_progress", "started":
		return store.TicketStatusRunning
	case "blocked":
		return store.TicketStatusNeedsYou
	case "review", "in_review":
		return store.TicketStatusReview
	case "done":
		return store.TicketStatusDone
	case "canceled", "cancelled", "wontfix":
		return store.TicketStatusCanceled
	}
	return ""
}

// StatusToKa maps Syre's statuses to ka's default workflow ("" = don't change).
func StatusToKa(s string) string {
	switch s {
	case store.TicketStatusBacklog:
		return "backlog"
	case store.TicketStatusReady:
		return "ready"
	case store.TicketStatusRunning:
		return "in_progress"
	case store.TicketStatusNeedsYou, store.TicketStatusFailed:
		return "blocked"
	case store.TicketStatusReview:
		return "review"
	case store.TicketStatusDone:
		return "done"
	case store.TicketStatusCanceled:
		return "canceled"
	}
	return ""
}

func closedStatus(ka string) bool { return ka == "done" || ka == "canceled" }

func kindFromKa(k string) string {
	if k == "research" {
		return store.TicketKindSpike
	}
	return k
}

func kindToKa(k string) string {
	if k == store.TicketKindSpike {
		return "research"
	}
	return k
}
