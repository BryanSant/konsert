package chat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// Live test against a logged-in codex (ChatGPT subscription). Opt-in, one tiny prompt:
//
//	SYRE_LIVE_CODEX=1 go test ./internal/worker/chat -run TestLiveCodex -v
//
// The test thread is deleted afterwards (thread/delete). On a logged-out machine it
// checks the fail-fast auth path instead.
func TestLiveCodex(t *testing.T) {
	if os.Getenv("SYRE_LIVE_CODEX") != "1" {
		t.Skip("set SYRE_LIVE_CODEX=1 to run the live codex test")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	e := newCodexEngine()
	if path := os.Getenv("SYRE_LIVE_TRACE"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		e.trace = func(dir string, line []byte) { _, _ = f.WriteString(dir + " " + string(line) + "\n") }
	}
	acct := Account{ID: "codex", Agent: "codex", Label: "Codex", Binary: bin, IsDefault: true}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	p := e.Probe(ctx, acct)
	logProbe(t, p)
	rec := newLiveRec()
	if p.Auth.State == protocol.AuthLoggedOut {
		start := time.Now()
		_, err := e.Open(ctx, acct, protocol.AgentChatOpen{Cwd: os.TempDir()}, rec.emit)
		ev, _ := rec.until(t, 10*time.Second, func(ev protocol.AgentChatEvent) bool { return ev.Kind == protocol.EventError })
		t.Logf("logged out: open err=%v kind=%s relogin=%q in %s", err, ev.Error.Kind, ev.Error.Relogin.Command, time.Since(start).Round(time.Millisecond))
		if err == nil || ev.Error.Kind != protocol.ErrKindAuth {
			t.Fatal("logged-out codex did not fail fast with auth")
		}
		return
	}

	dir, err := os.MkdirTemp("/var/tmp", "syre-live-codex-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	s, err := e.Open(ctx, acct, protocol.AgentChatOpen{ChatID: "live", Agent: "codex", Cwd: dir, ApprovalMode: "read-only", Effort: "low"}, rec.emit)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cs := s.(*codexSession)
	defer func() {
		if conn := cs.conn; conn != nil && cs.threadID != "" {
			dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, derr := conn.call(dctx, "thread/delete", map[string]any{"threadId": cs.threadID})
			dcancel()
			t.Logf("cleanup: thread/delete err=%v", derr)
		}
		_ = s.Close()
	}()
	ready, _ := rec.until(t, 30*time.Second, func(ev protocol.AgentChatEvent) bool { return ev.Kind == protocol.EventSessionReady })
	t.Logf("session_ready: thread=%v model=%s effort=%s mode=%s", ready.AgentSessionID != "", ready.Model, ready.Effort, ready.ApprovalMode)
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t1", Text: "Reply with exactly: OK"}); err != nil {
		t.Fatal(err)
	}
	end, seen := rec.until(t, 3*time.Minute, turnDone)
	t.Logf("turn1: %s", liveSummary(seen))
	var msg, deltas string
	for _, ev := range seen {
		switch ev.Kind {
		case protocol.EventMessage:
			msg = ev.Text
		case protocol.EventMessageDelta:
			deltas += ev.Text
		case protocol.EventUsage:
			t.Logf("usage: in=%d out=%d cached=%d", ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CacheReadTokens)
		}
	}
	if end.Kind != protocol.EventTurnEnd || end.StopReason != "end_turn" || !strings.Contains(msg, "OK") || !strings.Contains(deltas, "OK") {
		t.Fatalf("turn: end=%+v msg=%q deltas=%q", end, msg, deltas)
	}
}
