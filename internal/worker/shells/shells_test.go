package shells

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// fakeSource is a scripted process table.
type fakeSource struct {
	mu    sync.Mutex
	procs map[int]Proc
	argv  map[int][]string
	err   error
}

func newFake() *fakeSource {
	return &fakeSource{procs: map[int]Proc{}, argv: map[int][]string{}}
}

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func (f *fakeSource) add(pid, ppid, pgid int, name string, startSec int, argv ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = Proc{PID: pid, PPID: ppid, PGID: pgid, Name: name, Start: t0.Add(time.Duration(startSec) * time.Second)}
	f.argv[pid] = argv
}

func (f *fakeSource) rm(pids ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range pids {
		delete(f.procs, p)
	}
}

func (f *fakeSource) reparent(pid, ppid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.procs[pid]
	p.PPID = ppid
	f.procs[pid] = p
}

func (f *fakeSource) setCPU(pid int, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.procs[pid]
	p.CPU, p.CPUKnown = d, true
	f.procs[pid] = p
}

func (f *fakeSource) Snapshot() ([]Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Proc, 0, len(f.procs))
	for _, p := range f.procs {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeSource) Details(*Proc) {}

func (f *fakeSource) Argv(pid int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.argv[pid]
}

type fakeKiller struct {
	src        *fakeSource
	terminated []int
	killed     []int
	ignoreTerm map[int]bool
}

func (k *fakeKiller) Terminate(pid int) error {
	k.terminated = append(k.terminated, pid)
	if !k.ignoreTerm[pid] {
		k.src.rm(pid)
	}
	return nil
}

func (k *fakeKiller) Kill(pid int) error {
	k.killed = append(k.killed, pid)
	k.src.rm(pid)
	return nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time      { return c.now }
func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }

func newTracker(src *fakeSource) (*Tracker, *clock, *fakeKiller, *[]func()) {
	c := &clock{now: t0.Add(time.Hour)}
	k := &fakeKiller{src: src, ignoreTerm: map[int]bool{}}
	var later []func()
	tr := New(Options{Source: src, Killer: k, Now: c.Now, AfterFunc: func(_ time.Duration, f func()) { later = append(later, f) }})
	return tr, c, k, &later
}

func pids(list []protocol.ShellInfo) []int {
	var out []int
	for _, s := range list {
		out = append(out, s.PID)
	}
	return out
}

func TestTrackerCountsShellsUnderTheAgent(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(101, 100, 100, "node", 1)                                                                                                                             // MCP helper: not a shell
	src.add(110, 100, 100, "zsh", 10, "/bin/zsh", "-c", "-l", "source /h/.claude/shell-snapshots/s.sh && eval 'go test ./...' \\< /dev/null && pwd -P >| /tmp/c") // busy
	src.add(111, 110, 100, "go", 11)
	src.add(112, 110, 100, "bash", 12, "bash", "x.sh")          // nested shell: belongs to 110
	src.add(120, 101, 100, "sh", 20, "sh", "-c", "npm run dev") // under a helper: still counts
	tr, _, _, _ := newTracker(src)

	ch := tr.Tick(map[string]int{"chat:a": 100})
	got, ok := ch["chat:a"]
	if !ok {
		t.Fatalf("no report: %v", ch)
	}
	if got.Count != 2 || got.Busy != 1 {
		t.Fatalf("count/busy = %d/%d, want 2/1 (%+v)", got.Count, got.Busy, got.List)
	}
	if !slices.Equal(pids(got.List), []int{110, 120}) {
		t.Fatalf("pids = %v", pids(got.List))
	}
	if got.List[0].Command != "go test ./..." || got.List[1].Command != "npm run dev" {
		t.Fatalf("commands = %q, %q", got.List[0].Command, got.List[1].Command)
	}
	if !got.List[0].Busy || got.List[1].Busy {
		t.Fatalf("busy flags = %v %v", got.List[0].Busy, got.List[1].Busy)
	}

	// Nothing changed: no report.
	if ch := tr.Tick(map[string]int{"chat:a": 100}); len(ch) != 0 {
		t.Fatalf("unchanged tick reported %v", ch)
	}

	// The test run ends: the shell exits (the tool call returned); the dev server's
	// shell keeps running in the background and keeps counting.
	src.rm(111, 112, 110)
	ch = tr.Tick(map[string]int{"chat:a": 100})
	if got := ch["chat:a"]; got.Count != 1 || got.Busy != 0 || got.List[0].PID != 120 {
		t.Fatalf("after exit: %+v", got)
	}
}

