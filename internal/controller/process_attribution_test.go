package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func exitEvents(t *testing.T, st store.Store) []store.FleetEvent {
	t.Helper()
	evs, err := st.ListFleetEvents(context.Background(), store.FleetEventFilter{Kind: store.EventKindAction, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.FleetEvent
	for _, e := range evs {
		if e.Action == "process_exit" || e.Action == "process_kill" {
			out = append(out, e)
		}
	}
	return out
}

func TestProcessExitOverSSHIsTheUserLikely(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	upsertBox(t, th.st, "w1", "box")
	now := time.Now().UTC()
	exit := protocol.ProcessExitPayload{Name: "nginx", PID: 4242, After: now.Add(-5 * time.Second), Before: now,
		SSHShells: []protocol.SSHShell{{PID: 900, Name: "zsh", TTY: "/dev/pts/3", LastInput: now.Add(-3 * time.Second)}}}
	th.h.attributeProcessExit(ctx, "w1", exit)

	evs := exitEvents(t, th.st)
	if len(evs) != 1 || evs[0].Cause.Actor != store.ActorUser || evs[0].Cause.Source != store.SourceSSH ||
		evs[0].Cause.Confidence != store.ConfidenceLikely || !strings.Contains(evs[0].Summary, "likely stopped nginx (pid 4242) on box over SSH") {
		t.Fatalf("events: %+v", evs)
	}
	holds, _ := th.st.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, Limit: 10})
	if len(holds) != 1 || holds[0].Resource == nil || holds[0].Resource.Name != "nginx" || holds[0].WorkerID != "w1" {
		t.Fatalf("holds: %+v", holds)
	}
	// The same stop reported again (a restart and another stop) reuses the hold.
	th.h.attributeProcessExit(ctx, "w1", exit)
	if holds, _ = th.st.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, Limit: 10}); len(holds) != 1 {
		t.Fatalf("holds after a repeat: %d", len(holds))
	}
}

func TestProcessExitAttributionAlternatives(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	upsertBox(t, th.st, "w1", "box")
	now := time.Now().UTC()
	exit := func(name string, shells ...protocol.SSHShell) protocol.ProcessExitPayload {
		return protocol.ProcessExitPayload{Name: name, PID: 10, After: now.Add(-5 * time.Second), Before: now, SSHShells: shells}
	}
	stale := protocol.SSHShell{PID: 1, Name: "bash", LastInput: now.Add(-10 * time.Minute)}

	// An agent ran the stop: attributed to it, no hold, even with an SSH shell open.
	th.h.recent.addCommand(recentCommand{worker: "w1", chatID: "c1", chatSID: "c-agent", text: "sudo systemctl stop redis.service", at: now.Add(-20 * time.Second)})
	th.h.attributeProcessExit(ctx, "w1", exit("redis", protocol.SSHShell{PID: 2, LastInput: now}))
	// Nobody typed lately and no command named it: not attributed.
	th.h.attributeProcessExit(ctx, "w1", exit("backupd", stale))
	// A command that names a different process doesn't count.
	th.h.attributeProcessExit(ctx, "w1", exit("redis-sentinel"))

	evs := exitEvents(t, th.st)
	if len(evs) != 1 || evs[0].Cause.Actor != store.ActorAgent || evs[0].ChatID != "c1" || !strings.Contains(evs[0].Summary, "chat c-agent ran `sudo systemctl stop redis.service`") {
		t.Fatalf("events: %+v", evs)
	}
	if holds, _ := th.st.ListHolds(ctx, store.HoldFilter{ActiveOnly: true, Limit: 10}); len(holds) != 0 {
		t.Fatalf("holds: %+v", holds)
	}

	// A Syre terminal stop was already attributed when the command ended.
	th.h.recordFleetEvent(ctx, store.FleetEvent{Kind: store.EventKindAction, Action: "command", WorkerID: "w1",
		Summary: "The user stopped postgres", MetaJSON: auditMeta(map[string]any{"command": "pg_ctl stop -D /srv/pg"}), CreatedAt: now.Add(-2 * time.Second)})
	th.h.recordFleetEvent(ctx, store.FleetEvent{Kind: store.EventKindAction, Action: "command", WorkerID: "w1",
		Summary: "The user stopped pg_ctl", MetaJSON: auditMeta(map[string]any{"command": "systemctl stop postgres"}), CreatedAt: now.Add(-2 * time.Second)})
	th.h.attributeProcessExit(ctx, "w1", exit("postgres", protocol.SSHShell{PID: 3, LastInput: now}))
	if evs = exitEvents(t, th.st); len(evs) != 1 {
		t.Fatalf("a terminal stop was attributed twice: %+v", evs)
	}
}

