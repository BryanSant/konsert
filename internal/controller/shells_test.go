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

var shellStart = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func shellList() []protocol.ShellInfo {
	return []protocol.ShellInfo{
		{PID: 4100, Command: "cd /repo && npm run dev", StartedAt: shellStart, Busy: true, Since: shellStart},
		{PID: 4200, Command: "tsc --watch", StartedAt: shellStart.Add(time.Minute), Since: shellStart},
	}
}

func TestActivityShellsAndSubagents(t *testing.T) {
	h := &Hub{}
	base := time.Now().UTC().Add(-time.Minute)
	a := protocol.SessionActivity{TaskID: "t1", State: protocol.ActivityThinking, At: base,
		Shells: 2, ShellsBusy: 1, ShellList: shellList(),
		Subagents: []protocol.SubagentActivity{{ID: "A", Title: "Docs", State: protocol.ActivityWorking, Label: "Run: make"}}}
	h.setActivity(a)
	got, _ := h.sessionActivity("task:t1")
	if got.Shells != 2 || got.ShellList[0].Summary != "starting the dev server" {
		t.Fatalf("shells %+v", got.ShellList)
	}
	if got.ShellList[0].Summary == "" || got.ShellList[1].Summary == "" {
		t.Fatalf("no shell summaries: %+v", got.ShellList)
	}
	// Only the shell count changes: still a change.
	a.At = base.Add(time.Second)
	a.Shells, a.ShellsBusy, a.ShellList = 1, 1, shellList()[:1]
	h.setActivity(a)
	if got, _ := h.sessionActivity("task:t1"); got.Shells != 1 || len(got.ShellList) != 1 {
		t.Fatalf("count change dropped: %+v", got)
	}
	// The task ends (hub-derived done): the shells syred reported stay; sub-agents go.
	h.taskStatusActivity(store.Task{ID: "t1", Status: protocol.TaskStatusCompleted})
	got, _ = h.sessionActivity("task:t1")
	if got.State != protocol.ActivityDone || got.Shells != 1 || len(got.ShellList) != 1 || len(got.Subagents) != 0 {
		t.Fatalf("done: %+v", got)
	}
}