func TestTrackerOrphanedBackgroundJob(t *testing.T) {
	// `sleep 300 &` in a Bash call: the shell exits, sleep is reparented to init but
	// stays in the agent's process group.
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(130, 1, 100, "sleep", 30, "/bin/sleep", "300")
	src.add(140, 1, 140, "other", 30) // someone else's process
	tr, _, _, _ := newTracker(src)
	got := tr.Tick(map[string]int{"task:t": 100})["task:t"]
	if got.Count != 1 || got.List[0].PID != 130 || got.List[0].Command != "sleep 300" || !got.List[0].Busy {
		t.Fatalf("orphan: %+v", got)
	}
}

func TestTrackerDanglingAfterSessionEnds(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(110, 100, 100, "bash", 10, "bash", "-c", "vite dev")
	src.add(111, 110, 100, "node", 11)
	tr, _, _, _ := newTracker(src)
	if got := tr.Tick(map[string]int{"chat:a": 100})["chat:a"]; got.Count != 1 || got.List[0].Dangling {
		t.Fatalf("running: %+v", got)
	}
	// The agent exits; its shell is reparented and keeps running.
	src.rm(100)
	src.reparent(110, 1)
	got, ok := tr.Tick(map[string]int{})["chat:a"]
	if !ok || got.Count != 1 || !got.List[0].Dangling || !got.List[0].Busy {
		t.Fatalf("dangling: %+v (reported %v)", got, ok)
	}
	if ch := tr.Tick(nil); len(ch) != 0 {
		t.Fatalf("unchanged dangling tick reported %v", ch)
	}
	// It exits: one last report of zero, then the session is forgotten.
	src.rm(110, 111)
	got, ok = tr.Tick(nil)["chat:a"]
	if !ok || got.Count != 0 || len(got.List) != 0 {
		t.Fatalf("gone: %+v (reported %v)", got, ok)
	}
	if len(tr.sessions) != 0 {
		t.Fatalf("session kept: %v", tr.sessions)
	}
	if ch := tr.Tick(nil); len(ch) != 0 {
		t.Fatalf("forgotten session reported %v", ch)
	}
}

func TestTrackerPidReuseIsNotTheSameShell(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(110, 100, 100, "bash", 10, "bash", "-c", "make")
	tr, _, _, _ := newTracker(src)
	tr.Tick(map[string]int{"chat:a": 100})
	src.rm(100, 110)
	src.add(110, 1, 110, "bash", 500, "bash") // a new process got the pid
	got := tr.Tick(nil)["chat:a"]
	if got.Count != 0 {
		t.Fatalf("reused pid counted: %+v", got)
	}
}

func TestTrackerBusyFromCPU(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(110, 100, 100, "bash", 10, "bash", "-c", "while :; do :; done")
	src.setCPU(110, time.Second)
	tr, c, _, _ := newTracker(src)
	if got := tr.Tick(map[string]int{"chat:a": 100})["chat:a"]; got.Busy != 0 {
		t.Fatalf("first tick busy without a baseline: %+v", got)
	}
	c.add(time.Second)
	src.setCPU(110, 1900*time.Millisecond) // 90% of the tick
	got := tr.Tick(map[string]int{"chat:a": 100})["chat:a"]
	if got.Busy != 1 || !got.List[0].Since.Equal(c.now.UTC()) {
		t.Fatalf("spinning shell not busy: %+v", got)
	}
	c.add(time.Second)
	got = tr.Tick(map[string]int{"chat:a": 100})["chat:a"]
	if got.Busy != 0 {
		t.Fatalf("idle shell busy: %+v", got)
	}
}

