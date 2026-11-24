package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/hubkey"
	"github.com/BryanSant/konsert/internal/linkwatch"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/sshutil"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/coder/websocket"
)

// fakeDaemons are in-process konsertds behind their local socket: dialing one returns
// a pipe, and the daemon end serves the attach WebSocket and says worker.hello, as a
// real konsertd behind `konsertd attach` would.
type fakeDaemons struct {
	t     *testing.T
	mu    sync.Mutex
	infos map[string]protocol.WorkerInfo // dialed id -> the identity its daemon claims
	dials map[string]int
	links []*fakeLink
}

type fakeLink struct {
	dialed string
	hubID  string
	ws     *websocket.Conn
	closed chan websocket.StatusCode
}

func newFakeDaemons(t *testing.T) *fakeDaemons {
	return &fakeDaemons{t: t, infos: map[string]protocol.WorkerInfo{}, dials: map[string]int{}}
}

// up starts accepting attaches for info.ID.
func (f *fakeDaemons) up(info protocol.WorkerInfo) { f.upAs(info.ID, info) }

// upAs makes the socket dialed for id answer with info (another identity, to test the
// hub's binding).
func (f *fakeDaemons) upAs(id string, info protocol.WorkerInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos[id] = info
}

func (f *fakeDaemons) dialCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials[id]
}

func (f *fakeDaemons) lastLink() *fakeLink {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.links) == 0 {
		return nil
	}
	return f.links[len(f.links)-1]
}

func (f *fakeDaemons) dial(id string) (net.Conn, error) {
	f.mu.Lock()
	info, ok := f.infos[id]
	f.dials[id]++
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("dial unix: connect: no such file or directory")
	}
	hubSide, daemonSide := net.Pipe()
	go f.serve(id, info, daemonSide)
	return hubSide, nil
}

func (f *fakeDaemons) serve(id string, info protocol.WorkerInfo, c net.Conn) {
	ln := &oneConnListener{c: c, closed: make(chan struct{})}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer ln.Close()
		if r.URL.Path != protocol.AttachPath {
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		link := &fakeLink{dialed: id, hubID: r.Header.Get(protocol.HubIDHeader), ws: ws, closed: make(chan websocket.StatusCode, 1)}
		f.mu.Lock()
		f.links = append(f.links, link)
		f.mu.Unlock()
		raw, _ := protocol.NewEnvelope(protocol.MsgWorkerHello, "", protocol.WorkerHelloPayload{Worker: info})
		if err := ws.Write(context.Background(), websocket.MessageText, raw); err != nil {
			link.closed <- websocket.CloseStatus(err)
			return
		}
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				link.closed <- websocket.CloseStatus(err)
				return
			}
		}
	})}
	_ = srv.Serve(ln)
}

// oneConnListener hands out one connection, then blocks until closed.
type oneConnListener struct {
	c      net.Conn
	once   sync.Once
	closed chan struct{}
	cOnce  sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.cOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return &net.UnixAddr{Name: "fake", Net: "unix"} }

func (lt *launchTest) addSSHWorker(t *testing.T, id, name, target string) store.Worker {
	t.Helper()
	w, err := lt.st.EnsureWorker(context.Background(), store.Worker{ID: id, Name: name, Launch: store.LaunchSSH, SSHTarget: target})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// An SSH machine is attached by the hub (over SSH, faked here) and registers without
// any machine credential; the hub identifies itself so konsertd knows its key line.
func TestAttachSSHMachineWithoutCredential(t *testing.T) {
	lt := newLaunchTest(t)
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.daemons.up(testWorkerInfo(remoteID, "box"))
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "ssh machine online", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })

	hubID, _ := lt.srv.hubID(context.Background())
	if l := lt.daemons.lastLink(); l == nil || l.hubID != hubID || !hubkey.ValidHubID(hubID) {
		t.Fatalf("attach must carry the hub id %q: %+v", hubID, l)
	}
	if creds, _ := lt.st.ListMachineCredentials(context.Background(), remoteID); len(creds) != 0 {
		t.Fatalf("credentials = %+v", creds)
	}
	if v := toWorkerView(lt.worker(t, remoteID), 0, nil); v.Transport != TransportSSH || v.Status != "online" {
		t.Fatalf("view = %+v", v)
	}
	// ensure is idempotent: one loop, one link.
	lt.srv.attach.ensure(w)
	time.Sleep(100 * time.Millisecond)
	if n := lt.daemons.dialCount(remoteID); n != 1 {
		t.Fatalf("dials = %d", n)
	}
}

