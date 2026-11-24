package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/sshutil"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/coder/websocket"
)

// testClock is a settable clock for the removal retries.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) get() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func uninstallAudits(t *testing.T, st store.Store) []store.AuditEvent {
	t.Helper()
	ev, err := st.ListAudit(context.Background(), store.AuditFilter{Action: AuditWorkerUninstall})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(ev) // oldest first
	return ev
}

func auditMessages(ev []store.AuditEvent) string {
	var out []string
	for _, e := range ev {
		out = append(out, e.Message)
	}
	return strings.Join(out, "\n")
}

// Removing the hub's own machine runs `konsertd uninstall --worker <id>` locally and
// records what it did.
func TestRemoveLocalMachineRunsKonsertdUninstall(t *testing.T) {
	lt := newLaunchTest(t)
	if _, err := lt.st.EnsureWorker(context.Background(), store.Worker{ID: localID, Name: "hubhost", Launch: store.LaunchLocal}); err != nil {
		t.Fatal(err)
	}
	if code, _ := lt.do(t, http.MethodDelete, "/api/workers/"+localID); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	waitFor(t, 5*time.Second, "local uninstall", func() bool {
		return slices.Contains(lt.runner.subcommands(), "uninstall")
	})
	lt.runner.mu.Lock()
	var call []string
	for _, c := range lt.runner.calls {
		if c[1] == "uninstall" {
			call = c
		}
	}
	lt.runner.mu.Unlock()
	if !slices.Equal(call, []string{"/opt/konsert/bin/konsertd", "uninstall", "--worker", localID}) {
		t.Fatalf("uninstall call = %q", call)
	}
	waitFor(t, 5*time.Second, "audit", func() bool {
		ev := uninstallAudits(t, lt.st)
		return len(ev) == 1 && strings.HasPrefix(ev[0].Message, "Uninstalled konsertd from hubhost")
	})
}

// An unreachable ssh machine is retried with backoff until it works; a machine added
// again in the meantime is never uninstalled; after 30 days the hub gives up.
func TestRemoveSSHMachineRetriesWhenUnreachable(t *testing.T) {
	lt := newLaunchTest(t)
	clock := &testClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	lt.srv.removals.clock = clock.get
	var mu sync.Mutex
	var calls []string
	reachable := false
	lt.srv.launch.uninstallRemote = func(_ context.Context, target, _, workerID string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, target+" "+workerID)
		if !reachable {
			return "", fmt.Errorf("ssh %s: %w: connect timed out", target, sshutil.ErrUnreachable)
		}
		return "Removed the systemd user unit for box", nil
	}
	nCalls := func() int { mu.Lock(); defer mu.Unlock(); return len(calls) }
	lt.addSSHWorker(t, remoteID, "box", "me@box")
	if code, _ := lt.do(t, http.MethodDelete, "/api/workers/"+remoteID); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	waitFor(t, 5*time.Second, "first attempt", func() bool { return len(uninstallAudits(t, lt.st)) == 1 })
	if msg := uninstallAudits(t, lt.st)[0].Message; !strings.Contains(msg, "could not reach it") || !strings.Contains(msg, "retrying") {
		t.Fatalf("audit = %q", msg)
	}
	pending := lt.srv.loadRemovals(context.Background())
	if len(pending) != 1 || pending[0].WorkerID != remoteID || !pending[0].NextAt.Equal(clock.get().Add(2*time.Minute)) {
		t.Fatalf("pending = %+v", pending)
	}

	ctx := context.Background()
	lt.srv.retryRemovals(ctx) // not due yet
	if nCalls() != 1 {
		t.Fatalf("retried before it was due: %d calls", nCalls())
	}
	clock.add(3 * time.Minute)
	lt.srv.retryRemovals(ctx) // due, still unreachable: next delay 10 min, no new audit
	if nCalls() != 2 || len(uninstallAudits(t, lt.st)) != 1 {
		t.Fatalf("calls=%d audits=%q", nCalls(), auditMessages(uninstallAudits(t, lt.st)))
	}
	if p := lt.srv.loadRemovals(ctx); len(p) != 1 || p[0].Attempts != 2 || !p[0].NextAt.Equal(clock.get().Add(10*time.Minute)) {
		t.Fatalf("pending after 2nd attempt = %+v", p)
	}
	clock.add(11 * time.Minute)
	mu.Lock()
	reachable = true
	mu.Unlock()
	lt.srv.retryRemovals(ctx)
	if p := lt.srv.loadRemovals(ctx); len(p) != 0 {
		t.Fatalf("pending after success = %+v", p)
	}
	ev := uninstallAudits(t, lt.st)
	if len(ev) != 2 || ev[1].Message != "Uninstalled konsertd from box: Removed the systemd user unit for box" {
		t.Fatalf("audits = %q", auditMessages(ev))
	}

	// Gave up after 30 days.
	reachable = false
	lt.srv.putRemoval(ctx, pendingRemoval{WorkerID: otherID, Name: "old", Launch: store.LaunchSSH, SSHTarget: "me@old", RemovedAt: clock.get().Add(-31 * 24 * time.Hour)})
	lt.srv.retryRemovals(ctx)
	if p := lt.srv.loadRemovals(ctx); len(p) != 0 {
		t.Fatalf("expired removal kept: %+v", p)
	}
	if last := uninstallAudits(t, lt.st); !strings.Contains(last[len(last)-1].Message, "Gave up uninstalling konsertd from old after 30 days") {
		t.Fatalf("audits = %q", auditMessages(last))
	}
}

