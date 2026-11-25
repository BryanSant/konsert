package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// Live tests against the real claude CLI on this machine, using the caller's
// subscriptions (no API key). Opt-in, tiny prompts:
//
//	SYRE_LIVE_CLAUDE=1 go test ./internal/worker/chat -run TestLiveClaude -v
//
// claude2 is a second subscription selected by env (CLAUDE_CONFIG_DIR=~/.claude2 plus
// the setup token from ~/.claude2-oauth-token, read in-process). Transcripts the tests
// create for their scratch cwd are deleted afterwards. SYRE_LIVE_TRACE=<file> writes
// the raw stream (no secrets are sent over it) for fixture capture.

func liveClaudeGate(t *testing.T) {
	t.Helper()
	if os.Getenv("SYRE_LIVE_CLAUDE") != "1" {
		t.Skip("set SYRE_LIVE_CLAUDE=1 to run live claude tests")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
}

func liveClaudeAccounts(t *testing.T) (Account, Account, bool) {
	t.Helper()
	bin, _ := exec.LookPath("claude")
	def := Account{ID: "claude", Agent: "claude", Label: "Claude", Binary: bin, IsDefault: true}
	home, _ := os.UserHomeDir()
	tok, err := os.ReadFile(filepath.Join(home, ".claude2-oauth-token"))
	if err != nil {
		return def, Account{}, false
	}
	dir := filepath.Join(home, ".claude2")
	c2 := Account{ID: "claude2", Agent: "claude", Label: "Claude 2", Binary: bin, Env: map[string]string{
		"CLAUDE_CONFIG_DIR":               dir,
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": dir,
		"CLAUDE_CODE_OAUTH_TOKEN":         strings.TrimSpace(string(tok)),
	}}
	return def, c2, true
}

// liveScratch makes a /var/tmp scratch cwd and removes it plus the transcripts the
// accounts stored for it.
func liveScratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "syre-live-claude-")
	if err != nil {
		t.Fatal(err)
	}
	proj := claudeProjectDirName(dir)
	t.Cleanup(func() {
		home, _ := os.UserHomeDir()
		for _, cfg := range []string{".claude", ".claude2"} {
			p := filepath.Join(home, cfg, "projects", proj)
			if entries, err := os.ReadDir(p); err == nil {
				for _, e := range entries {
					t.Logf("cleanup: removing %s", filepath.Join(p, e.Name()))
				}
				_ = os.RemoveAll(p)
			}
		}
		_ = os.RemoveAll(dir)
		t.Logf("cleanup: removed scratch %s", dir)
	})
	return dir
}

type liveRec struct {
	mu  sync.Mutex
	evs []protocol.AgentChatEvent
	ch  chan protocol.AgentChatEvent
}

func newLiveRec() *liveRec { return &liveRec{ch: make(chan protocol.AgentChatEvent, 4096)} }

func (r *liveRec) emit(ev protocol.AgentChatEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	r.ch <- ev
}

// until collects events until pred matches one (returned) or the timeout.
func (r *liveRec) until(t *testing.T, timeout time.Duration, pred func(protocol.AgentChatEvent) bool) (protocol.AgentChatEvent, []protocol.AgentChatEvent) {
	t.Helper()
	var seen []protocol.AgentChatEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-r.ch:
			seen = append(seen, ev)
			if pred(ev) {
				return ev, seen
			}
		case <-deadline:
			t.Fatalf("timed out; saw %d events: %s", len(seen), liveSummary(seen))
		}
	}
}

