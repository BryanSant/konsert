package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestFetchArtifactCaps(t *testing.T) {
	ctx := context.Background()

	t.Run("lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "notes.txt")
		if err := os.WriteFile(path, []byte(numberedLines(300, 0)), 0o644); err != nil {
			t.Fatal(err)
		}
		page := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindFile, Workdir: dir, Path: "notes.txt",
		})
		if page.Error != "" {
			t.Fatal(page.Error)
		}
		if page.MaxLines != protocol.InspectDefaultMaxLines || !page.Truncated || page.NextOffset != 200 {
			t.Fatalf("page=%+v", page)
		}
		if !strings.Contains(page.Text, "L00200\n") || strings.Contains(page.Text, "L00201") {
			t.Fatalf("first page should end at line 200:\n%s", page.Text)
		}
		if !strings.Contains(page.Note, "offset=200") {
			t.Fatalf("note=%q", page.Note)
		}
		rest := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindFile, Workdir: dir, Path: "notes.txt", Offset: page.NextOffset,
		})
		if rest.Error != "" || rest.Truncated {
			t.Fatalf("second page=%+v", rest)
		}
		if !strings.HasPrefix(rest.Text, "L00201\n") || !strings.Contains(rest.Text, "L00300\n") {
			t.Fatalf("second page should be lines 201-300:\n%s", rest.Text)
		}
		past := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindFile, Workdir: dir, Path: "notes.txt", Offset: 1000,
		})
		if past.Error != "" || past.Truncated || past.Text != "" {
			t.Fatalf("offset past the end=%+v", past)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		const width = 2000
		dir := t.TempDir()
		path := filepath.Join(dir, "wide.txt")
		const n = 40
		if err := os.WriteFile(path, []byte(numberedLines(n, width)), 0o644); err != nil {
			t.Fatal(err)
		}
		page := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindFile, Workdir: dir, Path: "wide.txt", MaxLines: 9000,
		})
		if page.Error != "" {
			t.Fatal(page.Error)
		}
		if page.MaxLines != protocol.InspectMaxLinesLimit {
			t.Fatalf("max_lines not clamped: %+v", page.MaxLines)
		}
		if len(page.Text) > protocol.InspectByteCap || len(page.Text)%width != 0 {
			t.Fatalf("len=%d width=%d truncated=%v", len(page.Text), width, page.Truncated)
		}
		fit := protocol.InspectByteCap / width
		if !page.Truncated || page.NextOffset != fit {
			t.Fatalf("want %d lines then a new page, got %+v", fit, page)
		}
		if !strings.HasPrefix(page.Text, "L00001 ") || strings.Contains(page.Text, "L00033") {
			t.Fatalf("first page spilled into the next line:\n%.80q", page.Text)
		}
		rest := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindFile, Workdir: dir, Path: "wide.txt", MaxLines: 5000, Offset: page.NextOffset,
		})
		if rest.Error != "" || rest.Truncated || rest.MaxLines != protocol.InspectMaxLinesLimit {
			t.Fatalf("second page=%+v", rest)
		}
		if !strings.HasPrefix(rest.Text, "L00033 ") {
			t.Fatalf("second page starts at %q", rest.Text[:min(20, len(rest.Text))])
		}
	})

	t.Run("log", func(t *testing.T) {
		var b strings.Builder
		for i := 1; i <= 30; i++ {
			fmt.Fprintf(&b, "log %02d\n", i)
		}
		host := ArtifactHost{Log: func(id string) (string, bool) {
			if id == "task-1" {
				return b.String(), true
			}
			return "", false
		}}
		page := FetchArtifact(ctx, host, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindLog, TaskID: "task-1", MaxLines: 10,
		})
		if page.Error != "" || !page.Truncated || page.NextOffset != 10 || !strings.Contains(page.Text, "log 10\n") || strings.Contains(page.Text, "log 11") {
			t.Fatalf("log page=%+v", page)
		}
		missing := FetchArtifact(ctx, host, protocol.ArtifactFetchRequest{Kind: protocol.InspectKindLog, TaskID: "other"})
		if !strings.Contains(missing.Error, "no retained log") {
			t.Fatalf("missing log=%+v", missing)
		}
	})

	t.Run("cleaned up", func(t *testing.T) {
		page := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindDiff, Workdir: filepath.Join(t.TempDir(), "gone"),
		})
		if !strings.Contains(page.Error, "cleaned up") {
			t.Fatalf("page=%+v", page)
		}
	})

	t.Run("path escape", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "wt")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		secret := []byte("SECRET-VALUE")
		if err := os.WriteFile(filepath.Join(parent, "secret.txt"), secret, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"../secret.txt", "/etc/passwd"} {
			page := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
				Kind: protocol.InspectKindFile, Workdir: root, Path: path,
			})
			if page.Error == "" || strings.Contains(page.Text, "SECRET-VALUE") {
				t.Fatalf("path %q page=%+v", path, page)
			}
		}
	})

	t.Run("diff", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "config", "user.email", "t@example.com")
		runGit(t, dir, "config", "user.name", "tester")
		readme := filepath.Join(dir, "README")
		if err := os.WriteFile(readme, []byte("before\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "README")
		runGit(t, dir, "commit", "-m", "init")
		if err := os.WriteFile(readme, []byte("after-change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		page := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindDiff, Workdir: dir,
		})
		if page.Error != "" || !strings.Contains(page.Text, "after-change") {
			t.Fatalf("diff=%q err=%s", page.Text, page.Error)
		}
		stat := FetchArtifact(ctx, ArtifactHost{}, protocol.ArtifactFetchRequest{
			Kind: protocol.InspectKindDiffStat, Workdir: dir,
		})
		if stat.Error != "" || !strings.Contains(stat.Text, "README") {
			t.Fatalf("stat=%q err=%s", stat.Text, stat.Error)
		}
	})
}

func numberedLines(n, width int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		prefix := fmt.Sprintf("L%05d", i)
		if width <= len(prefix)+1 {
			b.WriteString(prefix)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(prefix)
		b.WriteByte(' ')
		b.WriteString(strings.Repeat("x", width-len(prefix)-2))
		b.WriteByte('\n')
	}
	return b.String()
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
