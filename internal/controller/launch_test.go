package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/sshutil"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
	"github.com/coder/websocket"
)

const (
	localID  = "01890a5d-ac96-774b-bcce-b302099a8057"
	remoteID = "01890a5d-ac96-774b-bcce-b302099a8058"
	otherID  = "01890a5d-ac96-774b-bcce-b302099a8059"
)

// fakeRunner stands in for local commands: it never installs anything.
type fakeRunner struct {
	mu        sync.Mutex
	calls     [][]string
	stdins    []string
	id        string
	installFn func(args []string) error // optional install behaviour
}

func (f *fakeRunner) Run(_ context.Context, stdin, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.stdins = append(f.stdins, stdin)
	fn := f.installFn
	f.mu.Unlock()
	if len(args) > 0 && args[0] == "id" {
		return f.id + "\n", nil
	}
	if len(args) > 0 && args[0] == "install" && fn != nil {
		return "", fn(args)
	}
	return "", nil
}

func (f *fakeRunner) subcommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c[1])
	}
	return out
}

func (f *fakeRunner) installCalls() ([][]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls [][]string
	var stdins []string
	for i, c := range f.calls {
		if c[1] == "install" {
			calls = append(calls, c)
			stdins = append(stdins, f.stdins[i])
		}
	}
	return calls, stdins
}

type launchTest struct {
	srv    *Server
	ts     *httptest.Server
	st     *sqlite.SQLiteStore
	runner *fakeRunner

	mu        sync.Mutex
	bootstrap []sshutil.BootstrapOptions
	bootRes   func(sshutil.BootstrapOptions) sshutil.BootstrapResult

	// daemons are the fake syreds the hub can attach to, by worker id.
	daemons *fakeDaemons
}

func newLaunchTest(t *testing.T) *launchTest {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "syre.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	srv.hubKeyFile = filepath.Join(t.TempDir(), HubKeyFileName)
	lt := &launchTest{srv: srv, st: st, runner: &fakeRunner{id: localID}}
	l := srv.launch
	l.runner = lt.runner
	l.findSyred = func() string { return "/opt/syre/bin/syred" }
	l.hostname = func() string { return "hubhost" }
	l.connectTimeout = 2 * time.Second
	l.startChild = func(context.Context, string, []string, string) error {
		t.Error("startChild called outside dry-run")
		return nil
	}
	l.bootstrap = func(_ context.Context, o sshutil.BootstrapOptions) sshutil.BootstrapResult {
		lt.mu.Lock()
		lt.bootstrap = append(lt.bootstrap, o)
		fn := lt.bootRes
		lt.mu.Unlock()
		if fn != nil {
			return fn(o)
		}
		return sshutil.BootstrapResult{OK: false, Target: o.Target, Error: "unreachable"}
	}
	// Attaching (ssh or local socket) reaches in-process fake daemons, never a real ssh.
	lt.daemons = newFakeDaemons(t)
	srv.attach.dialSSH = func(_ context.Context, _, _, workerID string) (net.Conn, error) { return lt.daemons.dial(workerID) }
	srv.attach.dialLocal = func(_ context.Context, workerID string) (net.Conn, error) { return lt.daemons.dial(workerID) }
	srv.attach.minBackoff, srv.attach.maxBackoff = 20*time.Millisecond, 100*time.Millisecond
	// Automatic restarts (restart.go) stay off unless a test turns them on.
	srv.hub.restarts.holdUntil(time.Now().Add(time.Hour))
	lt.ts = httptest.NewServer(srv)
	srv.SetListenAddr("127.0.0.1:8420")
	t.Cleanup(func() {
		srv.Close()
		lt.ts.Close()
		_ = st.Close()
	})
	return lt
}

func (lt *launchTest) bootstraps() []sshutil.BootstrapOptions {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return slices.Clone(lt.bootstrap)
}

