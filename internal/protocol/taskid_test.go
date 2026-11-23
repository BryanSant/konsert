package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTaskID(t *testing.T) {
	for _, ok := range []string{"TASK-7QK3M", "a", "Task-1", strings.Repeat("z", FrameTaskIDSize)} {
		if err := ValidateTaskID(ok); err != nil {
			t.Fatalf("ValidateTaskID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", `a\b`, "a b", "ext:1", "a.b", strings.Repeat("z", FrameTaskIDSize+1), "café"} {
		if err := ValidateTaskID(bad); !errors.Is(err, ErrInvalidTaskID) {
			t.Fatalf("ValidateTaskID(%q) = %v, want ErrInvalidTaskID", bad, err)
		}
	}
}
