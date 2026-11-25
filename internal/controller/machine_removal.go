package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/auth"
	"github.com/BryanSant/syre/internal/sshutil"
	"github.com/BryanSant/syre/internal/store"
)

// Removing a machine uninstalls syred from it (SYR-AV0YT). Closing its link with
// 4001 only stops the daemon, and the OS service (launchd KeepAlive, systemd
// Restart=always, the Windows task's every-minute trigger) would start it again. So:
//
//   - A connected syred uninstalls itself when it gets 4001 (its service, the
//     hub's key grant, and the agent hooks if it was the account's last syred).
//   - The hub also runs `syred uninstall --worker <id>` itself, over the user's SSH
//     for an ssh machine and directly for its own machine (idempotent, so after a
//     self-removal it only confirms nothing is left), and records the result in the
//     audit log (worker.uninstall).
//   - An ssh machine the hub cannot reach is retried with backoff for 30 days.
//   - A manual (outbound) machine that was offline is told on its next connection: its
//     removed credential is remembered, and a connection presenting it gets 4001,
//     which makes it uninstall itself.
//
// Pending removals are kept in the settings table (machineRemovalsSetting).

// AuditWorkerUninstall records the outcome of uninstalling syred from a removed
// machine.
const AuditWorkerUninstall = "worker.uninstall"

const machineRemovalsSetting = "machine_removals"

// removalExpiry is how long the hub keeps trying to uninstall a removed machine.
const removalExpiry = 30 * 24 * time.Hour

// removalSettle is how long the hub waits after closing a connected daemon's link
// before running its own uninstall, so the daemon's self-removal usually goes first.
var removalSettle = 3 * time.Second

// removalRetryDelays is the backoff between attempts on an unreachable ssh machine; the
// last delay repeats.
var removalRetryDelays = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 6 * time.Hour}

// removalCheckEvery is how often the hub looks for removals due a retry.
var removalCheckEvery = time.Minute

// pendingRemoval is a removed machine syred is not yet uninstalled from.
type pendingRemoval struct {
	WorkerID  string        `json:"worker_id"`
	WorkerSID string        `json:"worker_sid,omitempty"`
	Name      string        `json:"name,omitempty"`
	Launch    string        `json:"launch,omitempty"`
	SSHTarget string        `json:"ssh_target,omitempty"`
	Creds     []removedCred `json:"creds,omitempty"`
	RemovedAt time.Time     `json:"removed_at"`
	NextAt    time.Time     `json:"next_at,omitzero"`
	Attempts  int           `json:"attempts,omitzero"`
	LastError string        `json:"last_error,omitempty"`
}

// removedCred is a removed manual machine's credential: its id and secret hash, so
// the hub can recognize (and only then answer with 4001) the machine reconnecting.
type removedCred struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// removals holds the pending removals.
type removals struct {
	mu    sync.Mutex
	clock func() time.Time
}

func (s *Server) loadRemovals(ctx context.Context) []pendingRemoval {
	kv, err := s.store.GetAllSettings(ctx)
	if err != nil || kv[machineRemovalsSetting] == "" {
		return nil
	}
	var out []pendingRemoval
	if err := json.Unmarshal([]byte(kv[machineRemovalsSetting]), &out); err != nil {
		s.logger.Warn("unreadable pending machine removals; dropping them", "err", err)
		return nil
	}
	return out
}

func (s *Server) saveRemovals(ctx context.Context, list []pendingRemoval) {
	raw := ""
	if len(list) > 0 {
		b, err := json.Marshal(list)
		if err != nil {
			s.logger.Warn("save pending machine removals failed", "err", err)
			return
		}
		raw = string(b)
	}
	if err := s.store.SetSettings(context.WithoutCancel(ctx), map[string]string{machineRemovalsSetting: raw}); err != nil {
		s.logger.Warn("save pending machine removals failed", "err", err)
	}
}

// updateRemovals applies fn to the pending list under the lock and saves the result.
func (s *Server) updateRemovals(ctx context.Context, fn func([]pendingRemoval) []pendingRemoval) {
	s.removals.mu.Lock()
	defer s.removals.mu.Unlock()
	list := s.loadRemovals(ctx)
	before, _ := json.Marshal(list)
	list = fn(slices.Clone(list))
	if after, _ := json.Marshal(list); string(after) == string(before) {
		return
	}
	s.saveRemovals(ctx, list)
}

// forgetRemoval drops workerID's pending removal (it was uninstalled, or added again).
func (s *Server) forgetRemoval(ctx context.Context, workerID string) {
	s.updateRemovals(ctx, func(list []pendingRemoval) []pendingRemoval {
		out := list[:0]
		for _, p := range list {
			if p.WorkerID != workerID {
				out = append(out, p)
			}
		}
		return out
	})
}

