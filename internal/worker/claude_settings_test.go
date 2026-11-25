package worker

import (
	"bytes"
	"encoding/json/jsontext"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/claude-settings")

const testSyred = "/opt/syre/bin/syred"

// scratchHome is a temporary HOME under /var/tmp (never the real one; /tmp is RAM-backed
// on the fleet's Linux boxes).
func scratchHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/var/tmp", "syre-hooks-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "claude-settings", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update to create it)", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// Install edits only our entries into the user's file, in its own formatting; removal
// gives back the original byte for byte; a second install changes nothing.
func TestClaudeSettingsGolden(t *testing.T) {
	for _, name := range []string{"typical", "four-space-no-hooks", "tabs-stale", "compact"} {
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata", "claude-settings", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := installClaudeHookText(in, testSyred)
			if err != nil {
				t.Fatal(err)
			}
			if !jsontext.Value(out).IsValid() {
				t.Fatalf("installed settings are not valid JSON:\n%s", out)
			}
			golden(t, name+".installed.json", out)
			again, err := installClaudeHookText(out, testSyred)
			if err != nil || !bytes.Equal(again, out) {
				t.Fatalf("second install changed the file (err %v):\n%s", err, again)
			}
			removed, err := removeClaudeHookText(out)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(in, []byte(syreHookMarker)) {
				// Had an old Syre entry: removal leaves the user's settings only.
				golden(t, name+".removed.json", removed)
				if bytes.Contains(removed, []byte(syreHookMarker)) {
					t.Fatal("a Syre entry survived removal")
				}
			} else if !bytes.Equal(removed, in) {
				t.Fatalf("removal did not restore the original:\n--- got\n%s\n--- want\n%s", removed, in)
			}
		})
	}
}

func TestClaudeSettingsKeepsKeyOrderAndUserHooks(t *testing.T) {
	in, _ := os.ReadFile(filepath.Join("testdata", "claude-settings", "typical.json"))
	out, err := installClaudeHookText(in, testSyred)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	order := []string{`"model"`, `"permissions"`, `"hooks"`, `"PreToolUse"`, `echo \"pre — ✓\"`, testSyred + " hook-event --agent claude --event PreToolUse", `"SessionStart"`, `"PostToolUse"`, `"Notification"`, `"Stop"`, `"env"`, `"alwaysThinkingEnabled"`}
	at := 0
	for _, want := range order {
		i := strings.Index(s[at:], want)
		if i < 0 {
			t.Fatalf("%s missing or out of order in:\n%s", want, s)
		}
		at += i
	}
	// Lines of the user's own that we did not touch are byte-identical.
	if !strings.Contains(s, "    \"allow\": [\"Bash(git status)\", \"Read(~/notes/**)\"],\n") || !strings.Contains(s, "  \"env\": { \"FOO\": \"bar\" },\n") {
		t.Fatalf("user lines were reformatted:\n%s", s)
	}
}

func TestClaudeSettingsCRLFAndEmpty(t *testing.T) {
	in := []byte("{\r\n  \"theme\": \"dark\"\r\n}\r\n")
	out, err := installClaudeHookText(in, testSyred)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatalf("bare LF in a CRLF file:\n%q", out)
	}
	if back, _ := removeClaudeHookText(out); !bytes.Equal(back, in) {
		t.Fatalf("CRLF removal: %q", back)
	}
	for _, empty := range [][]byte{nil, []byte("  \n"), []byte("{}")} {
		out, err := installClaudeHookText(empty, testSyred)
		if err != nil || !jsontext.Value(out).IsValid() || !strings.HasPrefix(string(out), "{\n  \"hooks\": {\n    \"PreToolUse\": [\n") {
			t.Fatalf("install into %q: %v\n%s", empty, err, out)
		}
	}
	if _, err := installClaudeHookText([]byte(`{"hooks": [] }`), testSyred); err == nil {
		t.Fatal("a non-object hooks member was overwritten")
	}
	if _, err := installClaudeHookText([]byte(`{"a": 1,}`), testSyred); err == nil {
		t.Fatal("invalid JSON was edited")
	}
}

