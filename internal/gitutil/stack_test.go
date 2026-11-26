package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestMergeBranchStackedRetargetAfterBlockerSquash(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	run(repo, "init", "-b", "main")
	run(repo, "config", "user.email", "test@example.com")
	run(repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a - b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "calc.go")
	run(repo, "commit", "-m", "init buggy Add")

	// Blocker A fixes Add in two commits on syre/tk-a.
	wtA := filepath.Join(t.TempDir(), "wt-a")
	if _, err := CreateStackedWorktreeAt(ctx, repo, wtA, "main", "syre/tk-a", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtA, "calc.go"), []byte("package calc\n\n// Add returns a + b.\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(wtA, "add", "calc.go")
	run(wtA, "commit", "-m", "fix Add")
	blockerHead := run(wtA, "rev-parse", "HEAD")

	// Dependent B stacks on syre/tk-a and adds Sub to the same file.
	wtB := filepath.Join(t.TempDir(), "wt-b")
	infoB, err := CreateStackedWorktreeAt(ctx, repo, wtB, "main", "syre/tk-b", []string{"syre/tk-a"})
	if err != nil {
		t.Fatal(err)
	}
	if infoB.BaseSHA != blockerHead {
		t.Fatalf("infoB.BaseSHA = %q, want %q", infoB.BaseSHA, blockerHead)
	}
	if err := os.WriteFile(filepath.Join(wtB, "calc.go"), []byte("package calc\n\n// Add returns a + b.\nfunc Add(a, b int) int { return a + b }\n\nfunc Sub(a, b int) int { return a - b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(wtB, "add", "calc.go")
	run(wtB, "commit", "-m", "add Sub")

	// Deliver A into main via squash-merge (local-only).
	resA := MergeBranch(ctx, wtA, protocol.GitMergeRequest{
		SourceBranch:  "syre/tk-a",
		TargetBranch:  "main",
		Style:         "squash",
		CommitMessage: "Fix Add",
		TaskSID:       "t-a",
	})
	if !resA.OK || !resA.Merged {
		t.Fatalf("deliver A failed: %+v", resA)
	}

	// Deliver B into main with StackBaseSHA = infoB.BaseSHA (retargets B onto main).
	resB := MergeBranch(ctx, wtB, protocol.GitMergeRequest{
		SourceBranch:  "syre/tk-b",
		TargetBranch:  "main",
		Style:         "squash",
		CommitMessage: "Add Sub",
		TaskSID:       "t-b",
		StackBaseSHA:  infoB.BaseSHA,
	})
	if !resB.OK || !resB.Merged {
		t.Fatalf("deliver B failed: %+v", resB)
	}

	finalCalc, err := os.ReadFile(filepath.Join(repo, "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(finalCalc)
	if !strings.Contains(got, "return a + b") || !strings.Contains(got, "func Sub(a, b int) int") {
		t.Fatalf("main calc.go missing expected changes:\n%s", got)
	}
}
