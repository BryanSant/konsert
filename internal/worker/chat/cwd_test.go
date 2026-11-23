package chat

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestResolveCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"": home, "~": home, "~/src": filepath.Join(home, "src")} {
		got, err := ResolveCwd(in)
		if err != nil || got != want {
			t.Errorf("ResolveCwd(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// Seen live: the orchestrator chat (cwd "~") failed with "claude is not installed".
	if _, err := ResolveCwd(filepath.Join(home, "missing")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing dir error = %v", err)
	}
}

func TestChdirFailureIsNotNotInstalled(t *testing.T) {
	chdir := &fs.PathError{Op: "chdir", Path: "/nope", Err: fs.ErrNotExist}
	if IsNotInstalled(chdir) {
		t.Fatal("a missing working directory was classified as a missing binary")
	}
	if !IsNotInstalled(exec.ErrNotFound) || !IsNotInstalled(&fs.PathError{Op: "fork/exec", Path: "/opt/claude", Err: fs.ErrNotExist}) {
		t.Fatal("a missing binary must still be not-installed")
	}
}

// Chat engine processes carry the managed-task marker so konsertd's global agent hooks
// don't report them as separate external sessions (seen live: the Agents roster filled
// with konsertd's own chat processes).
func TestManagedMarkerDoesNotMutateAccount(t *testing.T) {
	orig := Account{ID: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/x"}}
	got := withManagedMarker(orig, "chat-1")
	if got.Env[ManagedTaskEnv] != "chat:chat-1" || got.Env["CLAUDE_CONFIG_DIR"] != "/x" {
		t.Fatalf("marker env = %v", got.Env)
	}
	if _, leaked := orig.Env[ManagedTaskEnv]; leaked {
		t.Fatal("registry account env was modified")
	}
}

// Seen live: "starting" and "working" were each reported twice (engine and manager).
func TestManagerDropsRepeatedStatus(t *testing.T) {
	var got []string
	m := NewManager(ManagerOptions{Sink: func(ev protocol.AgentChatEvent) {
		if ev.Kind == protocol.EventStatus {
			got = append(got, ev.Status)
		}
	}})
	c := &chatState{id: "c"}
	for _, st := range []string{"starting", "starting", "working", "working"} {
		m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: st})
	}
	m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventTurnEnd})
	m.emitLocked(c, protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: "working"})
	if strings.Join(got, ",") != "starting,working,working" {
		t.Fatalf("statuses = %v", got)
	}
}
