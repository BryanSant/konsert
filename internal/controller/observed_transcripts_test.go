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

func TestObservedTranscriptFetchRelayAndPurge(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	fw := cs.connectWorker(t, testWorkerInfo("w1", "box"), nil, "w1")
	// An observed session the daemon reported.
	fw.send(protocol.MsgExternalSession, protocol.ExternalSessionPayload{ExternalID: "sess-1", Agent: "claude", Cwd: "/work", Status: "working", Source: "transcript"})
	var taskID string
	waitFor(t, 3*time.Second, "observed task", func() bool {
		ts, _ := cs.st.ListTasks(ctx, "", "w1", 10)
		for _, tk := range ts {
			if tk.ExternalID == "sess-1" {
				taskID = tk.ID
			}
		}
		return taskID != ""
	})

	served := make(chan struct{})
	go func() {
		defer close(served)
		env := fw.expect(protocol.MsgObservedTranscript, 3*time.Second)
		req, _ := protocol.DecodePayload[protocol.ObservedTranscriptRequest](env)
		if req.ExternalID != "sess-1" || !req.Watch {
			return
		}
		fw.sendWithID(protocol.MsgObservedTranscript, env.ID, protocol.ObservedTranscript{ExternalID: "sess-1", Available: true, Events: []protocol.AgentChatEvent{
			{Seq: 1, Kind: protocol.EventUserMessage, TurnID: "u1", MessageID: "u1", Text: "fix the build"},
			{Seq: 2, Kind: protocol.EventMessage, TurnID: "u1", MessageID: "a1", Text: "On it."},
		}})
	}()
	code, raw := cs.api(t, http.MethodGet, "/api/tasks/"+taskID+"/transcript", nil)
	<-served
	var res protocol.ObservedTranscript
	if code != http.StatusOK || json.Unmarshal(raw, &res) != nil || !res.Available || len(res.Events) != 2 {
		t.Fatalf("transcript: %d %s", code, raw)
	}

	// Live events from the daemon are appended (duplicates by seq dropped).
	fw.send(protocol.MsgObservedEvents, protocol.ObservedEvents{ExternalID: "sess-1", Events: []protocol.AgentChatEvent{
		{Seq: 2, Kind: protocol.EventMessage, Text: "dup"},
		{Seq: 3, Kind: protocol.EventToolCall, ToolCall: &protocol.AgentToolCall{ID: "t1", Name: "Bash", Status: "running"}},
	}})
	waitFor(t, 3*time.Second, "live event cached", func() bool {
		cs.h.transcripts.mu.Lock()
		defer cs.h.transcripts.mu.Unlock()
		e := cs.h.transcripts.byTask[taskID]
		return e != nil && len(e.res.Events) == 3
	})
	// Served from memory while fresh: no second request to the machine.
	code, raw = cs.api(t, http.MethodGet, "/api/tasks/"+taskID+"/transcript", nil)
	if code != http.StatusOK || !strings.Contains(string(raw), `"seq":3`) {
		t.Fatalf("cached: %d %s", code, raw)
	}
	if got := fw.drain(protocol.MsgObservedTranscript, 200*time.Millisecond); len(got) != 0 {
		t.Fatal("refetched while fresh")
	}

	// Not viewed for the TTL: purged. Nothing of it was ever stored.
	if n := cs.h.purgeTranscripts(ctx, time.Now().Add(defaultObservedTranscriptTTL+time.Minute)); n != 1 {
		t.Fatalf("purged %d", n)
	}
	evs, _ := cs.st.ListEvents(ctx, taskID, 0, 100)
	for _, ev := range evs {
		if strings.Contains(ev.Message, "fix the build") || strings.Contains(ev.Message, "On it.") {
			t.Fatalf("transcript content stored: %+v", ev)
		}
	}
	chatEvs, _ := cs.st.ListChatEvents(ctx, taskID, 0, 100)
	if len(chatEvs) != 0 {
		t.Fatalf("chat events stored: %d", len(chatEvs))
	}

	// A managed (non-observed) task has no observed transcript.
	if err := cs.st.CreateTask(ctx, store.Task{ID: "t-managed", Agent: "mock", Status: protocol.TaskStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if code, _ := cs.api(t, http.MethodGet, "/api/tasks/t-managed/transcript", nil); code != http.StatusBadRequest {
		t.Fatalf("managed: %d", code)
	}
}
