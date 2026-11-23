package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

type emitLog struct {
	mu  sync.Mutex
	got []protocol.ExternalSessionPayload
}

func (e *emitLog) emit(p protocol.ExternalSessionPayload) {
	e.mu.Lock()
	e.got = append(e.got, p)
	e.mu.Unlock()
}

func (e *emitLog) all() []protocol.ExternalSessionPayload {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.ExternalSessionPayload(nil), e.got...)
}

func stubCwd(disc *SessionDiscovery, fn func(ctx context.Context, pid int) string) {
	disc.resolveCwd = fn
}

func TestHookPollKeepsPartialLines(t *testing.T) {
	home := t.TempDir()
	var log emitLog
	disc := NewSessionDiscovery(home, nil, log.emit)
	path := HookEventsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"session_id":"s1","agent":"codex","event":"working","cwd":"/nonexistent-konsert-test","ts":"2026-01-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(line[:20]), 0o600); err != nil {
		t.Fatal(err)
	}
	disc.PollHookEvents(context.Background())
	if len(log.all()) != 0 {
		t.Fatalf("partial hook line must not be emitted: %+v", log.all())
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(line[20:])
	_ = f.Close()
	disc.PollHookEvents(context.Background())
	if got := log.all(); len(got) != 1 || got[0].ExternalID != "s1" || got[0].Agent != "codex" {
		t.Fatalf("completed hook line not emitted on the next poll: %+v", got)
	}
}

// The same SessionDiscovery is reused across connections: events recorded while no Run is
// active (disconnected) are delivered by the next Run, and known sessions are re-announced.
func TestDiscoveryDeliversHookEventsWrittenWhileDisconnected(t *testing.T) {
	home := t.TempDir()
	var log emitLog
	disc := NewSessionDiscovery(home, func(int) bool { return false }, log.emit)
	stubCwd(disc, func(context.Context, int) string { return "" })

	if err := RecordHookEvent(home, HookEvent{SessionID: "during-disconnect", Agent: "grok", Event: "working", Cwd: "/x"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { disc.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(log.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	got := log.all()
	if len(got) == 0 || got[0].ExternalID != "during-disconnect" {
		t.Fatalf("hook event written while disconnected was not delivered: %+v", got)
	}
	// Next connection: Resync re-announces the live session.
	before := len(got)
	disc.Resync()
	if after := log.all(); len(after) != before+1 || after[before].ExternalID != "during-disconnect" {
		t.Fatalf("Resync did not re-announce live sessions: %+v", after[before:])
	}
}

func TestRecordHookEventSkipsManagedTasks(t *testing.T) {
	home := t.TempDir()
	t.Setenv(ManagedTaskEnv, "TASK-NCFFA")
	if err := RecordHookEvent(home, HookEvent{Agent: "claude", Event: "PreToolUse"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(HookEventsPath(home)); !os.IsNotExist(err) {
		t.Fatal("hook event from a managed task must not be recorded")
	}
	if _, ok := ParseHookEventLine(`{"agent":"claude","event":"Stop","task_id":"x"}`); ok {
		t.Fatal("hook line carrying task_id must not become an external session")
	}
}

func TestPassiveScanSkipsManagedProcessTree(t *testing.T) {
	var log emitLog
	const managed = 4242
	disc := NewSessionDiscovery(t.TempDir(), func(pid int) bool { return pid == managed }, log.emit)
	stubCwd(disc, func(context.Context, int) string { return "/work" })
	disc.scanProcessList(context.Background(), []psEntry{
		{pid: managed, ppid: 1, comm: "/usr/local/bin/claude"},
		{pid: 5000, ppid: managed, comm: "claude"}, // helper spawned by the managed agent
		{pid: 6000, ppid: 1, comm: "claude"},       // user's own session
	})
	got := log.all()
	if len(got) != 1 || got[0].ExternalID != "pid-6000" {
		t.Fatalf("expected only the unmanaged process as external, got %+v", got)
	}
}

func TestPassiveScanUnresolvedCwdKeepsSession(t *testing.T) {
	var log emitLog
	disc := NewSessionDiscovery(t.TempDir(), nil, log.emit)
	procs := []psEntry{{pid: 7000, ppid: 1, comm: "codex"}}

	stubCwd(disc, func(context.Context, int) string { return "/repo" })
	disc.scanProcessList(context.Background(), procs)

	// lsof now times out: the live process must not flicker to done, and the cached cwd
	// is used for it.
	disc.cwdTimeout = 20 * time.Millisecond
	disc.resolveCwd = func(ctx context.Context, _ int) string { <-ctx.Done(); return "" }
	disc.scanProcessList(context.Background(), procs)

	// With the cache dropped, an unresolved but known PID still counts as present.
	disc.mu.Lock()
	delete(disc.cwdCache, 7000)
	disc.mu.Unlock()
	disc.scanProcessList(context.Background(), procs)
	for _, p := range log.all() {
		if p.Status == "done" {
			t.Fatalf("live session marked done after an unresolved cwd: %+v", log.all())
		}
	}

	// Once the process is gone it is marked done.
	disc.scanProcessList(context.Background(), nil)
	got := log.all()
	if last := got[len(got)-1]; last.ExternalID != "pid-7000" || last.Status != "done" {
		t.Fatalf("exited process not marked done: %+v", got)
	}
}

func TestPassiveScanPerProcessTimeout(t *testing.T) {
	var log emitLog
	disc := NewSessionDiscovery(t.TempDir(), nil, log.emit)
	disc.cwdTimeout = 50 * time.Millisecond
	stubCwd(disc, func(ctx context.Context, pid int) string {
		if pid == 1111 {
			<-ctx.Done() // hung lsof for this process only
			return ""
		}
		return "/fast"
	})
	disc.scanProcessList(context.Background(), []psEntry{
		{pid: 1111, ppid: 1, comm: "claude"},
		{pid: 2222, ppid: 1, comm: "grok"},
	})
	got := log.all()
	if len(got) != 1 || got[0].ExternalID != "pid-2222" {
		t.Fatalf("a slow lookup for one process must not starve the others: %+v", got)
	}
}
