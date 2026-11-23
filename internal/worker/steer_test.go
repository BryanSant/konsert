package worker

import "testing"

func TestSteerPayload(t *testing.T) {
	cases := []struct {
		in, goos string
		nl       bool
		want     string
	}{
		{"exit", "linux", true, "exit\n"},
		{"exit", "darwin", true, "exit\n"},
		{"exit", "windows", true, "exit\r"},
		{"exit", "windows", false, "exit"},
		{"exit\n", "windows", true, "exit\n"},
		{"exit\r", "linux", true, "exit\r"},
	}
	for _, c := range cases {
		if got := steerPayload(c.in, c.nl, c.goos); got != c.want {
			t.Errorf("steerPayload(%q, %v, %s) = %q, want %q", c.in, c.nl, c.goos, got, c.want)
		}
	}
}
