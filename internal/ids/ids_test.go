package ids

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestUUIDIsV7AndOrdered(t *testing.T) {
	a, b := UUID(), UUID()
	u, err := uuid.Parse(a)
	if err != nil || u.Version() != 7 {
		t.Fatalf("UUID() = %q (version %v, err %v), want v7", a, u.Version(), err)
	}
	if !(a < b) {
		t.Fatalf("UUIDv7s not increasing: %s then %s", a, b)
	}
}

func TestNewFormatAndParse(t *testing.T) {
	for _, k := range Kinds {
		id, err := New(k, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, sfx, ok := Parse(id)
		if !ok || got != k || len(sfx) != SuffixLength {
			t.Fatalf("New(%s) = %q; Parse = %v %q %v", k, id, got, sfx, ok)
		}
		if !Valid(k, id) || Valid(Chat, "task-7qk3m") {
			t.Fatalf("Valid mismatch for %q", id)
		}
	}
	for _, bad := range []string{"", "TASK-7QK3M", "task-7qk3", "task-7qk3i", "nope-7qk3m", "75cca373-29b6-4692-840b-bd942267949f"} {
		if _, _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if !Valid(Workspace, DefaultWorkspace) || !Valid(Project, DefaultProject) {
		t.Fatalf("defaults invalid: %s %s", DefaultWorkspace, DefaultProject)
	}
}

func TestNewRetriesAndGrows(t *testing.T) {
	calls := 0
	id, err := New(Task, func(string) (bool, error) { calls++; return calls <= maxTriesPerLength, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, sfx, _ := Parse(id); len(sfx) != SuffixLength+1 {
		t.Fatalf("after %d collisions got %q, want a %d-char suffix", maxTriesPerLength, id, SuffixLength+1)
	}
	boom := errors.New("db down")
	if _, err := New(Task, func(string) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestLowEntropy(t *testing.T) {
	for s, want := range map[string]bool{"00000": true, "aaaab": true, "01234": true, "zyxwv": true, "7qk3m": false, "w47xk": false} {
		if got := lowEntropy(s); got != want {
			t.Errorf("lowEntropy(%q) = %v, want %v", s, got, want)
		}
	}
}
