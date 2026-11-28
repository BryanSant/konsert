package controller

import (
	"context"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// A machine finishing its spool replay fails the turns its old connection lost, but not
// a chat the scheduler is still opening on it: a ticket run created as starting just
// before the replay ended used to fail with "Lost the connection" (SYR-DZBS6).
func TestFailInterruptedChatsSparesStartingChats(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(id string, status string) {
		t.Helper()
		if err := cs.st.CreateChatSession(ctx, store.ChatSession{
			ID: id, Backend: store.ChatBackendAgent, WorkerID: "w-replay", Agent: "mock",
			Status: status, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("chat-starting", protocol.ChatStatusStarting)
	mk("chat-working", protocol.ChatStatusWorking)
	mk("chat-active", protocol.ChatStatusWorking)

	wc := &workerConn{id: "w-replay", activeTasks: map[string]bool{}}
	wc.markChatActive("chat-active")
	cs.h.failInterruptedChats(ctx, wc)

	get := func(id string) store.ChatSession {
		t.Helper()
		s, err := cs.st.GetChatSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := get("chat-starting"); s.Status != protocol.ChatStatusStarting || s.LastErrorJSON != "" {
		t.Errorf("starting chat = %q, error %q; want it left starting with no error", s.Status, s.LastErrorJSON)
	}
	if s := get("chat-active"); s.Status != protocol.ChatStatusWorking || s.LastErrorJSON != "" {
		t.Errorf("chat active on the connection = %q, error %q; want it left working", s.Status, s.LastErrorJSON)
	}
	if s := get("chat-working"); s.Status == protocol.ChatStatusWorking || s.LastErrorJSON == "" {
		t.Errorf("interrupted chat = %q, error %q; want it failed with the lost-connection error", s.Status, s.LastErrorJSON)
	}
}