func liveSummary(evs []protocol.AgentChatEvent) string {
	var parts []string
	for _, e := range evs {
		s := e.Kind
		switch {
		case e.Error != nil:
			s += "(" + e.Error.Kind + ": " + e.Error.Message + ")"
		case e.StopReason != "":
			s += "(" + e.StopReason + ")"
		case e.ToolCall != nil:
			s += "(" + e.ToolCall.Name + " " + e.ToolCall.Status + ")"
		case e.RateLimit != nil:
			s += "(" + e.RateLimit.Window + " " + e.RateLimit.State + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func turnDone(ev protocol.AgentChatEvent) bool {
	return ev.Kind == protocol.EventTurnEnd || ev.Kind == protocol.EventError
}

func liveEngine(t *testing.T) *claudeEngine {
	e := newClaudeEngine()
	if path := os.Getenv("SYRE_LIVE_TRACE"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		e.trace = func(dir string, line []byte) {
			mu.Lock()
			defer mu.Unlock()
			_, _ = f.WriteString(dir + " " + string(line) + "\n")
		}
		t.Cleanup(func() { _ = f.Close() })
	}
	return e
}

func logProbe(t *testing.T, a protocol.AgentAccount) {
	t.Helper()
	var names []string
	for _, m := range a.Models {
		names = append(names, m.ID)
	}
	t.Logf("%s: version=%s auth=%s method=%s plan=%s models(%s)=%v default=%s", a.ID, a.Version, a.Auth.State, a.Auth.Method, a.Plan, a.ModelsSource, names, a.DefaultModel)
	for _, u := range a.Usage {
		rs := ""
		if u.ResetsAt != nil {
			rs = u.ResetsAt.Format(time.RFC3339)
		}
		t.Logf("%s usage: %s %.0f%% %s resets %s", a.ID, u.Window, u.UsedPercent, u.State, rs)
	}
	if a.UsageNote != "" {
		t.Logf("%s usage note: %s", a.ID, a.UsageNote)
	}
}

func TestLiveClaudeProbe(t *testing.T) {
	liveClaudeGate(t)
	def, c2, ok := liveClaudeAccounts(t)
	e := liveEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	accts := []Account{def}
	if ok {
		accts = append(accts, c2)
	}
	for _, a := range accts {
		p := e.Probe(ctx, a)
		logProbe(t, p)
		if p.Auth.State != protocol.AuthOK && p.Auth.State != protocol.AuthExpiring {
			t.Errorf("%s: auth %s (%s)", a.ID, p.Auth.State, p.Auth.Detail)
		}
		if p.ModelsSource != "discovered" || len(p.Models) < 3 {
			t.Errorf("%s: models not discovered: %s %d", a.ID, p.ModelsSource, len(p.Models))
		}
		if len(p.Usage) == 0 && p.UsageNote == "" {
			t.Errorf("%s: neither usage nor a usage note", a.ID)
		}
	}
}

// TestLiveClaudeDefault runs one session on the default account: a tiny turn, a context
// turn, one permission elicitation approved via Respond, and one cancel.
func TestLiveClaudeDefault(t *testing.T) {
	liveClaudeGate(t)
	def, _, _ := liveClaudeAccounts(t)
	// The user's settings allow Write outright; an ask rule makes it prompt.
	def.ArgsPrefix = []string{"--settings", `{"permissions":{"ask":["Write"]}}`}
	e := liveEngine(t)
	dir := liveScratch(t)
	rec := newLiveRec()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	s, err := e.Open(ctx, def, protocol.AgentChatOpen{ChatID: "live", Agent: "claude", Cwd: dir, ApprovalMode: "default", Effort: "low"}, rec.emit)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ready, _ := rec.until(t, 30*time.Second, func(ev protocol.AgentChatEvent) bool { return ev.Kind == protocol.EventSessionReady })
	t.Logf("session_ready: has_session_id=%v mode=%s", ready.AgentSessionID != "", ready.ApprovalMode)

	// Turn 1.
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t1", Text: "Reply with exactly: OK"}); err != nil {
		t.Fatal(err)
	}
	end, seen := rec.until(t, 2*time.Minute, turnDone)
	t.Logf("turn1: %s", liveSummary(seen))
	if end.Kind != protocol.EventTurnEnd || end.StopReason != "end_turn" {
		t.Fatalf("turn1 ended with %+v", end)
	}
	var deltas, msg string
	var usage *protocol.AgentTokenUsage
	for _, ev := range seen {
		switch ev.Kind {
		case protocol.EventMessageDelta:
			deltas += ev.Text
		case protocol.EventMessage:
			msg = ev.Text
		case protocol.EventUsage:
			usage = ev.Usage
		}
	}
	if !strings.Contains(deltas, "OK") || !strings.Contains(msg, "OK") || usage == nil || usage.OutputTokens == 0 {
		t.Fatalf("turn1: deltas=%q msg=%q usage=%+v", deltas, msg, usage)
	}
	t.Logf("turn1 usage: in=%d out=%d cacheRead=%d cost=$%.4f", usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CostUSD)
	sid := s.AgentSessionID()

	// Turn 2: context.
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t2", Text: "What did you just reply? Answer in one word."}); err != nil {
		t.Fatal(err)
	}
	end, seen = rec.until(t, 2*time.Minute, turnDone)
	t.Logf("turn2: %s", liveSummary(seen))
	msg = ""
	for _, ev := range seen {
		if ev.Kind == protocol.EventMessage {
			msg = ev.Text
		}
	}
	if end.Kind != protocol.EventTurnEnd || !strings.Contains(strings.ToUpper(msg), "OK") {
		t.Fatalf("turn2: end=%+v msg=%q", end, msg)
	}
	if s.AgentSessionID() != sid {
		t.Errorf("session id changed: %s -> %s", sid, s.AgentSessionID())
	}

	// Turn 3: permission elicitation.
	target := filepath.Join(dir, "hello.txt")
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t3", Text: "Use the Write tool to create " + target + " containing hi. Then reply DONE."}); err != nil {
		t.Fatal(err)
	}
	el, seen := rec.until(t, 2*time.Minute, func(ev protocol.AgentChatEvent) bool {
		return ev.Kind == protocol.EventElicitation || turnDone(ev)
	})
	if el.Kind != protocol.EventElicitation {
		t.Fatalf("no elicitation: %s", liveSummary(seen))
	}
	t.Logf("elicitation: kind=%s title=%q tool=%s options=%d", el.Elicitation.Kind, el.Elicitation.Title, el.Elicitation.ToolCall.Name, len(el.Elicitation.Options))
	if err := s.Respond(ctx, protocol.AgentChatRespond{ElicitationID: el.Elicitation.ID, OptionID: "allow_once"}); err != nil {
		t.Fatal(err)
	}
	end, seen = rec.until(t, 2*time.Minute, turnDone)
	t.Logf("turn3: %s", liveSummary(seen))
	if end.Kind != protocol.EventTurnEnd {
		t.Fatalf("turn3 ended with %+v", end)
	}
	if b, err := os.ReadFile(target); err != nil || !strings.Contains(string(b), "hi") {
		t.Fatalf("file not written after approval: %v %q", err, b)
	}

	// Turn 4: cancel mid-turn.
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t4", Text: "Write the numbers 1 to 200 as words, one per line."}); err != nil {
		t.Fatal(err)
	}
	_, _ = rec.until(t, 2*time.Minute, func(ev protocol.AgentChatEvent) bool { return ev.Kind == protocol.EventMessageDelta })
	if err := s.Control(ctx, protocol.AgentChatControl{Cancel: true}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	end, seen = rec.until(t, time.Minute, turnDone)
	t.Logf("turn4 after cancel: %s", liveSummary(seen))
	if end.Kind != protocol.EventTurnEnd || end.StopReason != "cancelled" {
		t.Fatalf("cancel ended with %+v", end)
	}
}

