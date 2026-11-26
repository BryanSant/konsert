package chat

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestWorkdirAutoApproveInWorkdirVsEscalateOutside(t *testing.T) {
	workdir := t.TempDir()
	demoDir := filepath.Join(workdir, "demo")
	if err := os.MkdirAll(demoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()

	opts := []protocol.AgentElicitationOption{
		{ID: "allow-once", Kind: "allow_once"},
		{ID: "reject-once", Kind: "reject_once"},
	}

	s := &acpSession{
		opts: protocol.AgentChatOpen{
			Cwd:                workdir,
			AutoApproveWorkdir: true,
			EngineWorktrees:    []protocol.TaskWorktree{{RepoName: "demo", WorktreePath: demoDir}},
		},
	}

	allowed := []struct {
		name string
		tc   *protocol.AgentToolCall
		raw  string
	}{
		{
			name: "ls_in_workdir",
			tc:   &protocol.AgentToolCall{Name: "run_command", Kind: "execute", Title: "ls -la"},
			raw:  `{"rawInput":{"CommandLine":"cd ` + filepath.ToSlash(demoDir) + ` && ls -la && git status -sb","WaitMsBeforeAsync":5000}}`,
		},
		{
			name: "go_test_relative",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"go test ./..."}`},
		},
		{
			name: "rm_rf_relative_build_dir",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"rm -rf build/"}`},
		},
		{
			name: "edit_in_workdir",
			tc:   &protocol.AgentToolCall{Name: "Edit", Kind: "edit", Input: `{"file_path":"` + filepath.ToSlash(filepath.Join(demoDir, "main.go")) + `"}`},
		},
		{
			name: "read_relative",
			tc:   &protocol.AgentToolCall{Name: "Read", Kind: "read", Input: `{"path":"demo/README.md"}`},
		},
		{
			name: "git_push_force_feature_branch",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"git push --force-with-lease origin syre/SYR-12345"}`},
		},
	}
	for _, tc := range allowed {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			if got := s.workdirAllowOption(tc.tc, jsontext.Value(tc.raw), opts); got != "allow-once" {
				t.Fatalf("expected auto-allow, got %q", got)
			}
		})
	}

	escalated := []struct {
		name string
		tc   *protocol.AgentToolCall
		raw  string
	}{
		{
			name: "edit_outside_workdir",
			tc:   &protocol.AgentToolCall{Name: "Edit", Kind: "edit", Input: `{"file_path":"` + filepath.ToSlash(filepath.Join(outsideDir, "secret.txt")) + `"}`},
		},
		{
			name: "read_parent_traversal",
			tc:   &protocol.AgentToolCall{Name: "Read", Kind: "read", Input: `{"path":"../../etc/passwd"}`},
		},
		{
			name: "rm_rf_outside_workdir",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"rm -rf ` + filepath.ToSlash(outsideDir) + `"}`},
		},
		{
			name: "rm_rf_workdir_root",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"rm -rf ."}`},
		},
		{
			name: "sudo_command",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"sudo ls -la"}`},
		},
		{
			name: "sudo_chained",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"ls && sudo rm -rf build"}`},
		},
		{
			name: "git_push_force_main",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"git push --force origin main"}`},
		},
		{
			name: "git_push_f_master",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"git push -f origin master"}`},
		},
		{
			name: "git_push_force_no_branch",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"git push --force"}`},
		},
		{
			name: "cd_outside_workdir",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"cd ` + filepath.ToSlash(outsideDir) + ` && ls"}`},
		},
		{
			name: "bare_cd_home",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"cd && ls"}`},
		},
		{
			name: "redirect_outside",
			tc:   &protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"echo pwn >` + filepath.ToSlash(filepath.Join(outsideDir, "x")) + `"}`},
		},
		{
			name: "web_fetch",
			tc:   &protocol.AgentToolCall{Name: "WebFetch", Kind: "fetch", Input: `{"url":"https://example.com"}`},
		},
	}
	for _, tc := range escalated {
		t.Run("escalate/"+tc.name, func(t *testing.T) {
			if got := s.workdirAllowOption(tc.tc, jsontext.Value(tc.raw), opts); got != "" {
				t.Fatalf("expected escalation (empty allow option), got %q", got)
			}
		})
	}

	// Disabled when AutoApproveWorkdir is false (project approval_mode=ask or non-ticket chat).
	sAsk := &acpSession{opts: protocol.AgentChatOpen{Cwd: workdir, AutoApproveWorkdir: false}}
	if got := sAsk.workdirAllowOption(&protocol.AgentToolCall{Name: "Bash", Kind: "execute", Input: `{"command":"ls -la"}`}, nil, opts); got != "" {
		t.Fatalf("expected no auto-approve when AutoApproveWorkdir=false, got %q", got)
	}

	// Symlink escape inside workdir pointing outside must escalate.
	if runtime.GOOS != "windows" {
		link := filepath.Join(demoDir, "escape")
		if err := os.Symlink(outsideDir, link); err == nil {
			tc := &protocol.AgentToolCall{Name: "Edit", Kind: "edit", Input: `{"file_path":"` + filepath.Join(link, "pwn.txt") + `"}`}
			if got := s.workdirAllowOption(tc, nil, opts); got != "" {
				t.Fatalf("expected symlink escape to escalate, got %q", got)
			}
		}
	}
}
