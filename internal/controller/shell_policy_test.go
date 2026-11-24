package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// endedWithShells finishes the shellHub task (status) and returns when its activity
// turned terminal, with the shells konsertd reported still listed.
func endedWithShells(t *testing.T, cs *chatServer, taskID, status string) time.Time {
	t.Helper()
	code := 0
	var exit *int
	if protocol.IsTerminalTaskStatus(status) {
		exit = &code
	}
	if _, err := cs.h.setTaskStatus(context.Background(), taskID, status, "ok", "", exit, store.Cause{Actor: store.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	a, _ := cs.h.sessionActivity("task:" + taskID)
	if !protocol.ActivityTerminal(a.State) || a.Shells != 2 {
		t.Fatalf("activity after %s: %+v", status, a)
	}
	return a.Since
}

func putShellPolicy(t *testing.T, cs *chatServer, p ShellPolicySettings) {
	t.Helper()
	if code, raw := cs.api(t, http.MethodPut, "/api/settings/shells", p); code != http.StatusOK {
		t.Fatalf("PUT /api/settings/shells: %d %s", code, raw)
	}
}

func TestShellPolicySettingsRoundTrip(t *testing.T) {
	cs := newChatServer(t)
	code, raw := cs.api(t, http.MethodGet, "/api/settings/shells", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), `"mode":"remind"`) || !strings.Contains(string(raw), `"idle_minutes":30`) {
		t.Fatalf("defaults: %d %s", code, raw)
	}
	putShellPolicy(t, cs, ShellPolicySettings{Mode: ShellPolicyAsk, IdleMinutes: 45, Projects: map[string]string{"p1": ShellPolicyStop, "p2": ""}})
	p := cs.h.shellPolicySettings(context.Background())
	if p.Mode != ShellPolicyAsk || p.IdleMinutes != 45 || p.modeFor("p1") != ShellPolicyStop || p.modeFor("p2") != ShellPolicyAsk || p.modeFor("") != ShellPolicyAsk {
		t.Fatalf("saved %+v", p)
	}
	// Dropping a project from the map returns it to the global mode.
	putShellPolicy(t, cs, ShellPolicySettings{Mode: ShellPolicyRemind, IdleMinutes: 30})
	if p := cs.h.shellPolicySettings(context.Background()); len(p.Projects) != 0 {
		t.Fatalf("project override kept: %+v", p)
	}
	for _, bad := range []ShellPolicySettings{{Mode: "nuke", IdleMinutes: 30}, {Mode: ShellPolicyStop, IdleMinutes: 0}, {Mode: ShellPolicyStop, IdleMinutes: 30, Projects: map[string]string{"p": "x"}}} {
		if code, _ := cs.api(t, http.MethodPut, "/api/settings/shells", bad); code != http.StatusBadRequest {
			t.Fatalf("%+v accepted: %d", bad, code)
		}
	}
}

// Remind (default): one Needs-you item per session after the idle period, Keep running
// snoozes it, and it goes when the shells do.
func TestShellPolicyRemindsOncePerSession(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	ctx := context.Background()
	since := endedWithShells(t, cs, taskID, protocol.TaskStatusCompleted)

	cs.h.shellPolicyTick(ctx, since.Add(29*time.Minute))
	if n := len(cs.h.ShellReminders()); n != 0 {
		t.Fatalf("%d reminders before the idle period", n)
	}
	cs.h.shellPolicyTick(ctx, since.Add(31*time.Minute))
	cs.h.shellPolicyTick(ctx, since.Add(32*time.Minute))
	rs := cs.h.ShellReminders()
	if len(rs) != 1 {
		t.Fatalf("reminders %+v", rs)
	}
	r := rs[0]
	if r.ID != "task:"+taskID || r.Shells != 2 || !strings.HasPrefix(r.Title, "2 shells left running by ") || r.CanAsk || r.Mode != ShellPolicyRemind {
		t.Fatalf("reminder %+v", r)
	}
	if kills := w.drain(protocol.MsgSessionShellKill, 100*time.Millisecond); len(kills) != 0 {
		t.Fatal("remind mode killed shells")
	}
	code, raw := cs.api(t, http.MethodGet, "/api/shells/reminders", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), "left running by") {
		t.Fatalf("GET reminders: %d %s", code, raw)
	}

	// Keep running: gone, and not back on the next ticks.
	if code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/keep", nil); code != http.StatusOK {
		t.Fatalf("keep: %d %s", code, raw)
	}
	cs.h.shellPolicyTick(ctx, time.Now().Add(time.Hour))
	if n := len(cs.h.ShellReminders()); n != 0 {
		t.Fatalf("%d reminders after Keep running", n)
	}
	// The snooze ends: it comes back.
	cs.h.shellPolicyTick(ctx, time.Now().Add(shellSnooze+time.Hour))
	if n := len(cs.h.ShellReminders()); n != 1 {
		t.Fatalf("%d reminders after the snooze", n)
	}
	// The shells exit: the item goes.
	w.send(protocol.MsgSessionActivity, protocol.SessionActivity{TaskID: taskID, State: protocol.ActivityDone, At: time.Now()})
	waitFor(t, 3*time.Second, "shells gone", func() bool {
		a, _ := cs.h.sessionActivity("task:" + taskID)
		return a.Shells == 0
	})
	cs.h.shellPolicyTick(ctx, time.Now().Add(shellSnooze+2*time.Hour))
	if n := len(cs.h.ShellReminders()); n != 0 {
		t.Fatalf("%d reminders after the shells exited", n)
	}
}

