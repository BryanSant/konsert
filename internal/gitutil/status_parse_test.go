package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParsePorcelainZ(t *testing.T) {
	out := " M a b.txt\x00R  new name.go\x00old name.go\x00XX\x00?? café \"q\".txt\x00"
	got := ParsePorcelainZ(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries (malformed record skipped, not aborting), got %+v", got)
	}
	if got[0].Code != " M" || got[0].Path != "a b.txt" {
		t.Fatalf("entry 0: %+v", got[0])
	}
	if got[1].Code != "R " || got[1].Path != "new name.go" || got[1].OrigPath != "old name.go" {
		t.Fatalf("rename entry: %+v", got[1])
	}
	if got[2].Code != "??" || got[2].Path != "café \"q\".txt" {
		t.Fatalf("quoted entry: %+v", got[2])
	}
}

func TestParseNumstatZ(t *testing.T) {
	out := "3\t1\tplain.go\x00" + "2\t0\t\x00old dir/a.go\x00new dir/a.go\x00" + "-\t-\timg.png\x00"
	got := ParseNumstatZ(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %+v", got)
	}
	if got[0].Path != "plain.go" || got[0].Added != 3 || got[0].Deleted != 1 {
		t.Fatalf("plain: %+v", got[0])
	}
	if got[1].Path != "new dir/a.go" || got[1].OrigPath != "old dir/a.go" || got[1].Added != 2 {
		t.Fatalf("rename: %+v", got[1])
	}
	if !got[2].Binary || got[2].Path != "img.png" {
		t.Fatalf("binary: %+v", got[2])
	}
}

func gitTestRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	return repo, run
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectWorkingTreeQuotedRenamedAndSymlinkedPaths(t *testing.T) {
	ctx := context.Background()
	repo, run := gitTestRepo(t)

	writeFile(t, filepath.Join(repo, "old name.txt"), "one\ntwo\nthree\nfour\nfive\n")
	writeFile(t, filepath.Join(repo, "tracked.txt"), "a\n")
	run("add", "-A")
	run("commit", "-q", "-m", "init")

	// Staged rename of a path with a space, plus an edit so numstat is non-zero.
	run("mv", "old name.txt", "new name.txt")
	writeFile(t, filepath.Join(repo, "new name.txt"), "one\ntwo\nthree\nfour\nfive\nsix\n")
	run("add", "-A")

	// Tracked file modification.
	writeFile(t, filepath.Join(repo, "tracked.txt"), "a\nb\n")

	// Untracked file whose name git would C-quote without -z.
	quoted := "café \"quoted\".txt"
	if runtime.GOOS == "windows" {
		quoted = "café quoted.txt"
	}
	writeFile(t, filepath.Join(repo, quoted), "hello\n")

	// Untracked symlink pointing at a secret outside the repo.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_ed25519")
	writeFile(t, secret, "SECRET-KEY-MATERIAL\n")
	symlinked := false
	if err := os.Symlink(secret, filepath.Join(repo, "leak")); err == nil {
		symlinked = true
	}

	res, err := InspectWorkingTree(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]int{}
	for i, f := range res.Files {
		byPath[f.Path] = i
		if strings.Contains(f.Diff, "SECRET-KEY-MATERIAL") {
			t.Fatalf("symlink target content leaked via %q", f.Path)
		}
	}

	i, ok := byPath["new name.txt"]
	if !ok {
		t.Fatalf("renamed path missing: %+v", res.Files)
	}
	if res.Files[i].Status != "R" || res.Files[i].Added == 0 {
		t.Fatalf("rename entry should be status R with numstat matched: %+v", res.Files[i])
	}
	if i, ok := byPath["tracked.txt"]; !ok || res.Files[i].Added != 1 {
		t.Fatalf("tracked modification missing or numstat mismatch: %+v", res.Files)
	}
	if i, ok := byPath[quoted]; !ok || !strings.Contains(res.Files[i].Diff, "+hello") {
		t.Fatalf("quoted untracked path missing or unread: %+v", res.Files)
	}
	if symlinked {
		i, ok := byPath["leak"]
		if !ok {
			t.Fatalf("untracked symlink should still be listed: %+v", res.Files)
		}
		if res.Files[i].Diff != "" || res.Files[i].Added != 0 {
			t.Fatalf("untracked symlink must not be read: %+v", res.Files[i])
		}
	}
}
