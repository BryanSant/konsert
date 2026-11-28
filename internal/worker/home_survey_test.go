package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestSurveyHome(t *testing.T) {
	home := t.TempDir()
	mk := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{home}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("src", "a", ".git")
	mk("src", "b", ".git")
	mk("src", "notes")
	mk("source", "repos", "c", ".git")
	mk("code", "work", "deep", ".git") // two levels down: not counted for code
	if err := os.WriteFile(filepath.Join(home, "src", "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mk("Library") // not a code folder
	mk("custom", "x", ".git")

	got := surveyHome(home, []string{filepath.Join(home, "custom"), filepath.Join(home, "src"), "/elsewhere", filepath.Join(home, "..")})
	want := map[string]int{"src": 2, "code": 0, "source/repos": 1, "custom": 1}
	if len(got) != len(want) {
		t.Fatalf("survey = %+v, want %v", got, want)
	}
	for _, f := range got {
		if n, ok := want[f.Path]; !ok || n != f.Repos {
			t.Errorf("%+v, want repos %d (listed %v)", f, n, ok)
		}
	}
	if surveyHome("", nil) != nil {
		t.Error("no home should report nothing")
	}
	var _ []protocol.HomeFolder = got
}