// shellHub sets up a hub with a worker hosting a running task that reports two shells.
func shellHub(t *testing.T) (*chatServer, *fakeWorker, *fakeWorker, string) {
	t.Helper()
	cs := newChatServer(t)
	w := cs.connectWorker(t, testWorkerInfo("w-1", "box"), nil, "w-1")
	browser := cs.dialBrowser(t)
	res, err := cs.svc.DispatchTask(context.Background(), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w-1"}, Agent: "mock", RepoPath: "/repo", Prompt: "build it"})
	if err != nil {
		t.Fatal(err)
	}
	w.expect(protocol.MsgTaskStart, 3*time.Second)
	if _, err := cs.h.setTaskStatus(context.Background(), res.TaskID, protocol.TaskStatusRunning, "", "", nil, store.Cause{Actor: store.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	w.send(protocol.MsgSessionActivity, protocol.SessionActivity{TaskID: res.TaskID, State: protocol.ActivityThinking, At: time.Now(),
		Shells: 2, ShellsBusy: 1, ShellList: shellList()})
	waitFor(t, 3*time.Second, "shells reported", func() bool {
		a, _ := cs.h.sessionActivity("task:" + res.TaskID)
		return a.Shells == 2
	})
	return cs, w, browser, res.TaskID
}

func TestKillShellsVerifiesAndAudits(t *testing.T) {
	cs, w, _, taskID := shellHub(t)

	// A pid the session doesn't list (or a stale start time) is refused.
	code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/kill", ShellAction{Shells: []protocol.ShellIdent{{PID: 4100, StartedAt: shellStart.Add(time.Hour)}}})
	if code != http.StatusConflict {
		t.Fatalf("stale ident: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/kill", ShellAction{Shells: []protocol.ShellIdent{{PID: 4100, StartedAt: shellStart}}})
	if code != http.StatusOK {
		t.Fatalf("kill: %d %s", code, raw)
	}
	kill, _ := protocol.DecodePayload[protocol.SessionShellKill](w.expect(protocol.MsgSessionShellKill, 3*time.Second))
	if kill.TaskID != taskID || len(kill.Shells) != 1 || kill.Shells[0].PID != 4100 || !kill.Shells[0].StartedAt.Equal(shellStart) {
		t.Fatalf("kill message %+v", kill)
	}
	// Clean up all: no list, the worker kills every shell of the session.
	code, raw = cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/kill", nil)
	var res ShellActionResult
	_ = json.Unmarshal(raw, &res)
	if code != http.StatusOK || res.Shells != 2 {
		t.Fatalf("kill all: %d %s", code, raw)
	}
	if all, _ := protocol.DecodePayload[protocol.SessionShellKill](w.expect(protocol.MsgSessionShellKill, 3*time.Second)); all.TaskID != taskID || len(all.Shells) != 0 {
		t.Fatalf("kill all message %+v", all)
	}
	audit, err := cs.st.ListAudit(context.Background(), store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	kills := 0
	for _, e := range audit {
		if e.Action == AuditShellKill && e.TaskID == taskID {
			kills++
		}
	}
	if kills != 2 {
		t.Fatalf("%d shell kill audit events", kills)
	}
	evs, _ := cs.st.ListFleetEvents(context.Background(), store.FleetEventFilter{Limit: 50})
	var line string
	for _, e := range evs {
		if e.Action == "shell_kill" && strings.Contains(e.Summary, "npm run dev") {
			line = e.Summary
		}
	}
	if line == "" {
		t.Fatalf("no timeline entry naming the shell: %+v", evs)
	}
}

func TestAskStopShellsTask(t *testing.T) {
	cs, w, _, taskID := shellHub(t)
	code, raw := cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/ask", ShellAction{Shells: []protocol.ShellIdent{{PID: 4200, StartedAt: shellStart.Add(time.Minute)}}})
	if code != http.StatusOK || !strings.Contains(string(raw), `"delivery":"now"`) {
		t.Fatalf("ask: %d %s", code, raw)
	}
	steer, _ := protocol.DecodePayload[protocol.TaskSteerPayload](w.expect(protocol.MsgTaskSteer, 3*time.Second))
	if steer.TaskID != taskID || !strings.Contains(steer.InputText, "tsc --watch") || !strings.Contains(steer.InputText, "4200") || !steer.AppendNewline {
		t.Fatalf("steer %+v", steer)
	}
	// After the task ended its agent can't be asked; killing still works.
	code0 := 0
	if _, err := cs.h.setTaskStatus(context.Background(), taskID, protocol.TaskStatusCompleted, "ok", "", &code0, store.Cause{Actor: store.ActorAgent}); err != nil {
		t.Fatal(err)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/ask", nil)
	if code != http.StatusConflict || !strings.Contains(string(raw), "kill") {
		t.Fatalf("ask after end: %d %s", code, raw)
	}
	if code, raw = cs.api(t, http.MethodPost, "/api/tasks/"+taskID+"/shells/kill", nil); code != http.StatusOK {
		t.Fatalf("kill dangling: %d %s", code, raw)
	}
}

func TestAskStopShellsChatQueuesDuringATurn(t *testing.T) {
	cs := newChatServer(t)
	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	chat, _, turn := openChat(t, cs, w, b, map[string]any{"title": "dev"})
	w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chat.ID, Seq: 1, Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking, TurnID: turn})
	w.send(protocol.MsgSessionActivity, protocol.SessionActivity{ChatID: chat.ID, State: protocol.ActivityThinking, At: time.Now(), Shells: 1, ShellsBusy: 1, ShellList: shellList()[:1]})
	waitFor(t, 3*time.Second, "chat busy with a shell", func() bool {
		a, _ := cs.h.sessionActivity("chat:" + chat.ID)
		return a.Shells == 1 && mustSession(t, cs.st, chat.ID).Status == protocol.ChatStatusWorking
	})
	code, raw := cs.api(t, http.MethodPost, "/api/chats/"+chat.ID+"/shells/ask", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), `"delivery":"after_turn"`) {
		t.Fatalf("ask mid-turn: %d %s", code, raw)
	}
	if extra := w.drain(protocol.MsgAgentChatPrompt, 200*time.Millisecond); len(extra) != 0 {
		t.Fatal("prompted mid-turn")
	}
	// The turn ends: the queued request goes out as the next prompt.
	w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chat.ID, Seq: 2, Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn})
	p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	if !strings.Contains(p.Text, "npm run dev") || !strings.Contains(p.Text, "pid 4100") {
		t.Fatalf("follow-up prompt %q", p.Text)
	}
	// Idle now: sent at once.
	w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: chat.ID, Seq: 3, Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: p.TurnID})
	waitFor(t, 3*time.Second, "chat done", func() bool { return mustSession(t, cs.st, chat.ID).Status == protocol.ChatStatusDone })
	code, raw = cs.api(t, http.MethodPost, "/api/chats/"+chat.ID+"/shells/ask", ShellAction{Shells: []protocol.ShellIdent{{PID: 4100, StartedAt: shellStart}}})
	if code != http.StatusOK || !strings.Contains(string(raw), `"delivery":"now"`) {
		t.Fatalf("ask idle: %d %s", code, raw)
	}
	if p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second)); !strings.Contains(p.Text, "Please stop") {
		t.Fatalf("idle prompt %q", p.Text)
	}
	// No shells: nothing to ask about.
	if code, _ := cs.api(t, http.MethodPost, "/api/chats/"+chat.ID+"/shells/ask", ShellAction{Shells: []protocol.ShellIdent{{PID: 1, StartedAt: shellStart}}}); code != http.StatusConflict {
		t.Fatalf("unknown shell: %d", code)
	}
}
