package chat

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

// TestClaudeForkSessionAndMCPTimeout: a side-conversation fork starts with
// --resume <original> --fork-session --session-id <new>, the original transcript stays,
// Discard deletes only the fork's transcript; Konsert's MCP server gets the hub's
// tool timeout (per-server "timeout" and MCP_TOOL_TIMEOUT), which claude otherwise caps
// at 60 s for HTTP servers.
func TestClaudeForkSessionAndMCPTimeout(t *testing.T) {
	e, l := newTestClaude(t, fakeClaudeOpts{})
	rec := newCCRecorder()
	cfgDir, cwd := t.TempDir(), t.TempDir()
	tdir := filepath.Join(cfgDir, "projects", claudeProjectDirName(cwd))
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(tdir, "orig.jsonl")
	if err := os.WriteFile(orig, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acct := Account{ID: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": cfgDir}}
	opts := protocol.AgentChatOpen{
		Cwd: cwd, ApprovalMode: "plan", ForkFrom: "orig",
		MCPServers: []protocol.AgentMCPServer{{Name: "konsert", URL: "http://hub/mcp", ToolTimeoutSeconds: 2400}},
	}
	s, err := e.Open(context.Background(), acct, opts, rec.emit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ready, _ := rec.wait(t, isKind(protocol.EventSessionReady))
	args := l.last().spec.Args
	i := slices.Index(args, "--resume")
	if i < 0 || i+4 >= len(args) || args[i+1] != "orig" || args[i+2] != "--fork-session" || args[i+3] != "--session-id" || args[i+4] != ready.AgentSessionID || ready.AgentSessionID == "orig" {
		t.Fatalf("fork argv = %v (session %q)", args, ready.AgentSessionID)
	}
	if got := argValue(args, "--permission-mode"); got != "plan" {
		t.Errorf("--permission-mode = %q", got)
	}
	if !slices.Contains(l.last().spec.Env, "MCP_TOOL_TIMEOUT=2400000") {
		t.Errorf("MCP_TOOL_TIMEOUT not set in the child env")
	}
	b, _ := os.ReadFile(argValue(args, "--mcp-config"))
	var cfg struct {
		MCPServers map[string]struct {
			Timeout int `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.MCPServers["konsert"].Timeout != 2400000 {
		t.Fatalf("mcp config = %s", b)
	}

	// Discard removes only the fork's transcript.
	fork := filepath.Join(tdir, ready.AgentSessionID+".jsonl")
	if err := os.WriteFile(fork, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Discard(acct, opts, ready.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fork); !os.IsNotExist(err) {
		t.Error("fork transcript not discarded")
	}
	if _, err := os.Stat(orig); err != nil {
		t.Error("original transcript was removed")
	}
	if err := e.Discard(acct, protocol.AgentChatOpen{Cwd: cwd}, "orig"); err != nil || fileGone(orig) {
		t.Error("Discard of a non-fork chat must not delete anything")
	}

	// A fork of a session this account doesn't have fails clearly.
	if _, err := e.Open(context.Background(), acct, protocol.AgentChatOpen{Cwd: cwd, ForkFrom: "missing"}, rec.emit); err == nil {
		t.Fatal("fork of a missing session should fail")
	}
}

func fileGone(p string) bool {
	_, err := os.Stat(p)
	return os.IsNotExist(err)
}

func TestKonsertToolRunning(t *testing.T) {
	tools := map[string]*protocol.AgentToolCall{
		"a": {Name: "Read", Status: "running"},
		"b": {Name: "mcp__konsert__ask_user", Status: "completed"},
	}
	if konsertToolRunning(tools) {
		t.Fatal("no Konsert tool is running")
	}
	tools["c"] = &protocol.AgentToolCall{Name: "mcp__konsert__ask_user", Status: "running"}
	if !konsertToolRunning(tools) {
		t.Fatal("ask_user is running: the watchdog must stay quiet")
	}
	if !konsertToolRunning(map[string]*protocol.AgentToolCall{"x": {Title: "Run mcp_konsert_ask_user", Status: "pending"}}) {
		t.Fatal("ACP-style title not recognized")
	}
}

func TestMockForkRemembersTheOriginal(t *testing.T) {
	e := &MockEngine{StepDelay: 1}
	var evs []protocol.AgentChatEvent
	done := make(chan struct{}, 8)
	emit := func(ev protocol.AgentChatEvent) {
		evs = append(evs, ev)
		if ev.Kind == protocol.EventTurnEnd {
			done <- struct{}{}
		}
	}
	orig, err := e.Open(context.Background(), Account{ID: "mock"}, protocol.AgentChatOpen{ChatID: "c1"}, emit)
	if err != nil {
		t.Fatal(err)
	}
	_ = orig.Prompt(context.Background(), protocol.AgentChatPrompt{ChatID: "c1", TurnID: "t1", Text: "remember the secret word MANGO"})
	<-done
	fork, err := e.Open(context.Background(), Account{ID: "mock"}, protocol.AgentChatOpen{ChatID: "c2", ForkFrom: orig.AgentSessionID()}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if fork.AgentSessionID() == orig.AgentSessionID() || !strings.HasPrefix(fork.AgentSessionID(), "mock-fork-") {
		t.Fatalf("fork session id %q", fork.AgentSessionID())
	}
	_ = fork.Prompt(context.Background(), protocol.AgentChatPrompt{ChatID: "c2", TurnID: "t2",
		Text: "[Konsert side conversation] copy.\n\nQuestion q-abcde: Go?\nOption 1: Yes (recommended) — goes\nOption 2: No — stays\n\nThe user asks: why?"})
	<-done
	var reply string
	for _, ev := range evs {
		if ev.Kind == protocol.EventMessage && ev.TurnID == "t2" {
			reply = ev.Text
		}
	}
	if !strings.Contains(reply, "read-only fork of "+orig.AgentSessionID()) || !strings.Contains(reply, "MANGO") || !strings.Contains(reply, "DRAFT ANSWER: Yes") {
		t.Fatalf("fork reply = %q", reply)
	}
	_ = orig.Close()
	_ = fork.Close()
}
