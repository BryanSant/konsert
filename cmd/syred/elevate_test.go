package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/elevate"
	"github.com/spf13/pflag"
)

// The elevated child gets an install command line rebuilt from the parsed flags: never
// the password (it goes over the pipe), --password-stdin or --no-elevate, and from
// join only install's own flags (the one-time join code must not run twice).
func TestFlagArgsRebuildsInstall(t *testing.T) {
	join := pflag.NewFlagSet("join", pflag.ContinueOnError)
	join.AddFlagSet(newInstallCmd().Flags())
	join.String("code-only-join-has", "", "")
	if err := join.Parse([]string{"--service", "--user", `.\bob`, "--password-stdin", "--no-elevate",
		"--controller", "wss://hub/ws/worker", "--tag", "a=1", "--tag", "b=2", "--sandbox",
		"--code-only-join-has", "ABCD-EFGH"}); err != nil {
		t.Fatal(err)
	}

	got := flagArgs(join, newInstallCmd().Flags(), map[string]bool{"password-stdin": true, "no-elevate": true})
	s := strings.Join(got, " ")
	for _, want := range []string{"--service", `--user=.\bob`, "--controller=wss://hub/ws/worker", "--tag=a=1", "--tag=b=2", "--sandbox"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	for _, bad := range []string{"password-stdin", "no-elevate", "ABCD-EFGH"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaked into %s", bad, s)
		}
	}
	// The rebuilt line parses back to the same install.
	back := parseInstall(t, got...)
	if u, _ := back.GetString("user"); u != `.\bob` {
		t.Errorf("user = %q", u)
	}
	if tags, _ := back.GetStringSlice("tag"); strings.Join(tags, ";") != "a=1;b=2" {
		t.Errorf("tags = %v", tags)
	}
}

func TestUninstallArgs(t *testing.T) {
	o := uninstallOptions{name: "box", workerID: "w1", hubID: "h1", keepHooks: true, service: true, noElevate: true}
	if got := strings.Join(o.args(), " "); got != "uninstall --name=box --worker=w1 --hub=h1 --keep-hooks --service" {
		t.Errorf("args = %s", got)
	}
}

// The child runs the command with stdout and stderr (including direct writes to
// os.Stderr) relayed to the parent, and reports the exit code main would use.
func TestRunRelayed(t *testing.T) {
	parent, childEnd := net.Pipe()
	var out, errOut bytes.Buffer
	done := make(chan struct{})
	var code int
	var rerr error
	go func() {
		defer close(done)
		code, rerr = elevate.Relay(parent, elevate.Request{Args: []string{"install"}}, &out, &errOut)
	}()
	_, child, err := elevate.Accept(childEnd)
	if err != nil {
		t.Fatal(err)
	}
	c := runRelayed(child, func() error {
		fmt.Fprintln(os.Stdout, "to stdout")
		fmt.Fprintln(os.Stderr, "to stderr")
		return errors.New("boom")
	})
	if err := child.Exit(c); err != nil {
		t.Fatal(err)
	}
	<-done
	if rerr != nil || code != 1 {
		t.Fatalf("code=%d err=%v", code, rerr)
	}
	if out.String() != "to stdout\n" || errOut.String() != "to stderr\nsyred: boom\n" {
		t.Errorf("stdout %q stderr %q", out.String(), errOut.String())
	}
	if os.Stdout == nil || os.Stderr == nil {
		t.Fatal("stdio not restored")
	}
	if c := runRelayed(child, func() error { return exitCodeError{4} }); c != 4 {
		t.Errorf("exitCodeError code = %d", c)
	}
}
