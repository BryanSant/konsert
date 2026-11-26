package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

type stubGraceEngine struct {
	mu             sync.Mutex
	finishOnCancel bool
	session        *stubGraceSession
}

func (e *stubGraceEngine) Agent() string { return "stubgrace" }
func (e *stubGraceEngine) Probe(context.Context, Account) protocol.AgentAccount {
	return protocol.AgentAccount{Agent: "stubgrace"}
}
func (e *stubGraceEngine) Open(_ context.Context, _ Account, _ protocol.AgentChatOpen, emit Emit) (Session, error) {
	e.mu.Lock()
	fin := e.finishOnCancel
	s := &stubGraceSession{emit: emit, finishOnCancel: fin}
	e.session = s
	e.mu.Unlock()
	emit(protocol.AgentChatEvent{Kind: protocol.EventSessionReady, Status: protocol.ChatStatusIdle, AgentSessionID: "sess-1"})
	return s, nil
}

type stubGraceSession struct {
	mu             sync.Mutex
	emit           Emit
	finishOnCancel bool
	canceled       bool
	closed         bool
	closedAt       time.Time
	turnID         string
}

func (s *stubGraceSession) Prompt(_ context.Context, p protocol.AgentChatPrompt) error {
	s.mu.Lock()
	s.turnID = p.TurnID
	s.mu.Unlock()
	s.emit(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusWorking, TurnID: p.TurnID})
	return nil
}
func (s *stubGraceSession) Respond(context.Context, protocol.AgentChatRespond) error { return nil }
func (s *stubGraceSession) Control(_ context.Context, ctl protocol.AgentChatControl) error {
	if ctl.Cancel {
		s.mu.Lock()
		s.canceled = true
		fin := s.finishOnCancel
		tid := s.turnID
		s.mu.Unlock()
		if fin {
			s.emit(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: tid, StopReason: "cancelled"})
		}
	}
	return nil
}
func (s *stubGraceSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.closedAt = time.Now()
	s.mu.Unlock()
	return nil
}
func (s *stubGraceSession) AgentSessionID() string { return "sess-1" }

func TestManagerStopChatGrace(t *testing.T) {
	t.Run("immediate_close_when_turn_exits_on_cancel", func(t *testing.T) {
		col := newCollector()
		eng := &stubGraceEngine{finishOnCancel: true}
		mgr := NewManager(ManagerOptions{
			Engines:  map[string]Engine{"stubgrace": eng},
			Accounts: staticResolver{},
			Sink:     col.sink,
		})
		defer mgr.Shutdown(context.Background())

		mgr.Open(protocol.AgentChatOpen{ChatID: "c-fast", Agent: "stubgrace", Cwd: t.TempDir()})
		mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-fast", TurnID: "t1", Text: "work"})
		col.waitFor(t, 0, "working", func(e protocol.AgentChatEvent) bool {
			return e.Kind == protocol.EventStatus && e.Status == protocol.ChatStatusWorking
		})

		start := time.Now()
		mgr.StopChat("c-fast", 5*time.Second)

		waitUntil(t, "session closed", func() bool {
			eng.mu.Lock()
			s := eng.session
			eng.mu.Unlock()
			if s == nil {
				return false
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.canceled && s.closed
		})
		if elapsed := time.Since(start); elapsed > 1*time.Second {
			t.Fatalf("expected prompt close after turn cancelled, took %v", elapsed)
		}
	})

	t.Run("grace_honoured_then_force_close", func(t *testing.T) {
		col := newCollector()
		eng := &stubGraceEngine{finishOnCancel: false}
		mgr := NewManager(ManagerOptions{
			Engines:  map[string]Engine{"stubgrace": eng},
			Accounts: staticResolver{},
			Sink:     col.sink,
		})
		defer mgr.Shutdown(context.Background())

		mgr.Open(protocol.AgentChatOpen{ChatID: "c-slow", Agent: "stubgrace", Cwd: t.TempDir()})
		mgr.Prompt(protocol.AgentChatPrompt{ChatID: "c-slow", TurnID: "t1", Text: "hang"})
		col.waitFor(t, 0, "working", func(e protocol.AgentChatEvent) bool {
			return e.Kind == protocol.EventStatus && e.Status == protocol.ChatStatusWorking
		})

		grace := 300 * time.Millisecond
		start := time.Now()
		mgr.StopChat("c-slow", grace)

		waitUntil(t, "session force closed after grace", func() bool {
			eng.mu.Lock()
			s := eng.session
			eng.mu.Unlock()
			if s == nil {
				return false
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.canceled && s.closed
		})
		elapsed := time.Since(start)
		if elapsed < grace-40*time.Millisecond {
			t.Fatalf("closed before grace expired: elapsed=%v, grace=%v", elapsed, grace)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("took too long after grace: elapsed=%v", elapsed)
		}
	})
}
