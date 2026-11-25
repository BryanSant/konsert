package sshutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// remoteCmds returns the remote command of every ssh call (PowerShell scripts decoded).
func (f *fakeSSH) remoteCmds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c[0] != "ssh" || slices.Contains(c, "-G") {
			continue
		}
		cmd := c[len(c)-1]
		if s, ok := decodePS(cmd); ok {
			cmd = "PS> " + s
		}
		out = append(out, cmd)
	}
	return out
}

func indexOf(cmds []string, sub string) int {
	return slices.IndexFunc(cmds, func(c string) bool { return strings.Contains(c, sub) })
}

func lastSCP(f *fakeSSH) []string {
	var scp []string
	for _, c := range f.calls {
		if c[0] == "scp" {
			scp = c
		}
	}
	return scp
}

func writeDist(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", name), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

func TestBootstrapUsesSyredInstall(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{
		Target:  "me@box",
		HubKey:  testGrant(),
		Sandbox: true,
	})
	if !res.OK || res.WorkerID != testWorkerID || res.Name != "box" || res.ServiceMode != "systemd" || res.StartDeferred {
		t.Fatalf("result = %+v", res)
	}
	cmds := f.remoteCmds()
	const bin = "/home/me/.local/bin/syred"
	id := indexOf(cmds, bin+" id --name box --host-group box")
	hooks := indexOf(cmds, bin+" install-hooks")
	inst := slices.Index(cmds, bin+" install --name box --host-group box --sandbox")
	if id < 0 || hooks < 0 || inst < 0 || id > inst {
		t.Fatalf("want syred id, install-hooks, then install; got %q", cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "systemctl") || strings.Contains(c, "nohup") || strings.Contains(c, "worker.yaml") {
			t.Fatalf("bootstrap must leave the service to syred install, ran %q", c)
		}
	}
}

func TestBootstrapCopiesMissingBinary(t *testing.T) {
	writeDist(t, "syred-linux-amd64")
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(cmd string, stdin []byte) (string, error) {
		if strings.Contains(cmd, "command -v syred") {
			return "missing:/home/me/.local/bin/syred\nmanaged:/home/me/.local/bin/syred\n", nil
		}
		return posixRemote(cmd, stdin)
	}}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "box"})
	if !res.OK {
		t.Fatalf("bootstrap failed: %+v", res)
	}
	scp := lastSCP(f)
	if scp == nil || !strings.HasSuffix(scp[len(scp)-2], filepath.Join("dist", "syred-linux-amd64")) || scp[len(scp)-1] != "box:~/.local/bin/syred.new" {
		t.Fatalf("scp = %q", scp)
	}
	cmds := f.remoteCmds()
	if indexOf(cmds, `chmod +x /home/me/.local/bin/syred.new && mv -f /home/me/.local/bin/syred.new /home/me/.local/bin/syred`) < 0 {
		t.Fatalf("copied binary not moved into place: %q", cmds)
	}
}

// A syred that differs from the hub's build is replaced (renamed over, so a running
// daemon is unaffected); a matching one is left alone.
func TestBootstrapUpdatesDifferentBinary(t *testing.T) {
	writeDist(t, "syred-linux-amd64")                                                  // contents "bin"
	const binHash = "9e8e1b5a6d4ffdc52ac7a0d1e2c4dd1b3d5a2b4f5e9d2a4e6b8c0d1e2f3a4b5c" // not "bin"
	for _, tc := range []struct {
		remoteHash string
		wantCopy   bool
	}{
		{binHash, true},
		{"5551c2a1c4c1b1a4d0e6b0f1a1fa38d1b1f5b9ec9c94e2cb0b4cf6ec1ebb0b8e", true},
		{"", false},
	} {
		want, _ := fileSHA256(filepath.Join("dist", "syred-linux-amd64"))
		remote := tc.remoteHash
		f := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(cmd string, stdin []byte) (string, error) {
			if strings.Contains(cmd, "sha256sum") {
				return remote + "  /usr/local/bin/syred\n", nil
			}
			if strings.Contains(cmd, "command -v syred") {
				return "found:/usr/local/bin/syred\nmanaged:/home/me/.local/bin/syred\n", nil
			}
			return posixRemote(strings.ReplaceAll(cmd, "/usr/local/bin/syred", "/home/me/.local/bin/syred"), stdin)
		}}
		withFakeSSH(t, f)
		res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "box"})
		if !res.OK {
			t.Fatalf("bootstrap failed: %+v", res)
		}
		copied := lastSCP(f) != nil
		if copied != tc.wantCopy || (copied && res.SyredPath != "/home/me/.local/bin/syred") || (!copied && res.SyredPath != "/usr/local/bin/syred") {
			t.Fatalf("remote hash %q: copied=%v path=%s", tc.remoteHash, copied, res.SyredPath)
		}
		// Same hash: nothing to do.
		f2 := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(cmd string, stdin []byte) (string, error) {
			if strings.Contains(cmd, "sha256sum") {
				return strings.ToUpper(want) + "\n", nil
			}
			return posixRemote(cmd, stdin)
		}}
		withFakeSSH(t, f2)
		if res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "box"}); !res.OK || lastSCP(f2) != nil {
			t.Fatalf("matching binary must not be copied: %+v", res)
		}
	}
}

