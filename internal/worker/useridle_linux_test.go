package worker

import (
	"strconv"
	"testing"
	"time"
)

func TestParseLogindIdle(t *testing.T) {
	now := time.UnixMicro(1_800_000_000_000_000)
	idleSince := now.Add(-10 * time.Minute).UnixMicro()
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"IdleHint=no\nIdleSinceHint=0\nActive=yes\n", 0, true},
		{"IdleHint=yes\nIdleSinceHint=" + itoa64(idleSince) + "\nActive=yes\n", 10 * time.Minute, true},
		{"IdleHint=yes\nIdleSinceHint=0\nActive=yes\n", 0, true},
		{"IdleHint=yes\nIdleSinceHint=1\nActive=no\n", 0, false},
	}
	for _, c := range cases {
		if got, ok := parseLogindIdle([]byte(c.in), now); got != c.want || ok != c.ok {
			t.Errorf("%q: %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