func (s *Server) putRemoval(ctx context.Context, rec pendingRemoval) {
	s.updateRemovals(ctx, func(list []pendingRemoval) []pendingRemoval {
		out := list[:0]
		for _, p := range list {
			if p.WorkerID != rec.WorkerID {
				out = append(out, p)
			}
		}
		return append(out, rec)
	})
}

// removedCreds snapshots a manual machine's credentials before its record (and them)
// is deleted.
func (s *Server) removedCreds(ctx context.Context, workerID string) []removedCred {
	list, err := s.store.ListMachineCredentials(ctx, workerID)
	if err != nil {
		return nil
	}
	var out []removedCred
	for _, c := range list {
		if c.RevokedAt != nil {
			continue
		}
		if _, hash, err := s.store.GetMachineCredential(ctx, c.ID); err == nil && hash != "" {
			out = append(out, removedCred{ID: c.ID, Hash: hash})
		}
	}
	return out
}

// uninstallRemovedMachine starts uninstalling syred from w, just removed. connected
// says whether its daemon was connected (and so got 4001 and removes itself too).
func (s *Server) uninstallRemovedMachine(ctx context.Context, w store.Worker, connected bool, creds []removedCred) {
	rec := pendingRemoval{
		WorkerID: w.ID, WorkerSID: w.SID, Name: w.Name, Launch: w.Launch, SSHTarget: w.SSHTarget,
		RemovedAt: s.removals.clock(),
	}
	actor := auditActorFrom(ctx)
	actx := withAuditActor(context.WithoutCancel(ctx), actor.User, actor.Source)
	switch w.Launch {
	case store.LaunchLocal, store.LaunchSSH:
		s.hub.goBackground(func(hctx context.Context) {
			if connected {
				select {
				case <-hctx.Done():
					return
				case <-time.After(removalSettle):
				}
			}
			s.attemptUninstall(withAuditActor(hctx, actor.User, actor.Source), rec)
		})
	default:
		if connected {
			s.auditUninstall(actx, rec, true, "syred on "+machineLabel(rec)+" uninstalls itself (the hub closed its connection with code 4001)", "")
			return
		}
		if len(creds) == 0 {
			s.auditUninstall(actx, rec, false, "syred on "+machineLabel(rec)+" was not connected and has no credential to recognize it by; uninstall it there with `syred uninstall`", "")
			return
		}
		rec.Creds = creds
		s.putRemoval(actx, rec)
		s.auditUninstall(actx, rec, false, "syred on "+machineLabel(rec)+" is offline; it uninstalls itself when it next connects", "")
	}
}

func machineLabel(rec pendingRemoval) string {
	if rec.Name != "" {
		return rec.Name
	}
	if rec.SSHTarget != "" {
		return rec.SSHTarget
	}
	return rec.WorkerID
}

// attemptUninstall runs one uninstall of a removed local or ssh machine, and records
// the outcome or schedules a retry.
func (s *Server) attemptUninstall(ctx context.Context, rec pendingRemoval) {
	if _, err := s.store.GetWorker(ctx, rec.WorkerID); !errors.Is(err, store.ErrNotFound) {
		// Added again since (or the store failed): never uninstall a machine in use.
		if err == nil {
			s.forgetRemoval(ctx, rec.WorkerID)
		}
		return
	}
	out, err := s.runUninstall(ctx, rec)
	rec.Attempts++
	if err == nil {
		s.forgetRemoval(ctx, rec.WorkerID)
		msg := "Uninstalled syred from " + machineLabel(rec)
		switch sum := summarizeUninstall(out); {
		case strings.Contains(out, "Nothing installed"):
			// The daemon got 4001 and removed itself first; the hub's run confirms it.
			msg = "syred on " + machineLabel(rec) + " uninstalled itself; nothing of it is left"
		case !strings.Contains(out, "Removed the ") && strings.Contains(out, "Removed /"):
			// Only state the stopping daemon wrote on its way out.
			msg = "syred on " + machineLabel(rec) + " was already uninstalled; removed its leftover state"
		case out != "":
			msg += ": " + sum
		}
		s.auditUninstall(ctx, rec, true, msg, out)
		return
	}
	now := s.removals.clock()
	if rec.Launch == store.LaunchSSH && now.Sub(rec.RemovedAt) < removalExpiry {
		delay := removalRetryDelays[min(rec.Attempts-1, len(removalRetryDelays)-1)]
		rec.NextAt = now.Add(delay)
		first := rec.LastError == ""
		rec.LastError = err.Error()
		s.putRemoval(ctx, rec)
		if first {
			why := "could not uninstall"
			if errors.Is(err, sshutil.ErrUnreachable) {
				why = "could not reach it to uninstall"
			}
			s.auditUninstall(ctx, rec, false, fmt.Sprintf("Removed %s, but %s syred (%v); retrying for 30 days", machineLabel(rec), why, err), out)
		}
		return
	}
	s.forgetRemoval(ctx, rec.WorkerID)
	msg := fmt.Sprintf("Could not uninstall syred from %s: %v", machineLabel(rec), err)
	if rec.Launch == store.LaunchSSH {
		msg = fmt.Sprintf("Gave up uninstalling syred from %s after 30 days: %v", machineLabel(rec), err)
	}
	s.auditUninstall(ctx, rec, false, msg, out)
}

