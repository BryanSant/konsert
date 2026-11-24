package sshutil

import (
	"context"
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

func TestRemoveHubKeyRunsKonsertdOverUserSSH(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	withFakeSSH(t, f)
	if err := RemoveHubKey(context.Background(), "me@box", "hub1", testWorkerID); err != nil {
		t.Fatal(err)
	}
	cmds := f.remoteCmds()
	if indexOf(cmds, "/home/me/.local/bin/konsertd hub-key remove --hub hub1 --worker "+testWorkerID) < 0 {
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
	if err := RemoveHubKey(context.Background(), "me@box", "hub1", testWorkerID); err != nil || indexOf(f2.remoteCmds(), "hub-key") >= 0 {
		t.Fatalf("err=%v cmds=%q", err, f2.remoteCmds())
	}
}