// The file on disk: no write and no backup when nothing changes, a timestamped backup
// before every change, only our last 3 kept (other files alone), mode preserved.
func TestClaudeSettingsFileBackupsAndIdempotence(t *testing.T) {
	home := scratchHome(t)
	dir := filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(filepath.Join("testdata", "claude-settings", "four-space-no-hooks.json"))
	if err := os.WriteFile(path, orig, 0o640); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "settings.json.bak")
	if err := os.WriteFile(foreign, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	settingsNow = func() time.Time { return clock }
	t.Cleanup(func() { settingsNow = time.Now })

	if err := installClaudeHooks(home, testSyred); err != nil {
		t.Fatal(err)
	}
	baks, _ := claudeSettingsBackupList(path)
	if len(baks) != 1 || filepath.Base(baks[0]) != "settings.json.syre-bak-20261124-090000" {
		t.Fatalf("backups %v", baks)
	}
	if b, _ := os.ReadFile(baks[0]); !bytes.Equal(b, orig) {
		t.Fatal("the backup is not the original")
	}
	// Windows has no Unix permission bits (os.Stat reports 0666 or 0444), so only check where
	// the mode is real.
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v, want 0640", fi.Mode().Perm())
	}
	installed, _ := os.ReadFile(path)
	fi1, _ := os.Stat(path)

	// Already installed: nothing written, no backup.
	clock = clock.Add(time.Minute)
	if err := installClaudeHooks(home, testSyred); err != nil {
		t.Fatal(err)
	}
	if baks, _ := claudeSettingsBackupList(path); len(baks) != 1 {
		t.Fatalf("a no-op install took a backup: %v", baks)
	}
	if fi2, _ := os.Stat(path); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("a no-op install rewrote the file")
	}

	// Moving syred changes the entries each time: one backup per change, last 3 kept
	// (two in the same second get a counter).
	for i := range 4 {
		if i != 2 {
			clock = clock.Add(time.Second)
		}
		if err := installClaudeHooks(home, fmt.Sprintf("/opt/syre-%d/syred", i)); err != nil {
			t.Fatal(err)
		}
	}
	baks, _ = claudeSettingsBackupList(path)
	var names []string
	for _, b := range baks {
		names = append(names, filepath.Base(b))
	}
	want := []string{"settings.json.syre-bak-20261124-090102", "settings.json.syre-bak-20261124-090102-2", "settings.json.syre-bak-20261124-090103"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("backups %v, want %v", names, want)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("another tool's backup was removed")
	}

	// Removal brings back the original text.
	if _, err := RemoveAgentHooks(home); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, orig) {
		t.Fatalf("after removal:\n%s\nwant:\n%s", got, orig)
	}
	if bytes.Equal(installed, orig) {
		t.Fatal("install changed nothing")
	}
	// Removing again, or from a home without settings, is a no-op.
	if changed, err := removeClaudeHooks(home); err != nil || changed {
		t.Fatalf("second removal: %v %v", changed, err)
	}
	if changed, err := removeClaudeHooks(scratchHome(t)); err != nil || changed {
		t.Fatalf("removal without settings: %v %v", changed, err)
	}
}

// A symlinked settings.json (a second config dir sharing ~/.claude's) stays a link; the
// file it points to is edited.
func TestClaudeSettingsFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home := scratchHome(t)
	shared := filepath.Join(home, "shared-settings.json")
	if err := os.WriteFile(shared, []byte("{\n  \"theme\": \"dark\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".claude", "settings.json")
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}
	if err := installClaudeHooks(home, testSyred); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("settings.json is no longer a symlink")
	}
	if b, _ := os.ReadFile(shared); !bytes.Contains(b, []byte(syreHookMarker)) {
		t.Fatalf("the shared file was not edited:\n%s", b)
	}
}
