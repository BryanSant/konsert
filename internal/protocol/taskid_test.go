package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTaskID(t *testing.T) {
	for _, ok := range []string{"550e8400-e29b-41d4-a716-446655440000", "a", "Task-1", strings.Repeat("z", 36)} {
		if err := ValidateTaskID(ok); err != nil {
			t.Fatalf("ValidateTaskID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", `a\b`, "a b", "ext:1", "a.b", strings.Repeat("z", 37), "café"} {
		if err := ValidateTaskID(bad); !errors.Is(err, ErrInvalidTaskID) {
			t.Fatalf("ValidateTaskID(%q) = %v, want ErrInvalidTaskID", bad, err)
		}
	}
}