// The link is bound to the machine record: a daemon claiming another identity is
// refused (policy violation) and registers as nothing; the hub keeps retrying.
func TestAttachRejectsIdentityMismatch(t *testing.T) {
	lt := newLaunchTest(t)
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.daemons.upAs(remoteID, testWorkerInfo(otherID, "impostor"))
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "a refused link", func() bool { return lt.daemons.lastLink() != nil })
	select {
	case code := <-lt.daemons.lastLink().closed:
		if code != websocket.StatusPolicyViolation {
			t.Fatalf("close code = %d, want policy violation", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mismatched hello was not refused")
	}
	waitFor(t, 5*time.Second, "a retry", func() bool { return lt.daemons.dialCount(remoteID) >= 2 })
	if lt.srv.hub.IsWorkerConnected(remoteID) || lt.srv.hub.IsWorkerConnected(otherID) {
		t.Fatal("a mismatched daemon must not register")
	}
	if _, err := lt.st.GetWorker(context.Background(), otherID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("impostor recorded: %v", err)
	}
}

// Removing an SSH machine closes its link with 4001 (konsertd drops the key line and
// stops), removes the grant over the user's SSH as well, and stops attaching.
func TestRemoveSSHMachineDropsKeyAndStopsAttaching(t *testing.T) {
	lt := newLaunchTest(t)
	type call struct{ target, hub, worker string }
	calls := make(chan call, 4)
	lt.srv.launch.removeHubKey = func(_ context.Context, target, hubID, workerID string) error {
		calls <- call{target, hubID, workerID}
		return nil
	}
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.daemons.up(testWorkerInfo(remoteID, "box"))
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "online", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })
	link := lt.daemons.lastLink()

	if code, _ := lt.do(t, http.MethodDelete, "/api/workers/"+remoteID); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	select {
	case code := <-link.closed:
		if code != websocket.StatusCode(protocol.WSCloseWorkerRemoved) {
			t.Fatalf("close code = %d, want 4001", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("link not closed with 4001")
	}
	hubID, _ := lt.srv.hubID(context.Background())
	select {
	case c := <-calls:
		if c != (call{"me@box", hubID, remoteID}) {
			t.Fatalf("removeHubKey(%+v)", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hub key not removed")
	}
	n := lt.daemons.dialCount(remoteID)
	time.Sleep(300 * time.Millisecond)
	if lt.daemons.dialCount(remoteID) != n || lt.srv.attach.running(remoteID) {
		t.Fatal("the hub kept attaching to a removed machine")
	}
}

// fakeSSHDialer starts the test binary as "ssh": it bridges its stdio to the daemon's
// socket exactly like `konsertd attach` does behind sshd. It records each link so a
// test can kill it (a dropped SSH connection).
type fakeSSHDialer struct {
	sock  string
	mu    sync.Mutex
	conns []*sshutil.CommandConn
}

func (d *fakeSSHDialer) dial(ctx context.Context, _, _, _ string) (net.Conn, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), fakeSSHSocketEnv+"="+d.sock)
	c, err := sshutil.StartCommandConn(cmd)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

func (d *fakeSSHDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

func (d *fakeSSHDialer) kill() {
	d.mu.Lock()
	c := d.conns[len(d.conns)-1]
	d.mu.Unlock()
	_ = c.Close()
}

// End to end over the SSH transport with a real konsertd: the hub attaches through an
// "ssh" process; killing that process drops the link but not the agent session, the hub
// re-dials and the task is still there; a restarted hub (same database) attaches again
// and resyncs the running task from worker.hello; removal drops the authorized_keys
// grant and stops the daemon.
func TestSSHTransportSurvivesDroppedLinkAndHubRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const wid = "01890a5d-ac96-774b-bcce-b302099a8060"
	home := t.TempDir()
	t.Setenv("HOME", home)

	sockDir, err := os.MkdirTemp("", "ks")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "run", "d.sock")

	wCfg := config.DefaultWorkerConfig()
	wCfg.WorkerID = wid
	wCfg.Name = "box"
	wCfg.EnableMockDriver = true
	daemon := worker.NewDaemon(wCfg, drivers.DefaultRegistry, nil)
	daemon.SocketPath = sock
	daemonErr := make(chan error, 1)
	go func() { daemonErr <- daemon.Run(ctx) }()

	dbPath := filepath.Join(t.TempDir(), "konsert.db")
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	cfg.DBPath = dbPath
	start := func() (*Server, *sqlite.SQLiteStore, *fakeSSHDialer) {
		st, err := sqlite.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		srv := NewServer(cfg, st, nil, "test")
		d := &fakeSSHDialer{sock: sock}
		srv.attach.dialSSH = d.dial
		srv.attach.minBackoff, srv.attach.maxBackoff = 20*time.Millisecond, 200*time.Millisecond
		srv.launch.bootstrap = func(context.Context, sshutil.BootstrapOptions) sshutil.BootstrapResult {
			return sshutil.BootstrapResult{Error: "no bootstrap in this test"}
		}
		return srv, st, d
	}

	srv1, st1, ssh1 := start()
	if _, err := st1.EnsureWorker(ctx, store.Worker{ID: wid, Name: "box", Launch: store.LaunchSSH, SSHTarget: "me@box"}); err != nil {
		t.Fatal(err)
	}
	// The grant bootstrap would have installed, so removal can be checked at the end.
	grant, err := srv1.hubKeyGrant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hubkey.Install(hubkey.Entry{HubID: grant.HubID, PublicKey: grant.PublicKey, Exe: "/k", GOOS: runtime.GOOS, WorkerID: wid}); err != nil {
		t.Fatal(err)
	}
	srv1.startAttachers(ctx)
	waitForWorkerOnline(t, srv1.Service(), wid)

	disp, err := srv1.Service().DispatchTask(ctx, DispatchRequest{
		Agent: "mock", RepoPath: t.TempDir(), Prompt: "WAIT_INPUT", Mode: protocol.TaskModeInteractive,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	live := func(s string) bool { return s == protocol.TaskStatusRunning || s == protocol.TaskStatusWaitingInput }
	waitFor(t, 10*time.Second, "task running", func() bool {
		return live(mustTask(t, st1, disp.TaskID).Status)
	})

	// 1. Drop the link: kill the hub-side ssh.
	ssh1.kill()
	waitFor(t, 10*time.Second, "re-dial after the dropped link", func() bool { return ssh1.count() >= 2 })
	waitForWorkerOnline(t, srv1.Service(), wid)
	if st := mustTask(t, st1, disp.TaskID).Status; !live(st) {
		t.Fatalf("task after dropped link: %s", st)
	}
	if !slices.Contains(daemon.Runner().ActiveTaskIDs(), disp.TaskID) {
		t.Fatal("the agent session died with the link")
	}

	// 2. Restart the hub on the same database.
	srv1.Hub().Close()
	_ = st1.Close()
	srv2, st2, _ := start()
	defer st2.Close()
	defer srv2.Hub().Close()
	srv2.startAttachers(ctx)
	waitForWorkerOnline(t, srv2.Service(), wid)
	if _, err := srv2.Service().SteerTask(ctx, disp.TaskID, "exit", true); err != nil {
		t.Fatalf("steer after restart: %v", err)
	}
	res, err := srv2.Service().GetTaskResult(ctx, disp.TaskID, true, 10)
	if err != nil || res.Status != protocol.TaskStatusCompleted {
		t.Fatalf("re-adopted task: %+v %v", res, err)
	}

	// 3. Remove the machine: konsertd drops the grant and stops.
	srv2.launch.removeHubKey = func(context.Context, string, string, string) error { return nil }
	ts := httptest.NewServer(srv2)
	defer ts.Close()
	if resp := doReq(t, http.MethodDelete, ts.URL+"/api/workers/"+wid, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	select {
	case err := <-daemonErr:
		if err != nil {
			t.Fatalf("daemon after removal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop after removal")
	}
	b, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if strings.Contains(string(b), hubkey.Marker(grant.HubID)) {
		t.Fatalf("hub key line left after removal: %q", b)
	}
}

// A wake from sleep or a network change on the hub's machine drops every attach link and
// redials at once, without waiting out the backoff.
func TestAttachRedialsOnWakeOrNetworkChange(t *testing.T) {
	lt := newLaunchTest(t)
	lt.srv.attach.minBackoff, lt.srv.attach.maxBackoff = time.Minute, time.Minute
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.daemons.up(testWorkerInfo(remoteID, "box"))
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "ssh machine online", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })
	first := lt.daemons.lastLink()
	lt.srv.attach.redialAll(linkwatch.Change{Reason: linkwatch.ReasonNetwork, Since: time.Now()})
	select {
	case <-first.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the old link was not dropped")
	}
	waitFor(t, 5*time.Second, "second link", func() bool { return lt.daemons.dialCount(remoteID) == 2 })
	waitFor(t, 5*time.Second, "ssh machine online again", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })
	if l := lt.daemons.lastLink(); l == first {
		t.Fatal("no new link")
	}
}

// A wake or network change reported after the attach loop already re-dialed (its link
// noticed the drop before the watcher's tick) does not drop that fresh link: the hub
// reconnects once, not twice. One whose link predates the change still redials.
func TestAttachIgnoresChangeOlderThanItsLink(t *testing.T) {
	lt := newLaunchTest(t)
	lt.srv.attach.minBackoff, lt.srv.attach.maxBackoff = time.Minute, time.Minute
	w := lt.addSSHWorker(t, remoteID, "box", "me@box")
	lt.daemons.up(testWorkerInfo(remoteID, "box"))
	before := time.Now().Add(-time.Second) // the change happened before the link was dialed
	lt.srv.attach.ensure(w)
	waitFor(t, 5*time.Second, "ssh machine online", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })
	first := lt.daemons.lastLink()

	lt.srv.attach.mu.Lock()
	loop := lt.srv.attach.loops[remoteID]
	lt.srv.attach.mu.Unlock()
	for _, reason := range []string{linkwatch.ReasonWake, linkwatch.ReasonNetwork} {
		lt.srv.attach.redialAll(linkwatch.Change{Reason: reason, Since: before})
		if n := len(loop.redial); n != 0 {
			t.Fatalf("%s older than the link queued a redial", reason)
		}
	}
	select {
	case <-first.closed:
		t.Fatal("the fresh link was dropped")
	default:
	}
	if n := lt.daemons.dialCount(remoteID); n != 1 {
		t.Fatalf("dials = %d, want 1", n)
	}

	// A change after the attempt started: this link predates it and is redialed once.
	lt.srv.attach.redialAll(linkwatch.Change{Reason: linkwatch.ReasonWake, Since: time.Now()})
	select {
	case <-first.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stale link was not dropped")
	}
	waitFor(t, 5*time.Second, "second link", func() bool { return lt.daemons.dialCount(remoteID) == 2 })
	waitFor(t, 5*time.Second, "online again", func() bool { return lt.srv.hub.IsWorkerConnected(remoteID) })
}
