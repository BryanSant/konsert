package install

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseSchtasksQuery(t *testing.T) {
	const name = `\konsertd-winbox`
	cases := []struct {
		out          string
		found        bool
		state        string
		wantsRunning bool
	}{
		{`"\konsertd-winbox","N/A","Running"` + "\r\n", true, "Running", true},
		{"\uFEFF" + `"\konsertd-winbox","11/24/2026 3:01:00 PM","Ready"` + "\r\n\r\n", true, "Ready", false},
		{"\r\n" + `"\konsertd-winbox","N/A","Queued"` + "\r\n", true, "Queued", false},
		// Two triggers can list the task twice; a running row wins.
		{`"\konsertd-winbox","11/24/2026 3:01:00 PM","Ready"` + "\r\n" + `"\konsertd-winbox","N/A","Running"` + "\r\n", true, "Running", true},
		{`"\KONSERTD-WINBOX","N/A","running"`, true, "running", true},
		{`"\konsertd-other","N/A","Running"`, false, "", false},
		{"", false, "", false},
		{`ERROR: The system cannot find the file specified.`, false, "", false},
	}
	for _, c := range cases {
		found, state := parseSchtasksQuery(c.out, name)
		if found != c.found || state != c.state || schtasksStateRunning(state) != c.wantsRunning {
			t.Errorf("parseSchtasksQuery(%q) = %v %q", c.out, found, state)
		}
	}
}

// scriptedRunner answers schtasks /Query with the next scripted state.
type scriptedRunner struct {
	states []string
	runs   int
	calls  []string
}

func (r *scriptedRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, line)
	switch {
	case strings.HasPrefix(line, "schtasks /Run"):
		r.runs++
	case strings.HasPrefix(line, "schtasks /Query"):
		st := r.states[0]
		if len(r.states) > 1 {
			r.states = r.states[1:]
		}
		return `"\konsertd-winbox","N/A","` + st + `"` + "\r\n", nil
	}
	return "", nil
}

func TestStartAndWait(t *testing.T) {
	ctx := context.Background()
	noSleep := func(context.Context, time.Duration) error { return nil }
	opts := func(alive func(context.Context) bool) WaitOptions {
		return WaitOptions{Wait: 10 * time.Second, Poll: time.Second, Alive: alive, Sleep: noSleep}
	}

	// Already running: nothing started.
	r := &scriptedRunner{states: []string{"Running"}}
	in := &Installer{Runner: r}
	st, err := in.startAndWait(ctx, schtasksBackend{}, "winbox", opts(nil))
	if err != nil || !st.Running || r.runs != 0 {
		t.Fatalf("running: %+v %v runs=%d", st, err, r.runs)
	}

	// Ready → /Run → Running a few polls later.
	r = &scriptedRunner{states: []string{"Ready", "Ready", "Queued", "Running"}}
	in = &Installer{Runner: r}
	st, err = in.startAndWait(ctx, schtasksBackend{}, "winbox", opts(nil))
	if err != nil || !st.Running || r.runs != 1 || st.Detail != "Running" {
		t.Fatalf("started: %+v %v runs=%d", st, err, r.runs)
	}

	// The task still says Ready but the daemon answers on its socket: running.
	polls := 0
	r = &scriptedRunner{states: []string{"Ready"}}
	in = &Installer{Runner: r}
	st, err = in.startAndWait(ctx, schtasksBackend{}, "winbox", opts(func(context.Context) bool { polls++; return polls > 2 }))
	if err != nil || !st.Running || !strings.Contains(st.Detail, "daemon answering") {
		t.Fatalf("socket: %+v %v", st, err)
	}

	// Never comes up (nobody logged on): not running after the wait, /Run retried once.
	r = &scriptedRunner{states: []string{"Ready"}}
	in = &Installer{Runner: r}
	st, err = in.startAndWait(ctx, schtasksBackend{}, "winbox", opts(func(context.Context) bool { return false }))
	if err != nil || st.Running || r.runs != 2 {
		t.Fatalf("deferred: %+v %v runs=%d", st, err, r.runs)
	}

	// Not installed: not started.
	in = &Installer{Runner: &fakeRunner{fail: map[string]error{"schtasks /Query": context.DeadlineExceeded}}}
	if st, err := in.startAndWait(ctx, schtasksBackend{}, "winbox", opts(nil)); err != nil || st.Installed || st.Running {
		t.Fatalf("not installed: %+v %v", st, err)
	}
}