// runUninstall runs `syred uninstall --worker <id>` for rec: over the user's SSH for
// an ssh machine, locally for the hub's own.
func (s *Server) runUninstall(ctx context.Context, rec pendingRemoval) (string, error) {
	l := s.launch
	switch rec.Launch {
	case store.LaunchSSH:
		hubID, err := s.hubID(ctx)
		if err != nil {
			return "", err
		}
		return l.uninstallRemote(ctx, rec.SSHTarget, hubID, rec.WorkerID)
	case store.LaunchLocal:
		if l.dryRun {
			return "nothing to uninstall (" + InstallDryRunEnv + ": nothing was registered)", nil
		}
		exe := l.findSyred()
		if exe == "" {
			return "", errors.New("syred not found on this machine")
		}
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		out, err := l.runner.Run(rctx, "", exe, "uninstall", "--worker", rec.WorkerID)
		return strings.TrimSpace(out), err
	}
	return "", fmt.Errorf("machine %s is not hub-installed", rec.WorkerID)
}

// summarizeUninstall keeps the report lines of `syred uninstall` output ("Removed
// ...", "Kept ...", "Nothing installed ...") for the audit message.
func summarizeUninstall(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "Removed the ") || strings.HasPrefix(l, "Kept ") || strings.HasPrefix(l, "Nothing installed") || strings.HasPrefix(l, "syred is not installed") {
			keep = append(keep, strings.TrimSuffix(l, "."))
		}
	}
	if len(keep) == 0 {
		return "done"
	}
	return strings.Join(keep, "; ")
}

func (s *Server) auditUninstall(ctx context.Context, rec pendingRemoval, ok bool, msg, output string) {
	meta := map[string]any{"name": rec.Name, "launch": rec.Launch, "ok": ok}
	if rec.SSHTarget != "" {
		meta["target"] = rec.SSHTarget
	}
	if output != "" {
		meta["output"] = output
	}
	if rec.Attempts > 0 {
		meta["attempts"] = rec.Attempts
	}
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action:    AuditWorkerUninstall,
		WorkerID:  rec.WorkerID,
		WorkerSID: rec.WorkerSID,
		Message:   msg,
		MetaJSON:  auditMeta(meta),
	})
}

// retryRemovals runs the due ssh removals and expires old manual ones.
func (s *Server) retryRemovals(ctx context.Context) {
	now := s.removals.clock()
	var due []pendingRemoval
	var expired []pendingRemoval
	s.updateRemovals(ctx, func(list []pendingRemoval) []pendingRemoval {
		out := list[:0]
		for _, p := range list {
			switch {
			case p.Launch != store.LaunchSSH && now.Sub(p.RemovedAt) >= removalExpiry:
				expired = append(expired, p)
				continue
			case p.Launch == store.LaunchSSH && !now.Before(p.NextAt):
				due = append(due, p)
			}
			out = append(out, p)
		}
		return out
	})
	for _, p := range expired {
		s.auditUninstall(ctx, p, false, "Gave up waiting for syred on "+machineLabel(p)+" to reconnect and uninstall itself (30 days)", "")
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return
		}
		s.attemptUninstall(ctx, p)
	}
}

// removalLoop retries pending removals until the hub stops.
func (s *Server) removalLoop(ctx context.Context) {
	ctx = withAuditActor(ctx, "system", "")
	t := time.NewTicker(removalCheckEvery)
	defer t.Stop()
	for {
		s.retryRemovals(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// removedMachineCred matches a credential the hub no longer has against the removed
// manual machines: the worker id of the machine it belonged to, if any.
func (s *Server) removedMachineCred(ctx context.Context, credID, secret string) (pendingRemoval, bool) {
	s.removals.mu.Lock()
	defer s.removals.mu.Unlock()
	for _, p := range s.loadRemovals(ctx) {
		for _, c := range p.Creds {
			if c.ID == credID && auth.VerifyMachineSecret(secret, c.Hash) {
				return p, true
			}
		}
	}
	return pendingRemoval{}, false
}