// dial connects to /ws/worker as a fake syred (over loopback, like httptest) and
// keeps the connection open until the test ends. Safe to call from any goroutine.
func (lt *launchTest) dial(t *testing.T, info protocol.WorkerInfo) error {
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(lt.ts.URL, "http")+"/ws/worker", nil)
	if err != nil {
		cancel()
		return err
	}
	raw, _ := protocol.NewEnvelope(protocol.MsgWorkerHello, "", protocol.WorkerHelloPayload{Worker: info})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		cancel()
		return err
	}
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		cancel()
	})
	return nil
}

// connect dials and waits until the hub registers the worker.
func (lt *launchTest) connect(t *testing.T, info protocol.WorkerInfo) {
	t.Helper()
	if err := lt.dial(t, info); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "worker "+info.ID+" to connect", func() bool { return lt.srv.hub.IsWorkerConnected(info.ID) })
}

func (lt *launchTest) do(t *testing.T, method, path string) (int, map[string]any) {
	t.Helper()
	resp := doReq(t, method, lt.ts.URL+path, strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"})
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func (lt *launchTest) worker(t *testing.T, id string) store.Worker {
	t.Helper()
	w, err := lt.st.GetWorker(context.Background(), id)
	if err != nil {
		t.Fatalf("worker %s: %v", id, err)
	}
	return w
}

func TestSetupStatus(t *testing.T) {
	lt := newLaunchTest(t)
	code, body := lt.do(t, http.MethodGet, "/api/setup")
	local, _ := body["local"].(map[string]any)
	if code != 200 || body["needs_setup"] != true || local["hostname"] != "hubhost" || local["syred_found"] != true ||
		local["worker_id"] != localID || local["connected"] != false || local["syred_path"] != "/opt/syre/bin/syred" {
		t.Fatalf("fresh hub: %d %v", code, body)
	}
	if got := lt.runner.calls[0]; strings.Join(got, " ") != "/opt/syre/bin/syred id --name hubhost" {
		t.Fatalf("syred id invocation = %q", got)
	}

	// Dismissing the first-run dialog is persisted by the hub.
	if code, _ := lt.do(t, http.MethodPost, "/api/setup/dismiss"); code != 200 {
		t.Fatalf("dismiss: %d", code)
	}
	if _, body := lt.do(t, http.MethodGet, "/api/setup"); body["needs_setup"] != false {
		t.Fatalf("after dismiss: %v", body)
	}

	// No syred: setup says how to fix it.
	lt2 := newLaunchTest(t)
	lt2.srv.launch.findSyred = func() string { return "" }
	_, body = lt2.do(t, http.MethodGet, "/api/setup")
	local, _ = body["local"].(map[string]any)
	if local["syred_found"] != false || !strings.Contains(local["fix"].(string), "PATH") || len(lt2.runner.calls) != 0 {
		t.Fatalf("no syred: %v", body)
	}

	// A machine row (any daemon that connected) means the hub is set up.
	lt3 := newLaunchTest(t)
	lt3.connect(t, testWorkerInfo(remoteID, "elsewhere"))
	if _, body := lt3.do(t, http.MethodGet, "/api/setup"); body["needs_setup"] != false {
		t.Fatalf("with a machine: %v", body)
	}
}

func TestAddLocalAttachesRunningDaemonWithoutInstall(t *testing.T) {
	lt := newLaunchTest(t)
	// A daemon run by hand with a custom --name on this host, connected over loopback.
	info := testWorkerInfo(otherID, "custom-name")
	info.Hostname = "HubHost.local"
	lt.connect(t, info)

	_, body := lt.do(t, http.MethodGet, "/api/setup")
	if local := body["local"].(map[string]any); local["connected"] != true || local["worker_id"] != otherID {
		t.Fatalf("setup should see the running local daemon: %v", body)
	}
	code, body := lt.do(t, http.MethodPost, "/api/workers/local")
	if code != 200 || body["state"] != "connected" || body["worker_id"] != otherID {
		t.Fatalf("attach: %d %v", code, body)
	}
	if slices.Contains(lt.runner.subcommands(), "install") {
		t.Fatalf("attaching must not install anything: %q", lt.runner.calls)
	}
	if w := lt.worker(t, otherID); w.Launch != store.LaunchLocal || w.Name != "custom-name" || w.Status != "online" {
		t.Fatalf("attached worker = %+v", w)
	}
	if _, body := lt.do(t, http.MethodGet, "/api/setup"); body["needs_setup"] != false || body["local"].(map[string]any)["added"] != true {
		t.Fatalf("after attach: %v", body)
	}
}

func TestAddLocalStartsSyredInstall(t *testing.T) {
	lt := newLaunchTest(t)
	lt.runner.installFn = func([]string) error {
		// The installed daemon comes up on its socket; the hub attaches.
		lt.daemons.up(testWorkerInfo(localID, "hubhost"))
		return nil
	}
	code, body := lt.do(t, http.MethodPost, "/api/workers/local")
	if code != http.StatusAccepted || body["state"] != "starting" || body["worker_id"] != localID || !strings.HasPrefix(body["worker_sid"].(string), "m-") {
		t.Fatalf("add local: %d %v", code, body)
	}
	waitFor(t, 5*time.Second, "local daemon to connect", func() bool { return lt.srv.hub.IsWorkerConnected(localID) })
	waitFor(t, 3*time.Second, "launch state to clear", func() bool {
		w := lt.worker(t, localID)
		return w.LaunchState == store.LaunchStateNone && w.LaunchAt != nil
	})
	calls, stdins := lt.runner.installCalls()
	want := []string{"/opt/syre/bin/syred", "install", "--name", "hubhost"}
	if len(calls) != 1 || !slices.Equal(calls[0], want) || stdins[0] != "" {
		t.Fatalf("install calls = %q stdin %q, want %q and no stdin", calls, stdins, want)
	}
	// The local socket needs no credential.
	if creds, _ := lt.st.ListMachineCredentials(context.Background(), localID); len(creds) != 0 {
		t.Fatalf("local machine got credentials: %+v", creds)
	}
	w := lt.worker(t, localID)
	if w.Launch != store.LaunchLocal || w.Status != "online" {
		t.Fatalf("local worker = %+v", w)
	}
	if v := toWorkerView(w, 0, nil); v.Transport != TransportLocal {
		t.Fatalf("transport = %q", v.Transport)
	}
	// Adding it again while connected attaches; no second install.
	if code, body := lt.do(t, http.MethodPost, "/api/workers/local"); code != 200 || body["state"] != "connected" {
		t.Fatalf("re-add: %d %v", code, body)
	}
	if calls, _ := lt.runner.installCalls(); len(calls) != 1 {
		t.Fatalf("a connected daemon must never be started again: %q", calls)
	}
}

func TestAddLocalRecordsFailures(t *testing.T) {
	lt := newLaunchTest(t)
	lt.runner.installFn = func([]string) error { return errors.New("launchctl bootstrap failed: 5") }
	if code, _ := lt.do(t, http.MethodPost, "/api/workers/local"); code != http.StatusAccepted {
		t.Fatalf("add local: %d", code)
	}
	waitFor(t, 3*time.Second, "failed state", func() bool { return lt.worker(t, localID).LaunchState == store.LaunchStateFailed })
	if w := lt.worker(t, localID); !strings.Contains(w.LaunchError, "launchctl bootstrap failed") || w.Status != "offline" {
		t.Fatalf("worker = %+v", w)
	}

	// Installed but never connects: a bounded wait, then a clear error.
	lt.runner.mu.Lock()
	lt.runner.installFn = nil
	lt.runner.mu.Unlock()
	lt.srv.launch.connectTimeout = 300 * time.Millisecond
	if code, _ := lt.do(t, http.MethodPost, "/api/workers/"+localID+"/start"); code != http.StatusAccepted {
		t.Fatalf("retry: %d", code)
	}
	waitFor(t, 3*time.Second, "connect timeout", func() bool {
		return strings.Contains(lt.worker(t, localID).LaunchError, "has not connected")
	})
}

func TestAddLocalDryRunNeverRegistersAService(t *testing.T) {
	lt := newLaunchTest(t)
	lt.srv.launch.dryRun = true
	var child []string
	var childStdin string
	lt.srv.launch.startChild = func(_ context.Context, exe string, args []string, stdin string) error {
		child, childStdin = append([]string{exe}, args...), stdin
		lt.daemons.up(testWorkerInfo(localID, "hubhost"))
		return nil
	}
	if code, _ := lt.do(t, http.MethodPost, "/api/workers/local"); code != http.StatusAccepted {
		t.Fatalf("add local: %d", code)
	}
	waitFor(t, 5*time.Second, "child daemon to connect", func() bool { return lt.srv.hub.IsWorkerConnected(localID) })
	calls, _ := lt.runner.installCalls()
	if len(calls) != 1 || calls[0][len(calls[0])-1] != "--dry-run" {
		t.Fatalf("dry run must pass --dry-run to install: %q", calls)
	}
	if strings.Join(child, " ") != "/opt/syre/bin/syred run --name hubhost" || childStdin != "" {
		t.Fatalf("child = %q stdin %q", child, childStdin)
	}
}

func TestReconcilerClearsStaleLaunchStatesAndStartRetries(t *testing.T) {
	lt := newLaunchTest(t)
	ctx := context.Background()
	mk := func(id, name, launch, target string) {
		if _, err := lt.st.EnsureWorker(ctx, store.Worker{ID: id, Name: name, Launch: launch, SSHTarget: target}); err != nil {
			t.Fatal(err)
		}
	}
	const connectedSSH = "01890a5d-ac96-774b-bcce-b302099a805a"
	const manual = "01890a5d-ac96-774b-bcce-b302099a805b"
	mk(localID, "hubhost", store.LaunchLocal, "")
	mk(remoteID, "box", store.LaunchSSH, "me@box")
	mk(connectedSSH, "up", store.LaunchSSH, "me@up")
	mk(manual, "hand", store.LaunchManual, "")
	lt.connect(t, testWorkerInfo(connectedSSH, "up"))
	// Left over from an earlier hub run: a start that never finished, and a failure.
	_ = lt.st.SetWorkerLaunchState(ctx, localID, store.LaunchStateStarting, "", time.Now())
	_ = lt.st.SetWorkerLaunchState(ctx, remoteID, store.LaunchStateFailed, "unreachable", time.Now())

	lt.srv.reconcileLaunches(ctx)
	if w := lt.worker(t, localID); w.LaunchState != store.LaunchStateNone {
		t.Fatalf("stale starting state kept: %+v", w)
	}
	if w := lt.worker(t, remoteID); w.LaunchState != store.LaunchStateFailed {
		t.Fatalf("a real failure must stay visible: %+v", w)
	}
	if calls, _ := lt.runner.installCalls(); len(calls) != 0 || len(lt.bootstraps()) != 0 {
		t.Fatalf("the reconciler itself starts nothing (restarts are the attach loops'): %q %+v", calls, lt.bootstraps())
	}

	// The API retries on demand; it refuses machines the hub does not start.
	lt.mu.Lock()
	lt.bootRes = func(o sshutil.BootstrapOptions) sshutil.BootstrapResult {
		lt.daemons.up(testWorkerInfo(remoteID, "box"))
		return sshutil.BootstrapResult{OK: true, Target: o.Target, WorkerID: remoteID, Name: "box"}
	}
	lt.mu.Unlock()
	lt.srv.launch.connectTimeout = 3 * time.Second
	if code, _ := lt.do(t, http.MethodPost, "/api/workers/"+lt.worker(t, remoteID).SID+"/start"); code != http.StatusAccepted {
		t.Fatalf("retry by sid: %d", code)
	}
	waitFor(t, 5*time.Second, "retried ssh host online", func() bool {
		w := lt.worker(t, remoteID)
		return lt.srv.hub.IsWorkerConnected(remoteID) && w.LaunchState == store.LaunchStateNone
	})
	boots := lt.bootstraps()
	if len(boots) != 1 || boots[0].Target != "me@box" || boots[0].HubKey == nil || !strings.HasPrefix(boots[0].HubKey.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("bootstraps = %+v", boots)
	}
	if code, _ := lt.do(t, http.MethodPost, "/api/workers/"+manual+"/start"); code != http.StatusConflict {
		t.Fatalf("manual start: %d", code)
	}
	if code, body := lt.do(t, http.MethodPost, "/api/workers/"+connectedSSH+"/start"); code != 200 || body["state"] != "connected" {
		t.Fatalf("connected start: %d %v", code, body)
	}
	if len(lt.bootstraps()) != 1 {
		t.Fatalf("bootstraps = %+v", lt.bootstraps())
	}
	if w := lt.worker(t, manual); w.LaunchState != store.LaunchStateNone || w.LaunchAt != nil {
		t.Fatalf("manual worker touched: %+v", w)
	}
}

func TestSSHBootstrapRecordsLaunch(t *testing.T) {
	lt := newLaunchTest(t)
	lt.bootRes = func(o sshutil.BootstrapOptions) sshutil.BootstrapResult {
		return sshutil.BootstrapResult{OK: true, Target: "me@box", RemoteOS: "windows", RemoteArch: "amd64", WorkerID: remoteID, Name: "box", StartDeferred: true}
	}
	lt.srv.launch.connectTimeout = 200 * time.Millisecond
	resp := doReq(t, http.MethodPost, lt.ts.URL+"/api/ssh/bootstrap",
		strings.NewReader(`{"target":"me@box"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != 200 {
		t.Fatalf("bootstrap: %d", resp.StatusCode)
	}
	w := lt.worker(t, remoteID)
	if w.Launch != store.LaunchSSH || w.SSHTarget != "me@box" ||
		w.OS != "windows" || w.Status != "offline" || !strings.HasPrefix(w.SID, "m-") {
		t.Fatalf("recorded worker = %+v", w)
	}
	// A Windows logon task with nobody logged on: say it starts at the next login.
	waitFor(t, 3*time.Second, "deferred start message", func() bool {
		w := lt.worker(t, remoteID)
		return w.LaunchState == store.LaunchStateFailed && strings.Contains(w.LaunchError, "next Windows login")
	})
	if _, body := lt.do(t, http.MethodGet, "/api/setup"); body["needs_setup"] != false {
		t.Fatalf("adding a host completes setup: %v", body)
	}
	boots := lt.bootstraps()
	if len(boots) != 1 || boots[0].HubKey == nil || !strings.HasPrefix(boots[0].HubKey.PublicKey, "ssh-ed25519 ") {
		t.Fatalf("bootstraps = %+v", boots)
	}
	// The key lives next to the database, private; its comment names the hub.
	keyPath := lt.srv.hubKeyPath()
	if filepath.Base(keyPath) != HubKeyFileName || !strings.HasSuffix(boots[0].HubKey.PublicKey, "syre-hub:"+boots[0].HubKey.HubID) {
		t.Fatalf("hub key %s %q", keyPath, boots[0].HubKey.PublicKey)
	}
	if creds, _ := lt.st.ListMachineCredentials(context.Background(), remoteID); len(creds) != 0 {
		t.Fatalf("an SSH machine must get no credential: %+v", creds)
	}
}

func TestWorkerMatchesSSHHost(t *testing.T) {
	pc := sshutil.SSHHost{Alias: "pc", HostName: "pc"}
	wsl := sshutil.SSHHost{Alias: "pc-wsl", HostName: "pc"}
	installed := store.Worker{Name: "pc", HostGroup: "pc", SSHTarget: "pc"}
	if !workerMatchesSSHHost(installed, pc) || workerMatchesSSHHost(installed, wsl) {
		t.Fatal("an SSH-installed machine must match exactly its target, not every alias of its HostName")
	}
	manual := store.Worker{Name: "pc", HostGroup: "pc"}
	if !workerMatchesSSHHost(manual, pc) || !workerMatchesSSHHost(manual, wsl) {
		t.Fatal("a hand-started machine still matches by name / HostName")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	if !isLoopbackAddr("127.0.0.1:5000") || !isLoopbackAddr("[::1]:5000") || isLoopbackAddr("10.0.0.2:5000") {
		t.Fatal("isLoopbackAddr")
	}
}