// TestLiveClaude2 sends one tiny prompt on the second account. When the account is out
// of usage it must fail as usage_limit (never auth), and Probe must then show the
// window as exhausted.
func TestLiveClaude2(t *testing.T) {
	liveClaudeGate(t)
	_, c2, ok := liveClaudeAccounts(t)
	if !ok {
		t.Skip("no ~/.claude2-oauth-token")
	}
	e := liveEngine(t)
	dir := liveScratch(t)
	rec := newLiveRec()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	s, err := e.Open(ctx, c2, protocol.AgentChatOpen{ChatID: "live2", Agent: "claude", Cwd: dir, Effort: "low"}, rec.emit)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t1", Text: "Reply with exactly: OK"}); err != nil {
		t.Fatal(err)
	}
	end, seen := rec.until(t, 2*time.Minute, turnDone)
	t.Logf("claude2 turn1: %s", liveSummary(seen))
	switch {
	case end.Kind == protocol.EventError:
		t.Logf("claude2 error: kind=%s window=%s resets=%v retryable=%v message=%q", end.Error.Kind, end.Error.Window, end.Error.ResetsAt, end.Error.Retryable, end.Error.Message)
		if end.Error.Kind != protocol.ErrKindUsageLimit {
			t.Fatalf("expected usage_limit, got %s", end.Error.Kind)
		}
		p := e.Probe(ctx, c2)
		logProbe(t, p)
		found := false
		for _, u := range p.Usage {
			if u.Window == end.Error.Window && u.State == "exhausted" {
				found = true
			}
		}
		if !found {
			t.Errorf("probe does not show %s exhausted", end.Error.Window)
		}
	default:
		// Account has usage again: run the context turn too.
		if err := s.Prompt(ctx, protocol.AgentChatPrompt{TurnID: "t2", Text: "What did you just reply? Answer in one word."}); err != nil {
			t.Fatal(err)
		}
		end, seen = rec.until(t, 2*time.Minute, turnDone)
		t.Logf("claude2 turn2: %s", liveSummary(seen))
		if end.Kind != protocol.EventTurnEnd {
			t.Fatalf("turn2 ended with %+v", end)
		}
	}
}
