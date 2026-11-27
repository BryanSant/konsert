package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/worker/guard"
)

// TestLiveTicketShedEscalates is the three-shed escalation end to end with real memory
// (opt-in: it eats the host's memory). The ticket's verify command is a memory hog, run
// as a guarded session under the host's native guard. Each turn's verify is shed for
// real, the hub requeues the ticket after a cooldown, and the third shed hands it to
// the user.
//
//	SYRE_GUARD_LIVE=1 go test -run TestLiveTicketShedEscalates -v -timeout 15m ./internal/controller
func TestLiveTicketShedEscalates(t *testing.T) {
	if os.Getenv("SYRE_GUARD_HOG") == "1" {
		liveHog()
		return
	}
	if os.Getenv("SYRE_GUARD_LIVE") != "1" || runtime.GOOS == "windows" {
		t.Skip("set SYRE_GUARD_LIVE=1 (Linux or macOS) to run the live shed escalation check")
	}
	virt := os.Getenv("SYRE_GUARD_VIRT")
	if virt == "" {
		virt = "bare_metal"
	}
	var total uint64
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		var kb uint64
		_, _ = fmt.Sscanf(string(raw), "MemTotal: %d kB", &kb)
		total = kb << 10
	}
	if total == 0 {
		t.Skip("no /proc/meminfo")
	}
	prev := guard.Default()
	g := guard.New(guard.Config{Baseline: guard.NewBaseline(total, false, virt)})
	guard.SetDefault(g)
	t.Cleanup(func() { guard.SetDefault(prev) })
	st := g.Status()
	t.Logf("guard %s via %s, reserve %d MiB of %d MiB", st.Mode, st.Mechanism, st.Reserve>>20, st.TotalRAM>>20)

	cs := newChatServer(t)
	ctx := context.Background()
	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")
	_, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()
	cs.h.shedCooldown.period = time.Second // 1, 2, 4 s

	if err := cs.st.CreateProject(ctx, store.Project{Name: "LiveShed", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "app", RemoteURL: repoOrigin, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	exe, _ := os.Executable()
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Build the thing",
		Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"},
		VerifyCmd: fmt.Sprintf("SYRE_GUARD_HOG=1 %s -test.run '^TestLiveTicketShedEscalates$'", exe),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, BaseBranch: "main", TargetBranch: "feat/live-shed"}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		cs.h.SchedulePending(ctx)
		cur, _ := cs.st.GetTicket(ctx, tk.ID)
		if cur.Status == store.TicketStatusNeedsYou {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	cur, _ := cs.st.GetTicket(ctx, tk.ID)
	rows, _ := cs.st.ListAttempts(ctx, tk.ID, "")
	for _, r := range rows {
		t.Logf("attempt %d: %s", r.Attempt, attemptStatusLabel(r))
	}
	if cur.Status != store.TicketStatusNeedsYou {
		t.Fatalf("ticket %s after %s, want needs_you", cur.Status, time.Since(start).Round(time.Second))
	}
	if n := cs.h.ticketShedCount(ctx, tk.ID); n != shedEscalateAfter {
		t.Fatalf("%d counted sheds, want %d", n, shedEscalateAfter)
	}
	for _, r := range rows {
		if r.FailureCategory != protocol.ErrKindMemoryPressureShed {
			t.Fatalf("attempt %d is %s, want requeued/memory_pressure_shed", r.Attempt, attemptStatusLabel(r))
		}
	}
	t.Logf("three real sheds, then needs_you, in %s", time.Since(start).Round(time.Second))
}

// liveHog fills 128 MiB with incompressible data every 100 ms until it is killed.
func liveHog() {
	var keep [][]uint64
	x := uint64(time.Now().UnixNano()) | 1
	for {
		b := make([]uint64, (128<<20)/8)
		for i := range b {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			b[i] = x
		}
		keep = append(keep, b)
		runtime.KeepAlive(keep)
		time.Sleep(100 * time.Millisecond)
	}
}