// A removed machine that is added again before the retry is not uninstalled.
func TestReaddedMachineIsNotUninstalled(t *testing.T) {
	lt := newLaunchTest(t)
	called := make(chan string, 4)
	lt.srv.launch.uninstallRemote = func(_ context.Context, target, _, _ string) (string, error) {
		called <- target
		return "", nil
	}
	ctx := context.Background()
	lt.srv.putRemoval(ctx, pendingRemoval{WorkerID: remoteID, Name: "box", Launch: store.LaunchSSH, SSHTarget: "me@box", RemovedAt: time.Now().UTC()})
	if _, err := lt.srv.recordSSHWorker(ctx, sshutil.BootstrapResult{WorkerID: remoteID, Name: "box", Target: "me@box"}); err != nil {
		t.Fatal(err)
	}
	if p := lt.srv.loadRemovals(ctx); len(p) != 0 {
		t.Fatalf("re-adding kept the pending removal: %+v", p)
	}
	// Even a stray attempt checks first that the machine is gone.
	lt.srv.attemptUninstall(ctx, pendingRemoval{WorkerID: remoteID, Launch: store.LaunchSSH, SSHTarget: "me@box"})
	select {
	case tgt := <-called:
		t.Fatalf("uninstalled a machine in use (%s)", tgt)
	default:
	}
}

// A manual machine removed while offline is told to uninstall itself (4001) when it
// reconnects with its old credential, exactly once; a wrong secret gets nothing.
func TestRemoveOfflineManualMachineUninstallsOnReconnect(t *testing.T) {
	srv, ts, _ := newMachineAuthServer(t)
	credA := testMachineCredential(t, srv, wA)
	a, _ := dialWorker(t, ts, credA, wA)
	waitFor(t, 3*time.Second, "A online", func() bool { return srv.hub.IsWorkerConnected(wA) })
	_ = a.conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, 3*time.Second, "A offline", func() bool { return !srv.hub.IsWorkerConnected(wA) })

	if code, body := apiCall(t, ts, http.MethodDelete, "/api/workers/"+wA, "", machineAdminTok); code != 200 {
		t.Fatalf("delete: %d %v", code, body)
	}
	pending := srv.loadRemovals(context.Background())
	if len(pending) != 1 || len(pending[0].Creds) != 1 || strings.Contains(pending[0].Creds[0].Hash, strings.Split(credA, "_")[len(strings.Split(credA, "_"))-1]) {
		t.Fatalf("pending = %+v", pending)
	}
	if ev := uninstallAudits(t, srv.store); len(ev) != 1 || !strings.Contains(ev[0].Message, "it uninstalls itself when it next connects") {
		t.Fatalf("audits = %q", auditMessages(ev))
	}

	id, _, _ := auth.ParseMachineCredential(credA)
	if _, code := dialWorker(t, ts, auth.FormatMachineCredential(id, strings.Repeat("y", 43)), wA); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret for a removed credential: %d", code)
	}

	back, code := dialWorker(t, ts, credA, wA)
	if code != 0 {
		t.Fatalf("removed machine's reconnect refused at upgrade: %d", code)
	}
	if st := back.waitClosed(t); st != websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
		t.Fatalf("close = %d, want 4001", st)
	}
	if srv.hub.IsWorkerConnected(wA) {
		t.Fatal("a removed machine registered again")
	}
	if _, err := srv.store.GetWorker(context.Background(), wA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removed machine recorded again: %v", err)
	}
	if p := srv.loadRemovals(context.Background()); len(p) != 0 {
		t.Fatalf("pending after reconnect = %+v", p)
	}
	if ev := uninstallAudits(t, srv.store); len(ev) != 2 || !strings.Contains(ev[1].Message, "told to uninstall itself") {
		t.Fatalf("audits = %q", auditMessages(ev))
	}
	if _, code := dialWorker(t, ts, credA, wA); code != http.StatusUnauthorized {
		t.Fatalf("second reconnect: %d, want 401", code)
	}
}

// A connected manual machine gets 4001 and removes itself; nothing stays pending.
func TestRemoveConnectedManualMachine(t *testing.T) {
	srv, ts, _ := newMachineAuthServer(t)
	credA := testMachineCredential(t, srv, wA)
	a, _ := dialWorker(t, ts, credA, wA)
	waitFor(t, 3*time.Second, "A online", func() bool { return srv.hub.IsWorkerConnected(wA) })
	if code, _ := apiCall(t, ts, http.MethodDelete, "/api/workers/"+wA, "", machineAdminTok); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if st := a.waitClosed(t); st != websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
		t.Fatalf("close = %d, want 4001", st)
	}
	if p := srv.loadRemovals(context.Background()); len(p) != 0 {
		t.Fatalf("pending = %+v", p)
	}
	if ev := uninstallAudits(t, srv.store); len(ev) != 1 || !strings.Contains(ev[0].Message, "uninstalls itself") {
		t.Fatalf("audits = %q", auditMessages(ev))
	}
}

func TestSummarizeUninstall(t *testing.T) {
	out := "Disabled konsertd.service\nRemoved /x/konsertd.service\nRemoved the systemd user unit for box\nKept the agent hooks: still used by host on this account\n"
	if got := summarizeUninstall(out); got != "Removed the systemd user unit for box; Kept the agent hooks: still used by host on this account" {
		t.Fatalf("summary = %q", got)
	}
	if got := summarizeUninstall("nothing useful"); got != "done" {
		t.Fatalf("summary = %q", got)
	}
}
