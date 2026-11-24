package worker

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/install"
)

// recRunner records the service commands an uninstall runs.
type recRunner struct{ calls []string }

func (r *recRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return "", nil
}

// sharedAccount is one home directory shared by several hosts: a config dir, a state
// dir, and recorded side effects.
type sharedAccount struct {
	t        *testing.T
	state    string
	runner   *recRunner
	in       *install.Installer
	hooksRm  int
	grants   []string
	killed   []int
	procArgs map[int]string
}

func newSharedAccount(t *testing.T) *sharedAccount {
	t.Helper()
	root := t.TempDir()
	a := &sharedAccount{t: t, state: filepath.Join(root, "state"), runner: &recRunner{}, procArgs: map[int]string{}}
	a.in = &install.Installer{
		Runner: a.runner,
		Out:    &bytes.Buffer{},
		Spawn:  func(string, ...string) error { return nil },
		Env: install.Env{
			Home: filepath.Join(root, "home"), ConfigDir: filepath.Join(root, "config"),
			LogDir: filepath.Join(root, "logs"), UnitDir: filepath.Join(root, "units"), UID: 501, User: "me",
		},
	}
	return a
}

func (a *sharedAccount) uninstaller(self bool) *Uninstaller {
	return &Uninstaller{
		Installer: a.in, StateDir: a.state, Self: self, LockWait: 300 * time.Millisecond,
		RemoveHooks: func() ([]string, error) {
			a.hooksRm++
			return []string{"removed the Konsert hooks from ~/.claude/settings.json"}, nil
		},
		DropHubGrant: func(hubID, workerID string) (string, bool, error) {
			a.grants = append(a.grants, hubID+"/"+workerID)
			return "/home/me/.ssh/authorized_keys", true, nil
		},
		ProcArgs: func(pid int) (string, error) { return a.procArgs[pid], nil },
		Kill:     func(pid int) error { a.killed = append(a.killed, pid); return nil },
	}
}

// identity writes name's identity file and returns its worker id.
func (a *sharedAccount) identity(name string) string {
	a.t.Helper()
	id := ids.UUID()
	raw, _ := json.Marshal(identityRecord{ID: id})
	if err := os.MkdirAll(a.state, 0o700); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.state, workerIDFilename(name, "")), raw, 0o600); err != nil {
		a.t.Fatal(err)
	}
	return id
}

// service installs name as a service instance (its config in the config dir).
func (a *sharedAccount) service(instance, name string) string {
	a.t.Helper()
	p := a.in.ConfigPath(instance)
	if err := install.WriteSecretFile(p, []byte("name: "+name+"\n")); err != nil {
		a.t.Fatal(err)
	}
	return p
}

// nohup installs name as a nohup daemon with pid.
func (a *sharedAccount) nohup(name string, pid int) string {
	a.t.Helper()
	base := filepath.Join(a.state, install.NohupBase(name))
	if err := os.WriteFile(base+".yaml", []byte("name: "+name+"\n"), 0o600); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		a.t.Fatal(err)
	}
	a.procArgs[pid] = "/home/me/.local/bin/konsertd run -c " + base + ".yaml"
	return base + ".yaml"
}