func TestNamesProcess(t *testing.T) {
	for _, c := range []struct {
		cmd, name string
		want      bool
	}{
		{"sudo systemctl stop nginx", "nginx", true},
		{"systemctl restart nginx.service", "nginx", true},
		{"pkill -f /usr/sbin/nginx", "nginx", true},
		{"kill $(pgrep nginx)", "nginx", true},
		{"tail -f /var/log/nginx-access.log", "nginx", false},
		{"brew services stop postgresql@16", "postgresql", true},
	} {
		if got := namesProcess(c.cmd, c.name); got != c.want {
			t.Errorf("%q names %q = %v, want %v", c.cmd, c.name, got, c.want)
		}
	}
}

func TestKillProcessFromUI(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	fw := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		env := fw.expect(protocol.MsgProcessKill, 3*time.Second)
		p, _ := protocol.DecodePayload[protocol.ProcessKillPayload](env)
		if p.PID != 4242 {
			return
		}
		fw.sendWithID(protocol.MsgProcessKillResult, env.ID, protocol.ProcessKillResult{Name: "nginx", Killed: true})
	}()
	code, raw := cs.api(t, http.MethodPost, "/api/workers/w1/processes/kill", map[string]any{"pid": 4242, "started_at": time.Now().UTC()})
	<-done
	if code != http.StatusOK || !strings.Contains(string(raw), `"killed":true`) {
		t.Fatalf("kill: %d %s", code, raw)
	}
	evs := exitEvents(t, cs.st)
	if len(evs) != 1 || evs[0].Action != "process_kill" || !strings.Contains(evs[0].Summary, "nginx (pid 4242)") {
		t.Fatalf("events: %+v", evs)
	}
	if a, _ := cs.st.ListAudit(ctx, store.AuditFilter{Action: AuditProcessKill, Limit: 5}); len(a) != 1 {
		t.Fatalf("audit: %+v", a)
	}
	// The exit the daemon then reports is the kill, not a new attribution.
	cs.h.attributeProcessExit(ctx, "w1", protocol.ProcessExitPayload{Name: "nginx", PID: 4242, After: time.Now().Add(-time.Second), Before: time.Now(),
		SSHShells: []protocol.SSHShell{{PID: 1, LastInput: time.Now()}}})
	if evs = exitEvents(t, cs.st); len(evs) != 1 {
		t.Fatalf("the killed process was attributed again: %+v", evs)
	}
}

// The payload a daemon on bocabox reported live: a stand-in nginx (parent PID 1)
// stopped with `pkill -x nginx` typed in a plain interactive SSH session.
const liveSSHExit = `{"name":"nginx","pid":3160033,"started_at":"2026-11-27T20:44:11.42-05:00","after":"2026-11-27T20:45:28.532078552-05:00","before":"2026-11-27T20:45:33.542445313-05:00","ssh_shells":[{"pid":3165391,"name":"zsh","tty":"/dev/pts/1","last_input":"2026-11-28T01:45:29.317018297Z"}]}`

func TestLiveSSHStopIsTheUserLikely(t *testing.T) {
	th := newTestHub(t)
	upsertBox(t, th.st, "w1", "bocabox")
	var p protocol.ProcessExitPayload
	if err := json.Unmarshal([]byte(liveSSHExit), &p); err != nil {
		t.Fatal(err)
	}
	th.h.attributeProcessExit(context.Background(), "w1", p)
	evs := exitEvents(t, th.st)
	if len(evs) != 1 || evs[0].Cause.Actor != store.ActorUser || evs[0].Cause.Confidence != store.ConfidenceLikely ||
		!strings.Contains(evs[0].Summary, "nginx (pid 3160033) on bocabox over SSH (/dev/pts/1 had input") {
		t.Fatalf("events: %+v", evs)
	}
}
