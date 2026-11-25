package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/worker"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

func runID(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newIDCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("syred id %v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

// daemonID is the identity `syred run` with args would connect with.
func daemonID(t *testing.T, args ...string) string {
	t.Helper()
	var o runOptions
	cmd := newRunCmd()
	cmd.ResetFlags()
	o.bind(cmd.Flags())
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg, err := o.resolve(cmd.Flags())
	if err != nil {
		t.Fatal(err)
	}
	return worker.NewDaemon(cfg, drivers.DefaultRegistry, nil).WorkerID()
}

func TestIDMatchesRun(t *testing.T) {
	state := t.TempDir()
	t.Setenv(worker.StateDirEnv, state)
	host, _ := os.Hostname()

	cases := [][]string{
		nil,
		{"--name", host},
		{"--name", "sidecar"},
		{"--name", "sidecar", "--isolation", "container"},
	}
	for _, args := range cases {
		id := runID(t, args...)
		if !ids.IsUUIDv7(id) {
			t.Fatalf("syred id %v = %q, want a UUIDv7", args, id)
		}
		if got := daemonID(t, args...); got != id {
			t.Fatalf("run %v would use %s, syred id printed %s", args, got, id)
		}
		if again := runID(t, args...); again != id {
			t.Fatalf("syred id is not stable: %s then %s", id, again)
		}
	}
	// The default instance and --name <hostname> are the same identity; another name is not.
	if runID(t) != runID(t, "--name", host) || runID(t) == runID(t, "--name", "sidecar") {
		t.Fatal("identity files do not follow the name the way install instances do")
	}
	matches, _ := filepath.Glob(filepath.Join(state, "worker-id-*-host"))
	if len(matches) != 2 { // the hostname's and sidecar's
		t.Fatalf("syred id must create one identity file per name, got %v", matches)
	}
	if _, err := os.Stat(matches[0]); err != nil {
		t.Fatalf("syred id must create the identity file: %v", err)
	}

	// An explicit --id wins, as it does for run.
	const fixed = "01890a5d-ac96-774b-bcce-b302099a8057"
	if got := runID(t, "--id", fixed); got != fixed {
		t.Fatalf("--id ignored: %s", got)
	}

	// A config file is honoured too.
	cfgPath := filepath.Join(t.TempDir(), "w.yaml")
	if err := os.WriteFile(cfgPath, []byte("name: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runID(t, "-c", cfgPath) != runID(t, "--name", "from-file") {
		t.Fatal("syred id -c must resolve the name from the config file")
	}
}