func TestTrackerStaleParentLink(t *testing.T) {
	// Windows: a child whose recorded parent pid now belongs to a newer process.
	src := newFake()
	src.add(100, 1, 0, "claude.exe", 50)
	src.add(110, 100, 0, "cmd.exe", 10, "cmd.exe", "/c", "dir") // older than its "parent"
	src.add(120, 100, 0, "pwsh.exe", 60, "pwsh.exe", "-Command", "Get-ChildItem")
	tr, _, _, _ := newTracker(src)
	got := tr.Tick(map[string]int{"chat:w": 100})["chat:w"]
	if got.Count != 1 || got.List[0].PID != 120 || got.List[0].Command != "Get-ChildItem" {
		t.Fatalf("stale link: %+v", got)
	}
}

func TestTrackerSnapshotErrorKeepsState(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(110, 100, 100, "bash", 10, "bash")
	tr, _, _, _ := newTracker(src)
	tr.Tick(map[string]int{"chat:a": 100})
	src.err = errors.New("boom")
	if ch := tr.Tick(map[string]int{}); ch != nil {
		t.Fatalf("error tick reported %v", ch)
	}
	if len(tr.sessions["chat:a"].shells) != 1 {
		t.Fatal("state lost on a failed snapshot")
	}
}

func TestTrackerCapsTheList(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	for i := range 30 {
		src.add(200+i, 100, 100, "sh", 10+i, "sh", "-c", "sleep 1")
	}
	tr, _, _, _ := newTracker(src)
	got := tr.Tick(map[string]int{"chat:a": 100})["chat:a"]
	if got.Count != 30 || len(got.List) != protocol.MaxShellList {
		t.Fatalf("count %d list %d", got.Count, len(got.List))
	}
}

