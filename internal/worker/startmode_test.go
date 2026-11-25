package worker

import "testing"

func TestStartModeOf(t *testing.T) {
	for _, c := range []struct {
		service, batch bool
		session        uint32
		want           string
	}{
		{true, false, 0, StartModeService},
		{false, true, 0, StartModeBackgroundTask},
		{false, false, 1, StartModeUserSession},
		{false, false, 0, ""},
	} {
		if got := startModeOf(c.service, c.batch, c.session); got != c.want {
			t.Errorf("startModeOf(%v, %v, %d) = %q, want %q", c.service, c.batch, c.session, got, c.want)
		}
	}
}
