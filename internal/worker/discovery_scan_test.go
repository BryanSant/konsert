package worker

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/worker/observe"
	"github.com/BryanSant/syre/internal/worker/shells"
)

func TestPassiveAgentNames(t *testing.T) {
	argv := func(a ...string) func() []string { return func() []string { return a } }
	cases := []struct {
		name string
		argv func() []string
		want string
	}{
		{"claude", argv(), "claude"},
		{"/usr/local/bin/codex", argv(), "codex"},
		{"copilot", argv(), "copilot"},
		{"cursor-agent", argv(), "cursor"},
		{"grok", argv(), "grok"},
		// cursor-agent's launcher: exec -a "$0" node index.js
		{"node", argv("/home/u/.local/bin/cursor-agent", "--use-system-ca", "/home/u/.local/share/cursor-agent/versions/2026.11.22/index.js"), "cursor"},
		{"node", argv("node", "/usr/lib/node_modules/@github/copilot/index.js"), "copilot"},
		{"2.1.332", argv("/Users/u/.local/bin/claude", "--resume"), "claude"},
		{"2.1.332", argv("claude"), "claude"},
		{"1.2.3", argv("/opt/other/thing"), ""},
		{"node-MainThread", argv("/home/u/.local/bin/cursor-agent", "-e", "x"), "cursor"},
		// An editor's Copilot language server is not the copilot CLI.
		{"node", argv("node", "/home/u/.vscode/extensions/github.copilot/node_modules/@github/copilot-language-server/dist/language-server.js"), ""},
		{"node", argv("node", "server.js"), ""},
		{"zsh", argv(), ""},
	}
	if runtime.GOOS != "windows" {
		// macOS's desktop app is "Claude"; the CLI is "claude".
		cases = append(cases, struct {
			name string
			argv func() []string
			want string
		}{"Claude", argv(), ""})
	} else {
		cases = append(cases,
			struct {
				name string
				argv func() []string
				want string
			}{"claude.exe", argv(`C:\Users\u\.local\bin\claude.exe`), "claude"},
			struct {
				name string
				argv func() []string
				want string
			}{"claude.exe", argv(`C:\Users\u\AppData\Local\AnthropicClaude\app-1.0\claude.exe`), ""},
			struct {
				name string
				argv func() []string
				want string
			}{"node.exe", argv(`node.exe`, `C:\Users\u\AppData\Local\cursor-agent\versions\1\index.js`), "cursor"},
		)
	}
	for _, c := range cases {
		if got := passiveAgent(c.name, c.argv); got != c.want {
			t.Errorf("passiveAgent(%q, %v) = %q, want %q", c.name, c.argv(), got, c.want)
		}
	}
}

// fakeTable is a process table whose CPU times the test advances.
type fakeTable struct {
	procs []shells.Proc
	argv  map[int][]string
}

func (f *fakeTable) Snapshot() ([]shells.Proc, error) {
	return append([]shells.Proc(nil), f.procs...), nil
}
func (f *fakeTable) Details(*shells.Proc)  {}
func (f *fakeTable) Argv(pid int) []string { return f.argv[pid] }

func TestPassiveScanReadsTheProcessTable(t *testing.T) {
	var log emitLog
	disc := NewSessionDiscovery(t.TempDir(), nil, log.emit)
	stubCwd(disc, func(context.Context, int) string { return "/repo" })
	start := time.Now().Add(-time.Hour)
	tbl := &fakeTable{
		procs: []shells.Proc{
			{PID: 100, PPID: 1, Name: "node", Start: start, CPU: time.Second, CPUKnown: true},
			{PID: 200, PPID: 1, Name: "bash"},
		},
		argv: map[int][]string{100: {"/home/u/.local/bin/cursor-agent", "/home/u/.local/share/cursor-agent/versions/1/index.js"}},
	}
	disc.procs = tbl
	disc.ScanPassiveProcesses(context.Background())
	got := log.all()
	if len(got) != 1 || got[0].Agent != "cursor" || got[0].Status != "idle" || got[0].Cwd != "/repo" {
		t.Fatalf("first scan: %+v", got)
	}
	// Busy since the last scan (CPU time grew a lot): working.
	tbl.procs[0].CPU += 10 * time.Second
	time.Sleep(20 * time.Millisecond)
	disc.ScanPassiveProcesses(context.Background())
	if got := log.all(); len(got) != 2 || got[1].Status != "working" {
		t.Fatalf("busy scan: %+v", got)
	}
	// Exited: done.
	tbl.procs = tbl.procs[1:]
	disc.ScanPassiveProcesses(context.Background())
	if got := log.all(); len(got) != 3 || got[2].Status != "done" {
		t.Fatalf("exit: %+v", got)
	}
}

func TestPassiveScanLeavesSourcedAgentsToTheirRecords(t *testing.T) {
	var log emitLog
	disc := NewSessionDiscovery(t.TempDir(), nil, log.emit)
	stubCwd(disc, func(context.Context, int) string { return "/repo" })
	disc.observed = newObservedSessions(observedConfig{
		ClaudeDirs: func() []observe.ClaudeDir { return []observe.ClaudeDir{{Dir: t.TempDir()}} },
		Procs:      osProcTable(),
	}, disc.emitIfChanged)
	// A busy claude is not guessed "working" from CPU: claude's own records say.
	disc.scanProcessList(context.Background(), []psEntry{
		{pid: 300, ppid: 1, comm: "claude", pcpu: 90},
		{pid: 400, ppid: 1, comm: "codex", pcpu: 90},
	})
	got := log.all()
	if len(got) != 1 || got[0].Agent != "codex" || got[0].Status != "working" {
		t.Fatalf("got %+v", got)
	}
}
