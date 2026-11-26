package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeRemoteURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"git@github.com:Org/Repo.git", "github.com/Org/Repo"},
		{"git@GitHub.COM:Org/Repo.git/", "github.com/Org/Repo"},
		{"https://github.com/Org/Repo.git", "github.com/Org/Repo"},
		{"https://GITHUB.COM/Org/Repo/", "github.com/Org/Repo"},
		{"https://github.com/Org/Repo", "github.com/Org/Repo"},
		{"https://user:token@github.com/Org/Repo.git", "github.com/Org/Repo"},
		{"http://GitLab.Example.com/team/sub/proj.git/", "gitlab.example.com/team/sub/proj"},
		{"ssh://git@github.com/Org/Repo.git", "github.com/Org/Repo"},
		{"ssh://git@GITHUB.COM:22/Org/Repo.git", "github.com/Org/Repo"},
		{"ssh://git@github.com:2222/Org/Repo.git", "github.com:2222/Org/Repo"},
		{"https://github.com:443/Org/Repo.git", "github.com/Org/Repo"},
		{"git://github.com/Org/Repo.git", "github.com/Org/Repo"},
		{"file:///tmp/origin.git", "/tmp/origin"},
		{"/tmp/origin.git/", "/tmp/origin"},
		{"/tmp/origin", "/tmp/origin"},
	}
	for _, tc := range cases {
		got := NormalizeRemoteURL(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPathContains(t *testing.T) {
	cases := []struct {
		root, target string
		want         bool
	}{
		{"/src/app", "/src/app", true},
		{"/src/app/", "/src/app", true},
		{"/src/app", "/src/app/sub/dir", true},
		{"/src/app", "/src/app2", false},
		{"/src/app", "/src/ap", false},
		{`C:\Src\App`, `c:\src\app\pkg`, true},
		{`C:\Src\App`, `C:\Src\App2`, false},
		{"", "/src/app", false},
		{"/src/app", "", false},
	}
	for _, tc := range cases {
		if got := PathContains(tc.root, tc.target); got != tc.want {
			t.Errorf("PathContains(%q, %q) = %v, want %v", tc.root, tc.target, got, tc.want)
		}
	}
}

func initTempGitRepoWithRemote(t *testing.T, dir, remoteURL string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	if remoteURL != "" {
		run("remote", "add", "origin", remoteURL)
	}
	sha, err := CurrentSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestDefaultCheckoutRootsSkipHomeUnderTest(t *testing.T) {
	allowed := t.TempDir()
	got := DefaultCheckoutRoots([]string{allowed}, nil)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	homeSrc := filepath.Join(home, "src")
	for _, p := range got {
		if p == homeSrc {
			t.Fatalf("DefaultCheckoutRoots included %s under go test: %v", homeSrc, got)
		}
	}
	withHome := defaultCheckoutRoots([]string{allowed}, nil, true)
	found := false
	for _, p := range withHome {
		if p == homeSrc {
			found = true
		}
	}
	if !found {
		t.Fatalf("defaultCheckoutRoots(..., true) = %v, want %s", withHome, homeSrc)
	}
}

func TestDiscoverCheckouts(t *testing.T) {
	root := t.TempDir()
	repo1 := filepath.Join(root, "org", "alpha")
	sha1 := initTempGitRepoWithRemote(t, repo1, "git@GitHub.com:Org/Alpha.git")
	// Touch FETCH_HEAD to verify last_fetch extraction.
	fetchHead := filepath.Join(repo1, ".git", "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte(sha1+"\t\tbranch 'main'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantFetch := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	_ = os.Chtimes(fetchHead, wantFetch, wantFetch)

	// Linked worktree inside root should NOT be reported as a primary checkout.
	wtPath := filepath.Join(root, "wt-alpha")
	if _, err := CreateWorktreeAt(context.Background(), repo1, wtPath, "main", "feat/wt"); err != nil {
		t.Fatalf("CreateWorktreeAt: %v", err)
	}

	// Repo deeper than maxDepth=2 should be skipped.
	deepRepo := filepath.Join(root, "a", "b", "c", "deep")
	initTempGitRepoWithRemote(t, deepRepo, "https://github.com/org/deep.git")

	// Repos under .syre (clones and ticket worktrees) and the legacy clone dir are skipped.
	cloneRepo := filepath.Join(root, ".syre", "clones", "cloned")
	initTempGitRepoWithRemote(t, cloneRepo, "https://github.com/org/cloned.git")
	legacyClone := filepath.Join(root, ".syre-clones", "legacy")
	initTempGitRepoWithRemote(t, legacyClone, "https://github.com/org/legacy.git")
	ticketRepo := filepath.Join(root, ".syre", "tickets", "tk-1", "app")
	initTempGitRepoWithRemote(t, ticketRepo, "https://github.com/org/app.git")

	found := DiscoverCheckouts(context.Background(), []string{root}, 2)
	if len(found) != 1 {
		t.Fatalf("expected 1 checkout, got %d: %+v", len(found), found)
	}
	co := found[0]
	if co.RemoteURL != "github.com/Org/Alpha" {
		t.Errorf("RemoteURL = %q, want %q", co.RemoteURL, "github.com/Org/Alpha")
	}
	if co.Branch != "main" {
		t.Errorf("Branch = %q, want main", co.Branch)
	}
	if co.Head != sha1 {
		t.Errorf("Head = %q, want %q", co.Head, sha1)
	}
	if co.LastFetch == nil || co.LastFetch.Sub(wantFetch).Abs() > 2*time.Second {
		t.Errorf("LastFetch = %v, want ~%v", co.LastFetch, wantFetch)
	}
}