func TestBootstrapFallsBackToNohupWithoutUserSystemd(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(cmd string, stdin []byte) (string, error) {
		if strings.Contains(cmd, " install --name") {
			return "", errors.New("Error: no systemd user instance is reachable (exit status 1).")
		}
		return posixRemote(cmd, stdin)
	}}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "box", HubKey: testGrant()})
	if !res.OK || res.ServiceMode != "nohup" || res.WorkerID != testWorkerID {
		t.Fatalf("result = %+v", res)
	}
	cmds := f.remoteCmds()
	// install failed before writing anything; an uninstall would remove the config and
	// unit of a host sharing this home directory.
	if indexOf(cmds, " uninstall") >= 0 {
		t.Fatalf("the nohup fallback must not run uninstall: %q", cmds)
	}
	cfg := indexOf(cmds, "syred-nohup-box.yaml.tmp")
	start := indexOf(cmds, "nohup /home/me/.local/bin/syred run -c")
	if cfg < 0 || start < 0 || !(cfg < start) || !strings.Contains(cmds[start], "syred-nohup-box.pid") {
		t.Fatalf("nohup fallback sequence wrong: %q", cmds)
	}
	for i, c := range f.calls {
		// The nohup daemon serves the hub on its local socket too: no controller URL.
		if strings.Contains(c[len(c)-1], "syred-nohup-box.yaml.tmp") && strings.Contains(string(f.stdins[i]), "controller") {
			t.Fatalf("nohup config must not carry a controller URL, got %q", f.stdins[i])
		}
	}
}

func TestBootstrapInstallFailureIsReported(t *testing.T) {
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob, remote: func(cmd string, stdin []byte) (string, error) {
		if strings.Contains(cmd, " install --name") {
			return "", errors.New("Error: launchctl bootstrap gui/501: Bootstrap failed: 125")
		}
		return posixRemote(cmd, stdin)
	}}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "box"})
	if res.OK || res.Stage != "configuring_service" || !strings.Contains(res.Error, "launchctl bootstrap") {
		t.Fatalf("result = %+v", res)
	}
}

func TestBootstrapWindows(t *testing.T) {
	writeDist(t, "syred-windows-amd64.exe")
	const exe = `C:\Users\me\AppData\Local\Programs\syred\syred.exe`
	var keyStdin []byte
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	f.remote = func(cmd string, stdin []byte) (string, error) {
		if strings.HasPrefix(cmd, "uname") {
			return "", errors.New("uname : The term 'uname' is not recognized as the name of a cmdlet")
		}
		script, ok := decodePS(cmd)
		if !ok {
			return "", fmt.Errorf("non-PowerShell command sent to Windows: %q", cmd)
		}
		switch {
		case script == windowsProbeScript:
			return "Win32NT\r\nAMD64\r\n", nil
		case strings.Contains(script, "Test-Path -LiteralPath $p) { \"found"):
			return "missing:" + exe + "\r\nmanaged:" + exe + "\r\n", nil
		case strings.Contains(script, "'id'"):
			return testWorkerID + "\r\n", nil
		case strings.Contains(script, "'hub-key'"):
			keyStdin = stdin
			return `hub key allowed in C:\ProgramData\ssh\administrators_authorized_keys`, nil
		case strings.Contains(script, "'install'"):
			return "Registered scheduled task", nil
		case strings.Contains(script, "'status'"):
			return "syred \\syred\r\n  Installed  : yes\r\n  Running    : no\r\n", nil
		}
		return "", nil
	}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "bryan@winbox", HubKey: testGrant()})
	if !res.OK || res.RemoteOS != "windows" || res.RemoteArch != "amd64" || res.ServiceMode != "schtasks" ||
		res.WorkerID != testWorkerID || !res.StartDeferred || !strings.Contains(res.Message, "next Windows login") {
		t.Fatalf("result = %+v", res)
	}
	scp := lastSCP(f)
	if scp == nil || scp[len(scp)-1] != "bryan@winbox:/C:/Users/me/AppData/Local/Programs/syred/syred.exe.new" {
		t.Fatalf("scp = %q", scp)
	}
	cmds := f.remoteCmds()
	key := indexOf(cmds, "'hub-key' 'add' '--hub' 'hub1' '--worker' '"+testWorkerID+"'")
	inst := indexOf(cmds, "& '"+exe+"' 'install' '--name' 'winbox' '--host-group' 'winbox'")
	if key < 0 || inst < 0 || key > inst || !strings.Contains(cmds[key], "$line = [Console]::In.ReadLine()") ||
		!strings.Contains(cmds[key], "$line | & '"+exe+"'") || strings.Contains(cmds[inst], "--controller") {
		t.Fatalf("scripts = %q", cmds)
	}
	if string(keyStdin) != testHubPub+"\n" {
		t.Fatalf("hub key stdin = %q", keyStdin)
	}
	if indexOf(cmds, "New-Item -ItemType Directory") < 0 || indexOf(cmds, "'install-hooks'") < 0 || indexOf(cmds, `Move-Item -LiteralPath "$p.new"`) < 0 {
		t.Fatalf("missing mkdir, move into place or install-hooks: %q", cmds)
	}
	// The status check starts the task now and waits for the daemon.
	if st := indexOf(cmds, "'status' '--name' 'winbox' '--start' '--wait' '20s'"); st < inst {
		t.Fatalf("no status --start after install: %q", cmds)
	}
}

