package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallClaudeHooksQuotesPathAndReplacesOldEntries(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An older install wrote the path unquoted.
	old := `{"hooks":{"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"/old/konsertd hook-event --agent claude --event Stop"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := "/Users/Some User/Apps/konsert's bin/konsertd"
	if err := installClaudeHooks(home, bin); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	var parsed struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	stop := parsed.Hooks["Stop"]
	if len(stop) != 1 {
		t.Fatalf("expected the old Konsert Stop hook to be replaced, got %d entries: %s", len(stop), raw)
	}
	want := `'/Users/Some User/Apps/konsert'\''s bin/konsertd' hook-event --agent claude --event Stop`
	if got := stop[0].Hooks[0].Command; got != want {
		t.Fatalf("hook command = %q, want %q", got, want)
	}
	if shellQuote("/usr/local/bin/konsertd") != "/usr/local/bin/konsertd" {
		t.Fatal("plain paths should stay unquoted")
	}
}

func TestScratchDirIsPerUserAndPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the per-user TEMP directory")
	}
	if got := ResolveScratchDir("linux"); !strings.HasSuffix(got, scratchDirName()) || scratchDirName() == "konsert-attachments" {
		t.Fatalf("scratch dir is not per-user: %q", got)
	}
	base := t.TempDir()

	// A pre-existing dir with broad permissions is tightened to 0700.
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(loose, 0o777)
	if _, err := SaveScratchAttachment(loose, "s", "a.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(loose); info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch dir perms = %o, want 700", info.Mode().Perm())
	}

	// A symlinked scratch dir (e.g. planted by another user) is refused, and never pruned.
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(target, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().Add(-72 * time.Hour)
	_ = os.Chtimes(victim, longAgo, longAgo)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := checkPrivateDir(link); err == nil {
		t.Fatal("symlinked scratch dir must be refused")
	}
	PruneScratchAttachments(link, AttachmentTTL)
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("prune followed a symlinked scratch dir and deleted a file")
	}
}
