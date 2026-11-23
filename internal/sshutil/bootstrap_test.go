package sshutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestParseTargetRejectsInjection(t *testing.T) {
	bad := []string{
		"",
		"-oProxyCommand=sh -c 'id>/tmp/pwn'",
		"-oProxyCommand=id",
		"-p",
		"host -oProxyCommand=id",
		"host\tother",
		"host\nother",
		"user@-oProxyCommand=id",
		"-user@host",
		"us er@host",
		"host;id",
		"host$(id)",
		"host`id`",
		"a@b@c",
		"host:0",
		"host:70000",
		"host:22x",
		"[::1",
		"[1.2.3.4]:22",
		"hóst",
	}
	for _, in := range bad {
		if _, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) accepted an unsafe target", in)
		}
	}
}

func TestParseTargetAcceptsNormalShapes(t *testing.T) {
	cases := map[string]Target{
		"bocabox":               {Host: "bocabox"},
		"bsant@bocabox.local":   {User: "bsant", Host: "bocabox.local"},
		"daybreak-wsl:2222":     {Host: "daybreak-wsl", Port: "2222"},
		"me@10.0.0.5:22":        {User: "me", Host: "10.0.0.5", Port: "22"},
		"[fe80::1]:2200":        {Host: "fe80::1", Port: "2200"},
		"fe80::1":               {Host: "fe80::1"},
		"  starscream.example ": {Host: "starscream.example"},
	}
	for in, want := range cases {
		got, err := ParseTarget(in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestSSHAndSCPArgvSeparateTargetWithDoubleDash(t *testing.T) {
	tgt := Target{User: "me", Host: "fe80::1", Port: "2200"}
	args := sshArgs(tgt, nil, "uname -s")
	i := slices.Index(args, "--")
	if i < 0 || args[i+1] != "me@fe80::1" || args[i+2] != "uname -s" || len(args) != i+3 {
		t.Fatalf("ssh argv must end with -- dest cmd: %q", args)
	}
	if !slices.Contains(args, "StrictHostKeyChecking=yes") || slices.Contains(args, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("ssh must use strict host key checking: %q", args)
	}
	if j := slices.Index(args, "-p"); j < 0 || j > i || args[j+1] != "2200" {
		t.Fatalf("port option must precede --: %q", args)
	}

	sargs := scpArgs(tgt, nil, "dist/konsertd", "~/.local/bin/konsertd")
	k := slices.Index(sargs, "--")
	if k < 0 || sargs[k+1] != "dist/konsertd" || sargs[k+2] != "me@[fe80::1]:~/.local/bin/konsertd" {
		t.Fatalf("scp argv must end with -- src dest: %q", sargs)
	}
	if j := slices.Index(sargs, "-P"); j < 0 || j > k || sargs[j+1] != "2200" {
		t.Fatalf("scp port option must precede --: %q", sargs)
	}
}

func TestFingerprintSHA256(t *testing.T) {
	// `ssh-keygen -lf` prints this fingerprint for the ed25519 key blob below.
	const want = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"
	const blob = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	fp, err := FingerprintSHA256(blob)
	if err != nil {
		t.Fatal(err)
	}
	if fp != want {
		t.Fatalf("fingerprint = %q, want %q", fp, want)
	}
	if normalizeFingerprint(strings.TrimPrefix(fp, "SHA256:")+"=") != fp {
		t.Fatal("normalizeFingerprint must accept bare and padded forms")
	}
	if _, err := FingerprintSHA256("not base64!"); err == nil {
		t.Fatal("invalid blob must error")
	}
}

func TestClassifyHostKeyError(t *testing.T) {
	if classifyHostKeyError(errors.New("No ED25519 host key is known for box and you have requested strict checking.\nHost key verification failed.")) != hostKeyUnknown {
		t.Fatal("unknown key not detected")
	}
	if classifyHostKeyError(errors.New("@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@\nHost key verification failed.")) != hostKeyChanged {
		t.Fatal("changed key must be classified as changed, not unknown")
	}
	if classifyHostKeyError(errors.New("Connection refused")) != hostKeyOK {
		t.Fatal("unrelated errors are not host key problems")
	}
}

// fakeSSH records every invocation and simulates a remote host.
type fakeSSH struct {
	mu        sync.Mutex
	calls     [][]string
	stdins    [][]byte
	knownKey  bool // host key already trusted
	hostBlob  string
	trustFile string // set when UserKnownHostsFile override is used
}

func (f *fakeSSH) run(_ context.Context, name string, args []string, stdin []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.stdins = append(f.stdins, stdin)
	switch name {
	case "ssh-keyscan":
		return "# box:22 SSH-2.0-OpenSSH\nbox ssh-ed25519 " + f.hostBlob + "\n", nil
	case "scp":
		return "", nil
	}
	if slices.Contains(args, "-G") {
		return "user me\nhostname box\nport 22\n", nil
	}
	trusted := f.knownKey
	for _, a := range args {
		if strings.HasPrefix(a, "UserKnownHostsFile=") {
			f.trustFile = strings.Trim(strings.TrimPrefix(a, "UserKnownHostsFile="), `"`)
			if b, err := os.ReadFile(f.trustFile); err == nil && strings.Contains(string(b), f.hostBlob) {
				trusted = true
			}
		}
	}
	if !trusted {
		return "", errors.New("No ED25519 host key is known for box and you have requested strict checking.\nHost key verification failed.")
	}
	cmd := args[len(args)-1]
	switch {
	case strings.HasPrefix(cmd, "uname"):
		return "Linux\nx86_64\n", nil
	case strings.Contains(cmd, "systemctl"):
		return "systemd\n", nil
	}
	return "", nil
}

func withFakeSSH(t *testing.T, f *fakeSSH) {
	t.Helper()
	prevRun, prevKH := run, userKnownHostsPath
	kh := filepath.Join(t.TempDir(), "known_hosts")
	run = f.run
	userKnownHostsPath = func() (string, error) { return kh, nil }
	t.Cleanup(func() { run, userKnownHostsPath = prevRun, prevKH })
}

const testHostBlob = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

func TestBootstrapNeverPutsTokenInArgv(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	withFakeSSH(t, f)
	const token = "fleet-super-secret-token"
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{
		Target:        "me@box",
		ControllerURL: "wss://konsert.example.ts.net/ws/worker",
		FleetToken:    token,
	})
	// The fake reports konsertd as already installed, so no scp happens here.
	if !res.OK {
		t.Fatalf("bootstrap failed: %+v", res)
	}
	sawStdin := false
	for i, c := range f.calls {
		for _, a := range c {
			if strings.Contains(a, token) {
				t.Fatalf("fleet token leaked into argv: %q", c)
			}
		}
		if strings.Contains(string(f.stdins[i]), token) {
			sawStdin = true
			if !strings.Contains(c[len(c)-1], `cat > "$HOME/.konsert/worker.yaml.tmp"`) {
				t.Fatalf("token stdin sent to unexpected command %q", c)
			}
		}
		if c[0] == "ssh" && !slices.Contains(c, "--") {
			t.Fatalf("ssh invocation without --: %q", c)
		}
	}
	if !sawStdin {
		t.Fatal("worker config with token must be written over stdin")
	}
}

func TestBootstrapRejectsInjectedTargetWithoutRunningSSH(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "-oProxyCommand=sh -c 'id'"})
	if res.OK || len(f.calls) != 0 {
		t.Fatalf("injected target must be rejected before exec: res=%+v calls=%q", res, f.calls)
	}
}

