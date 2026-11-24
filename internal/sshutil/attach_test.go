package sshutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The attach ssh uses only the hub key, never a personal key, agent or shared
// connection, forwards nothing, and pins host keys.
func TestAttachArgsUseOnlyTheHubKey(t *testing.T) {
	args := AttachArgs(Target{User: "me", Host: "box", Port: "2222"}, "/db/konsert-hub-ssh-key", "01890a5d-ac96-774b-bcce-b302099a8057")
	for _, want := range []string{
		"BatchMode=yes", "StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "IdentityAgent=none",
		"ControlMaster=no", "ControlPath=none", "ForwardAgent=no", "ClearAllForwardings=yes",
		"ServerAliveInterval=15", "ServerAliveCountMax=3", "RequestTTY=no", "-T",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("attach argv lacks %s: %q", want, args)
		}
	}
	i := slices.Index(args, "--")
	if i < 0 || args[i+1] != "me@box" || args[i+2] != "konsertd attach 01890a5d-ac96-774b-bcce-b302099a8057" || len(args) != i+3 {
		t.Fatalf("argv must end with -- dest request: %q", args)
	}
	if k := slices.Index(args, "-i"); k < 0 || k > i || args[k+1] != "/db/konsert-hub-ssh-key" {
		t.Fatalf("-i must name the hub key before --: %q", args)
	}
	if p := slices.Index(args, "-p"); p < 0 || args[p+1] != "2222" {
		t.Fatalf("port: %q", args)
	}
}

// CommandConn carries bytes both ways over a child's stdio and reports its stderr.
func TestCommandConn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	c, err := StartCommandConn(exec.Command("sh", "-c", `read line; echo "got $line"; echo "Permission denied (publickey)." >&2`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(c)
	if string(out) != "got hello\n" {
		t.Fatalf("read %q", out)
	}
	<-c.Done()
	if !strings.Contains(c.Err(), "Permission denied") {
		t.Fatalf("Err() = %q", c.Err())
	}
	_ = c.Close()
	_ = c.Close()
}

func TestUninstallRemoteRunsKonsertdOverUserSSH(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	withFakeSSH(t, f)
	if _, err := UninstallRemote(context.Background(), "me@box", "hub1", testWorkerID); err != nil {
		t.Fatal(err)
	}
	cmds := f.remoteCmds()
	if indexOf(cmds, "/home/me/.local/bin/konsertd uninstall --worker "+testWorkerID+" --hub hub1") < 0 {
		t.Fatalf("cmds = %q", cmds)
	}
	for _, c := range f.calls {
		if slices.Contains(c, "IdentitiesOnly=yes") {
			t.Fatalf("removal uses the user's SSH access, not the hub key: %q", c)
		}
	}

	// No konsertd on the host: nothing to remove.
	f2 := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(cmd string, stdin []byte) (string, error) {
		if strings.Contains(cmd, "command -v konsertd") {
			return "missing:/home/me/.local/bin/konsertd\nmanaged:/home/me/.local/bin/konsertd\n", nil
		}
		return posixRemote(cmd, stdin)
	}}
	withFakeSSH(t, f2)
	if out, err := UninstallRemote(context.Background(), "me@box", "hub1", testWorkerID); err != nil || indexOf(f2.remoteCmds(), "uninstall") >= 0 || !strings.Contains(out, "not installed") {
		t.Fatalf("out=%q err=%v cmds=%q", out, err, f2.remoteCmds())
	}

	// An unreachable host is reported as such, so the hub retries later.
	f3 := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(string, []byte) (string, error) {
		return "", errors.New("ssh: connect to host box port 22: Operation timed out")
	}}
	withFakeSSH(t, f3)
	if _, err := UninstallRemote(context.Background(), "me@box", "hub1", testWorkerID); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("unreachable host: err = %v, want ErrUnreachable", err)
	}
}

// A Windows host runs the same uninstall through PowerShell.
func TestUninstallRemoteWindows(t *testing.T) {
	const exe = `C:\Users\me\AppData\Local\Programs\konsertd\konsertd.exe`
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	f.remote = func(cmd string, _ []byte) (string, error) {
		if strings.HasPrefix(cmd, "uname") {
			return "", errors.New("uname : The term 'uname' is not recognized")
		}
		script, ok := decodePS(cmd)
		if !ok {
			return "", fmt.Errorf("non-PowerShell command sent to Windows: %q", cmd)
		}
		switch {
		case script == windowsProbeScript:
			return "Win32NT\r\nAMD64\r\n", nil
		case strings.Contains(script, "Test-Path -LiteralPath $p) { \"found"):
			return "found:" + exe + "\r\nmanaged:" + exe + "\r\n", nil
		case strings.Contains(script, "'uninstall'"):
			return "Deleted scheduled task \\konsertd\r\nRemoved the scheduled task for win\r\n", nil
		}
		return "", nil
	}
	withFakeSSH(t, f)
	out, err := UninstallRemote(context.Background(), "me@win", "hub1", testWorkerID)
	if err != nil || !strings.Contains(out, "Removed the scheduled task") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if indexOf(f.remoteCmds(), "& '"+exe+"' 'uninstall' '--worker' '"+testWorkerID+"' '--hub' 'hub1'") < 0 {
		t.Fatalf("no konsertd uninstall on Windows: %q", f.remoteCmds())
	}
}
