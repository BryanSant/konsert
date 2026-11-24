package worker

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestInstallAgentHooksIdempotentAndPreservesExistingSettings(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"theme":"dark","customFlag":true,"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo user-hook"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	// Run twice to verify idempotency
	rep1, err := InstallAgentHooks(home, "/usr/local/bin/konsertd")
	if err != nil || !rep1.ClaudeHooksInstalled || !rep1.AgySkillInstalled {
		t.Fatalf("InstallAgentHooks #1 failed: rep=%+v err=%v", rep1, err)
	}
	rep2, err := InstallAgentHooks(home, "/usr/local/bin/konsertd")
	if err != nil || !rep2.ClaudeHooksInstalled {
		t.Fatalf("InstallAgentHooks #2 failed: rep=%+v err=%v", rep2, err)
	}

	raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The user's file is edited in place, never re-encoded: the compact one-liner keeps
	// its key order and layout, with our entries added in the same style.
	if !strings.HasPrefix(string(raw), `{"theme":"dark","customFlag":true,"hooks":{"PreToolUse":[{"matcher":"Bash"`) || strings.Contains(string(raw), "\n") {
		t.Fatalf("settings.json layout:\n%s", raw)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["theme"] != "dark" || parsed["customFlag"] != true {
		t.Fatalf("existing claude settings were not preserved: %+v", parsed)
	}
	hooks := parsed["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 { // 1 user hook + 1 Konsert hook (no duplicate on second install!)
		t.Fatalf("expected 2 PreToolUse hooks (1 user + 1 konsert), got %d: %+v", len(pre), pre)
	}
}

func TestHookEventDiscoveryAndQuotaCollection(t *testing.T) {
	home := t.TempDir()

	var emitted []protocol.ExternalSessionPayload
	disc := NewSessionDiscovery(home, nil, func(p protocol.ExternalSessionPayload) {
		emitted = append(emitted, p)
	})

	if err := RecordHookEvent(home, HookEvent{
		SessionID: "ext-agy-1",
		Agent:     "agy",
		Event:     "PreToolUse",
		Cwd:       "/srv/app1",
		FilePath:  "main.go",
	}); err != nil {
		t.Fatalf("RecordHookEvent failed: %v", err)
	}
	disc.PollHookEvents(context.Background())

	if len(emitted) != 1 || emitted[0].Agent != "agy" || emitted[0].Status != "working" || emitted[0].Cwd != "/srv/app1" {
		t.Fatalf("unexpected hook discovery emission: %+v", emitted)
	}

	// Stop hook transitions an open interactive session to "idle" (not spinning, not exited)
	if err := RecordHookEvent(home, HookEvent{
		SessionID: "ext-agy-1",
		Agent:     "agy",
		Event:     "Stop",
		Cwd:       "/srv/app1",
	}); err != nil {
		t.Fatalf("RecordHookEvent Stop failed: %v", err)
	}
	disc.PollHookEvents(context.Background())
	if len(emitted) != 2 || emitted[1].Status != "idle" {
		t.Fatalf("expected Stop hook to emit idle status, got %+v", emitted)
	}
}
