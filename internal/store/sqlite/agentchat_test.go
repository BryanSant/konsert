package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

func TestAgentChatSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cs := store.ChatSession{ID: "c1", Title: "Chat", WorkerID: "w1", Agent: "claude", AccountID: "claude2",
		Model: "opus", Effort: "high", ApprovalMode: "default", Cwd: "~/src", FleetTools: true, MCPTokenHash: "abc"}
	if err := st.CreateChatSession(ctx, cs); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetChatSession(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != store.ChatBackendAgent || got.AccountID != "claude2" || !got.FleetTools || got.MCPTokenHash != "abc" || got.Cwd != "~/src" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	got.Status = protocol.ChatStatusWorking
	got.AgentSessionID = "sess-9"
	got.LastErrorJSON = `{"kind":"exit"}`
	if err := st.UpdateChatSession(ctx, got); err != nil {
		t.Fatal(err)
	}
	byWorker, err := st.ListChatSessionsByWorker(ctx, "w1")
	if err != nil || len(byWorker) != 1 || byWorker[0].AgentSessionID != "sess-9" || byWorker[0].Status != protocol.ChatStatusWorking {
		t.Fatalf("ListChatSessionsByWorker = %+v, %v", byWorker, err)
	}
	if err := st.UpdateChatSession(ctx, store.ChatSession{ID: "missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update missing = %v, want ErrNotFound", err)
	}
}

func appendEv(t *testing.T, st *SQLiteStore, seq int64, turn string, ev protocol.AgentChatEvent) {
	t.Helper()
	ev.ChatID, ev.Seq, ev.TurnID = "c1", seq, turn
	b, _ := json.Marshal(ev)
	ok, err := st.AppendChatEvent(context.Background(), store.ChatEvent{ChatID: "c1", Seq: seq, TurnID: turn, Kind: ev.Kind, JSON: string(b)})
	if err != nil || !ok {
		t.Fatalf("append seq %d: ok=%v err=%v", seq, ok, err)
	}
}

func TestChatEventsPersistAndCoalesce(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if err := st.CreateChatSession(ctx, store.ChatSession{ID: "c1", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	// Turn 1 streams deltas and then sends the complete message.
	appendEv(t, st, 1, "t1", protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, Text: "hm"})
	appendEv(t, st, 2, "t1", protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, Text: "m"})
	appendEv(t, st, 3, "t1", protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "Hel"})
	appendEv(t, st, 4, "t1", protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "lo"})
	appendEv(t, st, 5, "t1", protocol.AgentChatEvent{Kind: protocol.EventMessage, Text: "Hello"})
	appendEv(t, st, 6, "t1", protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, StopReason: "end_turn"})
	// Turn 2 has only deltas.
	appendEv(t, st, 7, "t2", protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "a"})
	appendEv(t, st, 8, "t2", protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, Text: "b"})

	// Duplicate seq is ignored.
	if ok, err := st.AppendChatEvent(ctx, store.ChatEvent{ChatID: "c1", Seq: 8, Kind: "x", JSON: "{}"}); ok || err != nil {
		t.Fatalf("duplicate append ok=%v err=%v", ok, err)
	}
	if n, _ := st.MaxChatEventSeq(ctx, "c1"); n != 8 {
		t.Fatalf("max seq = %d", n)
	}

	for _, turn := range []string{"t1", "t2"} {
		if err := st.CoalesceChatTurn(ctx, "c1", turn); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := st.ListChatEvents(ctx, "c1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []string{protocol.EventThoughtDelta, protocol.EventMessage, protocol.EventTurnEnd, protocol.EventMessage}
	if len(kinds) != len(want) {
		t.Fatalf("kinds after coalesce = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds after coalesce = %v, want %v", kinds, want)
		}
	}
	var thought, folded protocol.AgentChatEvent
	_ = json.Unmarshal([]byte(evs[0].JSON), &thought)
	_ = json.Unmarshal([]byte(evs[3].JSON), &folded)
	if thought.Text != "hmm" || folded.Text != "ab" || folded.Kind != protocol.EventMessage || evs[3].Seq != 7 {
		t.Fatalf("folded thought=%+v message=%+v", thought, folded)
	}

	// after_seq catch-up.
	tail, _ := st.ListChatEvents(ctx, "c1", 5, 0)
	if len(tail) != 2 || tail[0].Seq != 6 {
		t.Fatalf("after_seq=5 returned %+v", tail)
	}

	// Deleting the session removes its events.
	if err := st.DeleteChatSession(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ListChatEvents(ctx, "c1", 0, 0); len(left) != 0 {
		t.Fatalf("%d events survived session delete", len(left))
	}
}

func TestAgentAccountsUpsert(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	for _, a := range []store.AgentAccountRecord{
		{WorkerID: "w1", AccountID: "claude", JSON: `{"v":1}`},
		{WorkerID: "w1", AccountID: "claude", JSON: `{"v":2}`},
		{WorkerID: "w2", AccountID: "grok", JSON: `{}`},
	} {
		if err := st.UpsertAgentAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	w1, _ := st.ListAgentAccounts(ctx, "w1")
	if len(w1) != 1 || w1[0].JSON != `{"v":2}` {
		t.Fatalf("w1 accounts = %+v", w1)
	}
	all, _ := st.ListAgentAccounts(ctx, "")
	if len(all) != 2 {
		t.Fatalf("all accounts = %+v", all)
	}
	if err := st.DeleteAgentAccounts(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ListAgentAccounts(ctx, ""); len(left) != 1 {
		t.Fatalf("after delete = %+v", left)
	}
}