// After an upgrade the task comes up within the wait: the bootstrap says it runs, not
// that it starts at the next login.
func TestBootstrapWindowsReportsRunning(t *testing.T) {
	writeDist(t, "syred-windows-amd64.exe")
	const exe = `C:\Users\me\AppData\Local\Programs\syred\syred.exe`
	f := &fakeSSH{knownKey: true, hostBlob: testHostBlob}
	f.remote = func(cmd string, stdin []byte) (string, error) {
		if strings.HasPrefix(cmd, "uname") {
			return "", errors.New("uname : The term 'uname' is not recognized")
		}
		script, _ := decodePS(cmd)
		switch {
		case script == windowsProbeScript:
			return "Win32NT\r\nAMD64\r\n", nil
		case strings.Contains(script, "Test-Path -LiteralPath $p) { \"found"):
			return "missing:" + exe + "\r\nmanaged:" + exe + "\r\n", nil
		case strings.Contains(script, "'id'"):
			return testWorkerID + "\r\n", nil
		case strings.Contains(script, "'status'") && strings.Contains(script, "'--start'"):
			return "syred \\syred-winbox\r\n  Installed  : yes\r\n  Running    : yes\r\n  State      : Ready (daemon answering)\r\n", nil
		case strings.Contains(script, "'status'"):
			return "syred \\syred-winbox\r\n  Installed  : yes\r\n  Running    : no\r\n", nil
		}
		return "", nil
	}
	withFakeSSH(t, f)
	res := BootstrapRemoteWorker(context.Background(), BootstrapOptions{Target: "bryan@winbox"})
	if !res.OK || res.StartDeferred || strings.Contains(res.Message, "next Windows login") {
		t.Fatalf("result = %+v", res)
	}
	var detail string
	for _, s := range res.Stages {
		if s.Stage == "configuring_service" {
			detail = s.Detail
		}
	}
	if detail != "running (started via schtasks)" {
		t.Fatalf("stage detail %q", detail)
	}
}

func TestParseWindowsProbe(t *testing.T) {
	cases := map[string]string{
		"Win32NT\r\nAMD64\r\n":   "amd64",
		"  Win32NT\nARM64\n":     "arm64",
		"Win32NT\r\n\r\nx86\r\n": "386",
	}
	for in, want := range cases {
		if got, ok := parseWindowsProbe(in); !ok || got != want {
			t.Errorf("parseWindowsProbe(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "Unix\nx86_64", "Win32NT"} {
		if _, ok := parseWindowsProbe(bad); ok {
			t.Errorf("parseWindowsProbe(%q) accepted", bad)
		}
	}
	if s, ok := decodePS(encodePS("é $env:X 'q'")); !ok || s != "é $env:X 'q'" {
		t.Fatalf("encodePS round trip = %q %v", s, ok)
	}
	if !notRunning("  Running    : no\n") || notRunning("  Running    : yes (pid 4)\n") {
		t.Fatal("notRunning misparses syred status")
	}
	if !isWindowsUname("mingw64_nt-10.0-26100") || isWindowsUname("linux") {
		t.Fatal("isWindowsUname")
	}
}