func TestBootstrapUnknownHostKeyRequiresConfirmation(t *testing.T) {
	f := &fakeSSH{knownKey: false, hostBlob: testHostBlob}
	withFakeSSH(t, f)
	fp, _ := FingerprintSHA256(testHostBlob)

	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "me@box", FleetToken: "t"})
	if res.OK || !res.HostKeyConfirmationRequired || res.Stage != StageHostKeyConfirmation {
		t.Fatalf("expected host key confirmation, got %+v", res)
	}
	if len(res.HostKeys) != 1 || res.HostKeys[0].Fingerprint != fp || res.HostKeys[0].Type != "ssh-ed25519" {
		t.Fatalf("unexpected host keys %+v", res.HostKeys)
	}
	if !strings.Contains(res.Error, fp) {
		t.Fatalf("error should name the fingerprint for older UIs: %q", res.Error)
	}

	// A wrong confirmation is refused.
	res = BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "me@box", AcceptHostKey: "SHA256:wrong"})
	if res.OK || !strings.Contains(res.Error, "does not match") {
		t.Fatalf("mismatched fingerprint must fail, got %+v", res)
	}

	// The confirmed fingerprint lets bootstrap proceed and is persisted.
	res = BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "me@box", AcceptHostKey: fp, FleetToken: "t"})
	if !res.OK {
		t.Fatalf("confirmed bootstrap failed: %+v", res)
	}
	kh, _ := userKnownHostsPath()
	b, err := os.ReadFile(kh)
	if err != nil || !strings.Contains(string(b), "box ssh-ed25519 "+testHostBlob) {
		t.Fatalf("confirmed key not persisted to known_hosts: %q err=%v", b, err)
	}
	if info, _ := os.Stat(kh); info.Mode().Perm() != 0o600 {
		t.Fatalf("known_hosts mode = %o", info.Mode().Perm())
	}
	if _, err := os.Stat(f.trustFile); !os.IsNotExist(err) {
		t.Fatal("temporary known_hosts must be cleaned up")
	}
}

func TestBootstrapChangedHostKeyIsFatal(t *testing.T) {
	prev := run
	t.Cleanup(func() { run = prev })
	var calls int
	run = func(_ context.Context, name string, _ []string, _ []byte) (string, error) {
		calls++
		return "", errors.New("@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@\nHost key verification failed.")
	}
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "box", AcceptHostKey: "SHA256:anything"})
	if res.OK || !strings.Contains(res.Error, "CHANGED") || calls != 1 {
		t.Fatalf("changed host key must abort without keyscan: res=%+v calls=%d", res, calls)
	}
}