func TestKillOnlyTheSessionsShells(t *testing.T) {
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(110, 100, 100, "bash", 10, "bash", "-c", "vite")
	src.add(111, 110, 100, "node", 11)
	src.add(112, 111, 100, "esbuild", 12)
	src.add(120, 100, 100, "bash", 20, "bash", "-c", "tsc -w")
	src.add(200, 1, 200, "bash", 5) // another session's shell
	tr, _, k, later := newTracker(src)
	tr.Tick(map[string]int{"chat:a": 100, "chat:b": 300})

	// Wrong start time (the pid was reused since the browser saw it): refused.
	if _, err := tr.Kill("chat:a", []protocol.ShellIdent{{PID: 110, StartedAt: t0.Add(99 * time.Second)}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale ident: %v", err)
	}
	// Not this session's shell: refused.
	if _, err := tr.Kill("chat:a", []protocol.ShellIdent{{PID: 200, StartedAt: t0.Add(5 * time.Second)}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign pid: %v", err)
	}
	if len(k.terminated) != 0 {
		t.Fatalf("terminated %v", k.terminated)
	}

	k.ignoreTerm[112] = true // esbuild ignores SIGTERM
	got, err := tr.Kill("chat:a", []protocol.ShellIdent{{PID: 110, StartedAt: t0.Add(10 * time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{110, 111, 112}) {
		t.Fatalf("signalled %v", got)
	}
	if len(*later) != 1 {
		t.Fatalf("no forced kill scheduled")
	}
	(*later)[0]()
	if !slices.Equal(k.killed, []int{112}) {
		t.Fatalf("forced %v, want only the survivor", k.killed)
	}

	// Clean up all: the rest of the session.
	got, err = tr.Kill("chat:a", nil)
	if err != nil || !slices.Equal(got, []int{120}) {
		t.Fatalf("all: %v %v", got, err)
	}
}

func TestIsShell(t *testing.T) {
	for _, n := range []string{"sh", "bash", "-zsh", "/bin/zsh", "fish", "dash", "pwsh", "powershell.exe", "cmd.exe", "CMD.EXE", "bash.exe"} {
		if !IsShell(n) {
			t.Errorf("%q is a shell", n)
		}
	}
	for _, n := range []string{"node", "claude", "sleep", "bashful", "python"} {
		if IsShell(n) {
			t.Errorf("%q is not a shell", n)
		}
	}
}

func TestCleanCommand(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"/bin/zsh", "-c", "-l", "source /Users/u/.claude/shell-snapshots/snapshot-zsh-1.sh && setopt NO_EXTENDED_GLOB 2>/dev/null || true && eval 'go test ./...' \\< /dev/null && pwd -P >| /var/folders/x/claude-ab-cwd"}, "go test ./..."},
		{[]string{"/bin/bash", "-c", "-l", "source /h/.claude/shell-snapshots/s.sh && shopt -u extglob 2>/dev/null || true && eval 'echo it'\\''s && sleep 1' < /dev/null && pwd -P >| /tmp/c"}, "echo it's && sleep 1"},
		// claude 2.1.33x, as seen in ps (zsh -c, no -l; the snapshot may be absent).
		{[]string{"/bin/zsh", "-c", `source /Users/u/.claude/shell-snapshots/snapshot-zsh-1795407075142-ma2t8r.sh 2>/dev/null || true && \builtin setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL NO_BRACE_CCL NO_MAGIC_EQUAL_SUBST NO_EQUALS 2>/dev/null || true && { \builtin unalias -- 'which'; \builtin unset -f -- 'which'; } >/dev/null 2>&1 || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'cd /repo && npm run dev -- --port 5173' < /dev/null && pwd -P >| /var/tmp/claude-ce03-cwd`}, "cd /repo && npm run dev -- --port 5173"},
		{[]string{"/bin/zsh", "-c", `\builtin setopt NO_EXTENDED_GLOB 2>/dev/null || true && { \builtin unalias -- 'which'; \builtin unset -f -- 'which'; } >/dev/null 2>&1 || true && eval 'sleep 25 && echo it'"'"'s done | wc -c' < /dev/null && pwd -P >| /var/tmp/claude-f54a-cwd`}, "sleep 25 && echo it's done | wc -c"},
		{[]string{"/bin/zsh", "-c", `SNAPSHOT_FILE=/Users/u/.claude/shell-snapshots/snapshot-zsh-1.sh source "/Users/u/.zshrc" < /dev/null # First, create/clear the snapshot file`}, "claude: shell snapshot"},
		{[]string{"bash", "-lc", "npm run dev"}, "npm run dev"},
		{[]string{"sh", "-c", "--", "make -j8", "sh"}, "make -j8"},
		{[]string{"cmd.exe", "/c", "dir", "/s"}, "dir /s"},
		{[]string{"pwsh.exe", "-NoProfile", "-Command", "Get-ChildItem", "-Recurse"}, "Get-ChildItem -Recurse"},
		{[]string{"/bin/zsh"}, "/bin/zsh"},
		{[]string{"/usr/local/bin/node", "server.js", "--port", "3000"}, "node server.js --port 3000"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := CleanCommand(c.argv); got != c.want {
			t.Errorf("CleanCommand(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'x'
	}
	if got := CleanCommand([]string{"sh", "-c", string(long)}); len([]rune(got)) != maxCommand {
		t.Errorf("cap: %d runes", len([]rune(got)))
	}
}

func TestTrackerRereadsArgvCaughtBeforeExec(t *testing.T) {
	// A snapshot between fork and exec shows the parent's image: a bare "sh" with no
	// arguments. The command must be read again once exec has replaced it.
	src := newFake()
	src.add(100, 1, 100, "claude", 0)
	src.add(150, 100, 100, "sh", 3600, "sh") // started just now (the clock is t0+1h)
	tr, c, _, _ := newTracker(src)
	if got := tr.Tick(map[string]int{"task:t": 100})["task:t"]; got.Count != 1 || got.List[0].Command != "sh" {
		t.Fatalf("first tick: %+v", got)
	}
	src.mu.Lock()
	src.argv[150] = []string{"sh", "-c", "sleep 300; true"}
	src.mu.Unlock()
	c.add(time.Second)
	if got := tr.Tick(map[string]int{"task:t": 100})["task:t"]; got.List[0].Command != "sleep 300; true" {
		t.Fatalf("after exec: %+v", got)
	}
	// Settled now: a later change to argv (a process rewriting its title) is ignored.
	src.mu.Lock()
	src.argv[150] = []string{"renamed"}
	src.mu.Unlock()
	c.add(5 * time.Second)
	if ch := tr.Tick(map[string]int{"task:t": 100}); len(ch) != 0 {
		t.Fatalf("settled command changed: %v", ch)
	}
}