// A host and two containers share one home (bocabox, bocabox-1, bocabox-2): removing a
// container removes only its own daemon, and the agent hooks stay until the last
// konsertd on the account goes.
func TestRemoveIdentitySharedHomeKeepsHooksUntilLast(t *testing.T) {
	a := newSharedAccount(t)
	host := a.identity("bocabox")
	box1 := a.identity("bocabox-1")
	box2 := a.identity("bocabox-2")
	hostCfg := a.service("", "bocabox")
	box1Cfg := a.nohup("bocabox-1", 4101)
	box2Cfg := a.nohup("bocabox-2", 4102)
	if err := os.MkdirAll(filepath.Join(a.state, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(CredentialPath(a.state, box1), []byte("cred"), 0o600)

	rep, err := a.uninstaller(false).RemoveIdentity(context.Background(), box1, "hub1")
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(box1Cfg) || fileExists(strings.TrimSuffix(box1Cfg, ".yaml")+".pid") || fileExists(CredentialPath(a.state, box1)) {
		t.Error("box1's nohup files or credential left behind")
	}
	if len(a.killed) != 1 || a.killed[0] != 4101 {
		t.Errorf("killed = %v, want box1's pid only", a.killed)
	}
	if !fileExists(hostCfg) || !fileExists(box2Cfg) {
		t.Fatal("another identity's daemon was removed")
	}
	if len(a.runner.calls) != 0 {
		t.Errorf("the host's service was touched: %v", a.runner.calls)
	}
	if a.hooksRm != 0 || !strings.Contains(strings.Join(rep.Kept, "\n"), "bocabox, bocabox-2") {
		t.Errorf("hooks removed=%d kept=%q", a.hooksRm, rep.Kept)
	}
	if len(a.grants) != 1 || a.grants[0] != "hub1/"+box1 {
		t.Errorf("grants dropped = %v", a.grants)
	}

	if _, err := a.uninstaller(false).RemoveIdentity(context.Background(), box2, "hub1"); err != nil {
		t.Fatal(err)
	}
	if a.hooksRm != 0 {
		t.Error("hooks removed while the host's konsertd remains")
	}

	rep, err = a.uninstaller(false).RemoveIdentity(context.Background(), host, "hub1")
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(hostCfg) || len(a.runner.calls) == 0 || rep.Services != 1 {
		t.Errorf("host service not removed: calls=%v rep=%+v", a.runner.calls, rep)
	}
	if a.hooksRm != 1 {
		t.Errorf("hooks removed %d times, want once with the last identity", a.hooksRm)
	}
}

// The daemon removing itself never signals its own nohup process, and uses the
// self-safe service removal.
func TestRemoveIdentitySelf(t *testing.T) {
	a := newSharedAccount(t)
	box := a.identity("box")
	cfg := a.nohup("box", 4200)
	rep, err := a.uninstaller(true).RemoveIdentity(context.Background(), box, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.killed) != 0 {
		t.Errorf("self-removal signalled pid %v", a.killed)
	}
	if fileExists(cfg) || a.hooksRm != 1 || rep.Services != 0 {
		t.Errorf("rep = %+v hooks=%d", rep, a.hooksRm)
	}

	host := a.identity("host")
	a.service("", "host")
	rep, err = a.uninstaller(true).RemoveIdentity(context.Background(), host, "")
	if err != nil || rep.Services != 1 {
		t.Fatalf("rep = %+v, %v", rep, err)
	}
	for _, c := range a.runner.calls {
		if strings.Contains(c, "--now") || strings.HasPrefix(c, "schtasks /End") || c == "launchctl bootout gui/501/dev.konsert.konsertd" {
			t.Errorf("blocking stop from inside the daemon: %q", c)
		}
	}
}

// A pid file whose pid now belongs to another process is not signalled.
func TestRemoveIdentityIgnoresReusedPID(t *testing.T) {
	a := newSharedAccount(t)
	box := a.identity("box")
	a.nohup("box", 4300)
	a.procArgs[4300] = "/usr/bin/vim notes.txt"
	if _, err := a.uninstaller(false).RemoveIdentity(context.Background(), box, ""); err != nil {
		t.Fatal(err)
	}
	if len(a.killed) != 0 {
		t.Fatalf("killed a reused pid: %v", a.killed)
	}
}

// Nothing installed (the daemon already removed itself): nothing is touched, and the
// hooks stay even though no other konsertd is installed.
func TestRemoveIdentityNothingInstalled(t *testing.T) {
	a := newSharedAccount(t)
	id := a.identity("gone")
	u := a.uninstaller(false)
	u.DropHubGrant = func(string, string) (string, bool, error) { return "", false, nil }
	rep, err := u.RemoveIdentity(context.Background(), id, "hub1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Found || a.hooksRm != 0 || len(a.runner.calls) != 0 {
		t.Fatalf("rep=%+v hooks=%d calls=%v", rep, a.hooksRm, a.runner.calls)
	}
	if l := rep.Lines(); len(l) != 1 || !strings.Contains(l[0], "already removed") {
		t.Fatalf("lines = %q", l)
	}
}

func TestRemoveIdentityKeepHooks(t *testing.T) {
	a := newSharedAccount(t)
	id := a.identity("solo")
	a.service("", "solo")
	u := a.uninstaller(false)
	u.KeepHooks = true
	rep, err := u.RemoveIdentity(context.Background(), id, "")
	if err != nil || a.hooksRm != 0 || !strings.Contains(strings.Join(rep.Kept, ""), "--keep-hooks") {
		t.Fatalf("rep=%+v err=%v hooks=%d", rep, err, a.hooksRm)
	}
}

// Removals on one account are serialized; a stale lock (a crashed removal) is ignored.
func TestRemoveIdentityLock(t *testing.T) {
	a := newSharedAccount(t)
	id := a.identity("solo")
	_ = os.MkdirAll(a.state, 0o700)
	lock := filepath.Join(a.state, "uninstall.lock")
	if err := os.WriteFile(lock, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.uninstaller(false).RemoveIdentity(context.Background(), id, ""); err == nil || !strings.Contains(err.Error(), "another konsertd removal") {
		t.Fatalf("err = %v, want a busy lock", err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(lock, old, old)
	if _, err := a.uninstaller(false).RemoveIdentity(context.Background(), id, ""); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	if fileExists(lock) {
		t.Error("lock not released")
	}
}

// A successful removal clears the removed marker; one that failed keeps it, so the
// daemon retries at its next start.
func TestRemoveIdentityClearsMarker(t *testing.T) {
	a := newSharedAccount(t)
	id := a.identity("solo")
	a.service("", "solo")
	if err := WriteRemovedMarker(a.state, id, "hub1"); err != nil {
		t.Fatal(err)
	}
	if hub, ok := ReadRemovedMarker(a.state, id); !ok || hub != "hub1" {
		t.Fatalf("marker = %q, %v", hub, ok)
	}
	if _, err := a.uninstaller(true).RemoveIdentity(context.Background(), id, "hub1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadRemovedMarker(a.state, id); ok {
		t.Fatal("marker left after a successful removal")
	}
}

// Identity files are keyed by name: containers sharing a home and a machine id
// (bocabox-1, bocabox-2) and their host each get their own identity.
func TestWorkerIDFilesPerNameOnSharedHome(t *testing.T) {
	dir := t.TempDir()
	a := ResolveStableWorkerID(dir, "bocabox-1", "bocabox-1", "")
	b := ResolveStableWorkerID(dir, "bocabox-2", "bocabox-2", "")
	h := ResolveStableWorkerID(dir, "BocaBox", "BocaBox", "")
	if a == b || a == h || b == h {
		t.Fatalf("shared identities: %s %s %s", a, b, h)
	}
	if again := ResolveStableWorkerID(dir, "bocabox-1", "bocabox-1", ""); again != a {
		t.Fatal("identity not stable")
	}
	if got, ok := lookupWorkerIDIn(dir, configForName("bocabox")); !ok || got != h {
		t.Fatalf("another host reads the host's identity as %q, %v; want %s", got, ok, h)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func configForName(name string) config.WorkerConfig { return config.WorkerConfig{Name: name} }

// A container's identity file carries its isolation; the installed config is read like
// `konsertd run` reads it, so the removal finds it.
func TestRemoveIdentityFindsContainerIdentity(t *testing.T) {
	a := newSharedAccount(t)
	id := ids.UUID()
	raw, _ := json.Marshal(identityRecord{ID: id})
	_ = os.MkdirAll(a.state, 0o700)
	if err := os.WriteFile(filepath.Join(a.state, workerIDFilename("box", "container")), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(a.state, install.NohupBase("box")+".yaml")
	if err := os.WriteFile(cfg, []byte("name: box\nisolation: container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := a.uninstaller(false).RemoveIdentity(context.Background(), id, "")
	if err != nil || !rep.Found || fileExists(cfg) {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}