// Kill all from the reminder clears it right away.
func TestShellPolicyKillClearsReminder(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	since := endedWithShells(t, cs, taskID, protocol.TaskStatusCompleted)
	cs.h.shellPolicyTick(context.Background(), since.Add(31*time.Minute))
	if len(cs.h.ShellReminders()) != 1 {
		t.Fatal("no reminder")
	}
	if code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/kill", nil); code != http.StatusOK {
		t.Fatalf("kill: %d %s", code, raw)
	}
	w.expect(protocol.MsgSessionShellKill, 3*time.Second)
	if n := len(cs.h.ShellReminders()); n != 0 {
		t.Fatalf("%d reminders after Kill all", n)
	}
}

// Stop after idle kills (audit-logged, attributed to the policy), never when pinned.
func TestShellPolicyStopAfterIdle(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	ctx := context.Background()
	putShellPolicy(t, cs, ShellPolicySettings{Mode: ShellPolicyStop, IdleMinutes: 10})
	since := endedWithShells(t, cs, taskID, protocol.TaskStatusCompleted)

	// Pinned with Keep: no kill, a reminder that says why.
	if err := cs.st.SetTaskKeep(ctx, taskID, true); err != nil {
		t.Fatal(err)
	}
	cs.h.shellPolicyTick(ctx, since.Add(11*time.Minute))
	if kills := w.drain(protocol.MsgSessionShellKill, 150*time.Millisecond); len(kills) != 0 {
		t.Fatal("killed the shells of a pinned task")
	}
	rs := cs.h.ShellReminders()
	if len(rs) != 1 || rs[0].Mode != ShellPolicyStop || !strings.Contains(rs[0].Note, "Keep") {
		t.Fatalf("pinned reminder %+v", rs)
	}

	if err := cs.st.SetTaskKeep(ctx, taskID, false); err != nil {
		t.Fatal(err)
	}
	cs.h.shellPolicyTick(ctx, since.Add(5*time.Minute))
	if kills := w.drain(protocol.MsgSessionShellKill, 150*time.Millisecond); len(kills) != 0 {
		t.Fatal("killed before the idle period")
	}
	cs.h.shellPolicyTick(ctx, since.Add(12*time.Minute))
	kill, _ := protocol.DecodePayload[protocol.SessionShellKill](w.expect(protocol.MsgSessionShellKill, 3*time.Second))
	if kill.TaskID != taskID || len(kill.Shells) != 0 {
		t.Fatalf("kill %+v", kill)
	}
	if n := len(cs.h.ShellReminders()); n != 0 {
		t.Fatalf("%d reminders after the automatic kill", n)
	}
	// Not repeated on every tick while konsertd reports the kill.
	cs.h.shellPolicyTick(ctx, since.Add(13*time.Minute))
	if kills := w.drain(protocol.MsgSessionShellKill, 150*time.Millisecond); len(kills) != 0 {
		t.Fatal("killed again right away")
	}
	audit, err := cs.st.ListAudit(ctx, store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var msg string
	for _, e := range audit {
		if e.Action == AuditShellKill && e.TaskID == taskID {
			msg = e.Message
		}
	}
	if !strings.Contains(msg, "after 10 minutes idle (Stop after idle)") {
		t.Fatalf("audit message %q", msg)
	}
	evs, _ := cs.st.ListFleetEvents(ctx, store.FleetEventFilter{Limit: 50})
	found := false
	for _, e := range evs {
		if e.Action == "shell_kill" && e.Cause.Source == "shell_policy" && strings.HasPrefix(e.Summary, "Konsert killed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no timeline entry attributed to the policy: %+v", evs)
	}
}

// Ask the agent: one follow-up shortly after the session ends with shells left (once per
// set of shells); an ended task can't be asked, so it reminds instead.
func TestShellPolicyAsksTheAgentOnce(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	ctx := context.Background()
	putShellPolicy(t, cs, ShellPolicySettings{Mode: ShellPolicyAsk, IdleMinutes: 30})
	since := endedWithShells(t, cs, taskID, protocol.TaskStatusIdle)

	cs.h.shellPolicyTick(ctx, since.Add(30*time.Second))
	if steers := w.drain(protocol.MsgTaskSteer, 150*time.Millisecond); len(steers) != 0 {
		t.Fatal("asked within the grace period")
	}
	cs.h.shellPolicyTick(ctx, since.Add(2*time.Minute))
	steer, _ := protocol.DecodePayload[protocol.TaskSteerPayload](w.expect(protocol.MsgTaskSteer, 3*time.Second))
	if steer.TaskID != taskID || !strings.Contains(steer.InputText, "Please stop the 2 shell processes") {
		t.Fatalf("steer %+v", steer)
	}
	cs.h.shellPolicyTick(ctx, since.Add(3*time.Minute))
	if steers := w.drain(protocol.MsgTaskSteer, 150*time.Millisecond); len(steers) != 0 {
		t.Fatal("asked twice about the same shells")
	}
	// Still running after the idle period: the reminder follows.
	cs.h.shellPolicyTick(ctx, since.Add(31*time.Minute))
	rs := cs.h.ShellReminders()
	if len(rs) != 1 || !rs[0].CanAsk || !strings.Contains(rs[0].Note, "asked") {
		t.Fatalf("reminder after asking %+v", rs)
	}
}

func TestShellPolicyAskFallsBackForEndedTasks(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	ctx := context.Background()
	putShellPolicy(t, cs, ShellPolicySettings{Mode: ShellPolicyAsk, IdleMinutes: 30})
	since := endedWithShells(t, cs, taskID, protocol.TaskStatusCompleted)
	cs.h.shellPolicyTick(ctx, since.Add(2*time.Minute))
	if steers := w.drain(protocol.MsgTaskSteer, 150*time.Millisecond); len(steers) != 0 {
		t.Fatal("steered an ended task")
	}
	cs.h.shellPolicyTick(ctx, since.Add(31*time.Minute))
	rs := cs.h.ShellReminders()
	if len(rs) != 1 || rs[0].CanAsk || !strings.Contains(rs[0].Note, "can't be asked") {
		t.Fatalf("reminder %+v", rs)
	}
}

// A per-project mode wins over the global one.
func TestShellPolicyProjectOverride(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	ctx := context.Background()
	task, err := cs.st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	projectID := task.ProjectID
	if projectID == "" {
		projectID = cs.h.projectForPath(ctx, task.WorkerID, firstNonEmpty(task.EffectiveDir, task.Workdir))
	}
	if projectID == "" {
		t.Skip("the dispatched task has no project")
	}
	putShellPolicy(t, cs, ShellPolicySettings{Mode: ShellPolicyRemind, IdleMinutes: 30, Projects: map[string]string{projectID: ShellPolicyStop}})
	since := endedWithShells(t, cs, taskID, protocol.TaskStatusCompleted)
	cs.h.shellPolicyTick(ctx, since.Add(31*time.Minute))
	w.expect(protocol.MsgSessionShellKill, 3*time.Second)
}

func TestShellsTitleAndMinutes(t *testing.T) {
	if got := shellsTitle(1, "Fix login"); got != "1 shell left running by Fix login" {
		t.Fatal(got)
	}
	if got := shellsTitle(2, "Fix login"); got != "2 shells left running by Fix login" {
		t.Fatal(got)
	}
	for n, want := range map[int]string{1: "1 minute", 30: "30 minutes", 60: "1 hour", 120: "2 hours", 90: "90 minutes"} {
		if got := humanMinutes(n); got != want {
			t.Fatalf("humanMinutes(%d) = %q", n, got)
		}
	}
}
