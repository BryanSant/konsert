package chat

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// TestClaudeLiveTokenUsage checks a real Claude account end to end (SYR-BA6HV): its auth
// mode, then one tiny turn (a little quota), then the usage windows it reported, with
// when they were seen. Opt-in: SYRE_CLAUDE_LIVE=1 [SYRE_CLAUDE_LIVE_ACCOUNT=claude2].
// Prints no token.
func TestClaudeLiveTokenUsage(t *testing.T) {
	if os.Getenv("SYRE_CLAUDE_LIVE") != "1" {
		t.Skip("set SYRE_CLAUDE_LIVE=1 to run a real Claude account")
	}
	id := os.Getenv("SYRE_CLAUDE_LIVE_ACCOUNT")
	if id == "" {
		id = "claude2"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	reg := NewAccountRegistry(AccountOptions{})
	acct, err := reg.Resolve("claude", id)
	if err != nil {
		t.Fatalf("resolve %s: %v", id, err)
	}
	e := newClaudeEngine()
	e.probeDir = t.TempDir()
	before := e.Probe(ctx, acct)
	t.Logf("before: auth=%s method=%s plan=%q windows=%d note=%q", before.Auth.State, before.Auth.Method, before.Plan, len(before.Usage), before.UsageNote)

	done := make(chan struct{})
	var closed bool
	emit := func(ev protocol.AgentChatEvent) {
		if (ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError) && !closed {
			closed = true
			close(done)
		}
	}
	s, err := e.Open(ctx, acct, protocol.AgentChatOpen{ChatID: "live", Agent: "claude", AccountID: id, Cwd: t.TempDir(), Model: "haiku"}, emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{ChatID: "live", TurnID: "t1", Text: "Reply with just: ok"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("no turn end")
	}

	after := e.Probe(ctx, acct)
	var names []string
	for _, w := range after.Usage {
		seen := "?"
		if w.ObservedAt != nil {
			seen = w.ObservedAt.Local().Format("15:04:05")
		}
		names = append(names, fmt.Sprintf("%s=%.0f%% seen %s", w.Window, w.UsedPercent, seen))
	}
	t.Logf("after: method=%s windows=%v note=%q", after.Auth.Method, names, after.UsageNote)
	if before.Auth.Method == "setup_token" {
		if len(after.Usage) < 2 {
			t.Fatalf("token account shows %d windows after a turn", len(after.Usage))
		}
		for _, w := range after.Usage {
			if w.ObservedAt == nil {
				t.Fatalf("window %s has no observed_at", w.Window)
			}
		}
	}
}
