package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestRetryDecisionPerCategory(t *testing.T) {
	for _, c := range []struct {
		cat   string
		used  int
		limit int
		want  retryAction
	}{
		{protocol.ErrKindOOM, 0, 2, retryLarger},
		{protocol.ErrKindDiskFull, 1, 2, retryLarger},
		{protocol.ErrKindTimeout, 0, 2, retryLarger},
		{protocol.ErrKindOOM, 2, 2, retryFail},
		{protocol.ErrKindUsageLimit, 0, 2, retryAgain},
		{protocol.ErrKindRateLimit, 1, 2, retryAgain},
		{protocol.ErrKindUsageLimit, 0, 0, retryFail}, // retries off
		{protocol.ErrKindAuth, 0, 2, retryAsk},
		{protocol.ErrKindModel, 5, 2, retryAsk},
		{protocol.ErrKindNotFound, 0, 2, retryAsk},
		{protocol.ErrKindMissingTool, 0, 2, retryAsk},
		{protocol.ErrKindAgentError, 0, 2, retryFail},
		{protocol.ErrKindVerify, 0, 2, retryFail},
	} {
		if got := retryDecision(c.cat, c.used, c.limit); got != c.want {
			t.Errorf("%s used %d/%d: got %v, want %v", c.cat, c.used, c.limit, got, c.want)
		}
	}
}

// A guardrails shed is not a failure of the work: it never spends a retry, and it is
// told apart from oom (SYR-SNAYZ keeps requeueing sheds on its own path).
func TestShedNeverSpendsTheRetryBudget(t *testing.T) {
	attempts := []store.Attempt{
		{Attempt: 1, Status: store.AttemptFailed, FailureCategory: protocol.ErrKindMemoryPressureShed},
		{Attempt: 2, Status: store.AttemptFailed, FailureCategory: protocol.FailureHostMemoryFull},
		{Attempt: 3, Status: store.AttemptFailed, FailureCategory: protocol.ErrKindOOM, WorkerID: "w-small"},
		{Attempt: 4, Status: store.AttemptFailed, FailureCategory: protocol.ErrKindMemoryPressureShed},
	}
	if n := retriesUsed(attempts); n != 1 {
		t.Fatalf("retries used = %d, want 1 (only the oom)", n)
	}
	if a, ok := lastSizedFailure(attempts); !ok || a.Attempt != 3 {
		t.Fatalf("last sized failure = %+v %v, want the oom", a, ok)
	}
	if retryDecision(protocol.ErrKindMemoryPressureShed, 0, 2) != retryFail {
		t.Fatal("a shed must not be retried as a failure: handleTicketShed requeues it")
	}
}

func TestOOMRetriesOnALargerMachineThenAsks(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	info := func(id string, ramMB uint64) protocol.WorkerInfo {
		wi := testWorkerInfo(id, id)
		wi.Facts = &protocol.MachineFacts{CPUCores: 8, RAMTotalMB: ramMB, DiskFreeGB: 100}
		return wi
	}
	small := cs.connectWorker(t, info("w-small", 8192), nil, "w-small")
	// The big machine is known but offline when the first run fails.
	bigFacts, _ := json.Marshal(protocol.MachineFacts{CPUCores: 32, RAMTotalMB: 65536, DiskFreeGB: 500}, protocol.JSONOptions)
	if err := cs.st.UpsertWorker(ctx, store.Worker{ID: "w-big", Name: "w-big", HostGroup: "w-big", OS: "linux", Arch: "amd64", Agents: []string{"mock"}, MaxConcurrent: 4, Status: "offline", FactsJSON: string(bigFacts)}); err != nil {
		t.Fatal(err)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"retry_limit": 11}); code != http.StatusBadRequest {
		t.Fatalf("retry_limit 11 accepted: %d", code)
	}

	if err := cs.st.CreateProject(ctx, store.Project{Name: "Mem"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "api", RemoteURL: "https://github.com/acme/api.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Big build", Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, TargetBranch: "feat/x"})

	failRun := func(fw *fakeWorker) {
		t.Helper()
		open := decodeOpen(t, fw)
		prompt, _ := protocol.DecodePayload[protocol.AgentChatPrompt](fw.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
		code := 137
		fw.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{ChatID: open.ChatID, Seq: 1, TurnID: prompt.TurnID, Kind: protocol.EventError,
			Error: &protocol.AgentChatError{Kind: protocol.ErrKindOOM, Message: "out of memory", ExitCode: &code}})
	}
	status := func() string { cur, _ := cs.st.GetTicket(ctx, tk.ID); return cur.Status }

	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	failRun(small)
	// Back to ready, but only a machine with more than 8 GB may take it.
	waitFor(t, 3*time.Second, "ticket requeued after oom", func() bool { return status() == store.TicketStatusReady })
	if got := small.drain(protocol.MsgAgentChatOpen, 300*time.Millisecond); len(got) != 0 {
		t.Fatal("the retry went to the machine that ran out of memory")
	}
	big := cs.connectWorker(t, info("w-big", 65536), nil, "w-big")
	failRun(big)
	// Nothing is larger than the big machine: a person decides.
	waitFor(t, 3*time.Second, "ticket needs you", func() bool { return status() == store.TicketStatusNeedsYou })
	attempts, _ := cs.st.ListAttempts(ctx, tk.ID, "")
	oom := 0
	for _, a := range attempts {
		if a.FailureCategory == protocol.ErrKindOOM {
			oom++
		}
	}
	if oom != 2 {
		t.Fatalf("attempts = %+v, want two oom", attempts)
	}
}
