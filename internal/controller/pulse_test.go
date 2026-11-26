package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPulseDebounceBatchingAndNoRoutinePulse(t *testing.T) {
	cs := newChatServer(t)
	cs.h.PulseDebounce = 120 * time.Millisecond
	cs.h.pulseNow = func() time.Time { return time.Date(2026, 11, 25, 14, 32, 0, 0, time.Local) }
	ctx := context.Background()

	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	var seq atomic.Int64
	finishTurn := func(chatID, turn string) {
		w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
			ChatID: chatID, Seq: seq.Add(1), Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn,
		})
		waitFor(t, 3*time.Second, "turn end", func() bool {
			return mustSession(t, cs.st, chatID).Status == protocol.ChatStatusDone
		})
	}

	// Create autopilot project + repo + idle commander chat.
	if err := cs.st.CreateProject(ctx, store.Project{Name: "billing", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "api", RemoteURL: "https://example.com/api.git"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	repo := repos[0]

	_, raw := cs.api(t, http.MethodPost, "/api/chats/commander", nil)
	var cmdr store.ChatSession
	_ = json.Unmarshal(raw, &cmdr)
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: cmdr.ID, Content: "watch billing"})
	p0, _ := protocol.DecodePayload[protocol.AgentChatPrompt](w.expect(protocol.MsgAgentChatPrompt, 3*time.Second))
	finishTurn(cmdr.ID, p0.TurnID)

	// Routine progress: creating a ready repo-bound ticket, moving to running, lease renewal,
	// and posting a comment must NEVER trigger a pulse.
	tk1, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "T1", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk1.ID, RepoID: repo.ID})
	tk2, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "T2", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk2.ID, RepoID: repo.ID})
	tk3, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "T3", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk3.ID, RepoID: repo.ID})
	// Extra ready ticket so board does not go idle during the 3 transitions.
	tkKeepBusy, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "T4", Status: store.TicketStatusReady})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkKeepBusy.ID, RepoID: repo.ID})

	// Claim (running), renew lease, comment -> routine progress.
	_, _, _ = cs.st.ClaimTicket(ctx, tk1.ID, "w1", "task-1", time.Now().UTC(), 10*time.Minute)
	_, _ = cs.st.RenewLeases(ctx, "w1", []string{tk1.ID}, time.Now().UTC(), 10*time.Minute)
	_, _ = cs.st.CreateTicketComment(ctx, store.TicketComment{TicketID: tk1.ID, Author: "worker", Body: "still working", CreatedAt: time.Now().UTC()})

	if extra := w.drain(protocol.MsgAgentChatPrompt, 300*time.Millisecond); len(extra) != 0 {
		t.Fatalf("routine progress unexpectedly triggered %d commander prompt(s)", len(extra))
	}

	// Fire 3 actionable transitions within the 120ms debounce window -> must produce ONE pulse.
	tk1.Status = store.TicketStatusReview
	_, _ = cs.st.UpdateTicket(ctx, tk1)
	cs.h.pulseTicketTransition(ctx, tk1, nil, &protocol.RunDigest{Followups: []string{"clean up v1"}}, "")

	tk2.Status = store.TicketStatusFailed
	_, _ = cs.st.UpdateTicket(ctx, tk2)
	cs.h.pulseTicketTransition(ctx, tk2, nil, &protocol.RunDigest{Failure: &protocol.DigestFailure{Category: protocol.ErrKindVerify}}, "")

	tk3.Status = store.TicketStatusNeedsYou
	_, _ = cs.st.UpdateTicket(ctx, tk3)
	cs.h.pulseTicketTransition(ctx, tk3, nil, nil, "question")

	env := expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == cmdr.ID
	})
	prompt, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
	if !strings.HasPrefix(prompt.Text, "[SYRE PULSE | autopilot on | 14:32]\n") {
		t.Fatalf("unexpected pulse header:\n%s", prompt.Text)
	}
	for _, want := range []string{
		tk1.SID + " → review; 1 followup",
		tk2.SID + " → failed (verify)",
		tk3.SID + " → needs you (question)",
	} {
		if !strings.Contains(prompt.Text, want) {
			t.Fatalf("batched pulse missing %q:\n%s", want, prompt.Text)
		}
	}
	if tok := protocol.EstimateTokens(prompt.Text); tok > protocol.PulseMaxTokens {
		t.Fatalf("pulse exceeded %d tokens: got %d\n%s", protocol.PulseMaxTokens, tok, prompt.Text)
	}
	finishTurn(cmdr.ID, prompt.TurnID)

	// Cursor dedupe: no second pulse for the same 3 transitions.
	if extra := w.drain(protocol.MsgAgentChatPrompt, 350*time.Millisecond); len(extra) != 0 {
		t.Fatalf("expected 1 batched pulse, got %d extra prompt(s)", len(extra))
	}
}

func TestPulseDeliveryIdleVsBusyAndScoping(t *testing.T) {
	cs := newChatServer(t)
	cs.h.PulseDebounce = 80 * time.Millisecond
	ctx := context.Background()

	w := cs.connectAgentWorker(t, "w1", "box", claudeAccount("claude", true))
	b := cs.dialBrowser(t)
	var seq atomic.Int64
	finishTurn := func(chatID, turn string) {
		w.send(protocol.MsgAgentChatEvent, protocol.AgentChatEvent{
			ChatID: chatID, Seq: seq.Add(1), Kind: protocol.EventTurnEnd, StopReason: "end_turn", TurnID: turn,
		})
		waitFor(t, 3*time.Second, "turn end", func() bool {
			return mustSession(t, cs.st, chatID).Status == protocol.ChatStatusDone
		})
	}

	// Two projects: projA and projB (autopilot off so we test explicit project scoping).
	_ = cs.st.CreateProject(ctx, store.Project{Name: "projA"})
	_ = cs.st.CreateProject(ctx, store.Project{Name: "projB"})
	projs, _ := cs.st.ListProjects(ctx)
	var projA, projB store.Project
	for _, p := range projs {
		if p.Name == "projA" {
			projA = p
		} else if p.Name == "projB" {
			projB = p
		}
	}

	// Commander A (busy!) owns work in projA; Commander B (idle!) owns work in projB.
	cmdrA := cs.createChat(t, map[string]any{"title": "cmdrA", "fleet_tools": true})
	cmdrB := cs.createChat(t, map[string]any{"title": "cmdrB", "fleet_tools": true})

	// Seed a task in projA owned by cmdrA, and a task in projB owned by cmdrB.
	_ = cs.st.CreateTask(ctx, store.Task{ID: "t-a", SID: "t-a", WorkerID: "w1", ProjectID: projA.ID, ParentChatID: cmdrA.ID, Status: protocol.TaskStatusRunning, Agent: "claude"})
	_ = cs.st.CreateTask(ctx, store.Task{ID: "t-b", SID: "t-b", WorkerID: "w1", ProjectID: projB.ID, ParentChatID: cmdrB.ID, Status: protocol.TaskStatusRunning, Agent: "claude"})

	// Start a turn on cmdrA and leave it BUSY (working).
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: cmdrA.ID, Content: "working on projA"})
	_, _ = protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == cmdrA.ID
	}))

	// Put cmdrB in idle state after a completed turn.
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: cmdrB.ID, Content: "init projB"})
	pb0, _ := protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == cmdrB.ID
	}))
	finishTurn(cmdrB.ID, pb0.TurnID)

	// Trigger a review transition in projA.
	tkA, _ := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: projA.ID, Kind: store.TicketKindFeature, Title: "A work", Status: store.TicketStatusReview})
	cs.h.pulseTicketTransition(ctx, tkA, nil, nil, "")

	// Wait past debounce:
	// - cmdrA is BUSY -> must NOT receive a new turn;
	// - cmdrB is scoped to projB -> must NOT receive a pulse for projA!
	if extra := w.drain(protocol.MsgAgentChatPrompt, 250*time.Millisecond); len(extra) != 0 {
		t.Fatalf("expected no turn for busy cmdrA or unrelated cmdrB, got %d", len(extra))
	}

	// Now send next prompt to cmdrA -> pulse must be prepended to its prompt!
	b.send(protocol.MsgChatSend, chatSendRequest{SessionID: cmdrA.ID, Content: "what next?"})
	pNext, _ := protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == cmdrA.ID
	}))
	if !strings.HasPrefix(pNext.Text, "[SYRE PULSE |") || !strings.Contains(pNext.Text, tkA.SID+" → review") || !strings.HasSuffix(pNext.Text, "what next?") {
		t.Fatalf("expected pulse prepended to busy commander's next prompt, got:\n%s", pNext.Text)
	}

	// Project-level trigger (directive expiring without specific chatID) -> idle cmdrB receives it!
	cs.h.NotifyDirectiveExpiring(ctx, "", "", "directive mini-mac.throttle expiring")
	pDir, _ := protocol.DecodePayload[protocol.AgentChatPrompt](expectWhere(t, w, protocol.MsgAgentChatPrompt, 3*time.Second, func(env protocol.Envelope) bool {
		p, _ := protocol.DecodePayload[protocol.AgentChatPrompt](env)
		return p.ChatID == cmdrB.ID
	}))
	if !strings.HasPrefix(pDir.Text, "[SYRE PULSE |") || !strings.Contains(pDir.Text, "directive mini-mac.throttle expiring") {
		t.Fatalf("expected project-level directive pulse on cmdrB, got:\n%s", pDir.Text)
	}
}

func TestPulseExternalMCPResourceAndNotifications(t *testing.T) {
	cs := newChatServer(t)
	cs.h.PulseDebounce = 80 * time.Millisecond
	ctx := context.Background()

	mcpSrv := NewMCPServer(cs.svc, "test")
	cTrans, sTrans := mcp.NewInMemoryTransports()
	srvSess, err := mcpSrv.Connect(ctx, sTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSess.Close()

	var mu sync.Mutex
	var updatedURIs []string

	client := mcp.NewClient(&mcp.Implementation{Name: "ext-commander", Version: "1.0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			mu.Lock()
			updatedURIs = append(updatedURIs, req.Params.URI)
			mu.Unlock()
		},
	})
	sess, err := client.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Subscribe to syre://pulse and read initial snapshot.
	if err := sess.Subscribe(ctx, &mcp.SubscribeParams{URI: PulseResourceURI}); err != nil {
		t.Fatalf("subscribe syre://pulse: %v", err)
	}
	res0, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: PulseResourceURI})
	if err != nil {
		t.Fatalf("read initial pulse resource: %v", err)
	}
	if len(res0.Contents) == 0 || !strings.HasPrefix(res0.Contents[0].Text, "[SYRE PULSE |") {
		t.Fatalf("unexpected initial pulse resource: %+v", res0.Contents)
	}

	// Trigger a ticket transition -> expect resource updated notification and updated `pulse` resource.
	tk, _ := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: store.DefaultProjectID, Kind: store.TicketKindFeature, Title: "MCP pulse", Status: store.TicketStatusReview})
	cs.h.pulseTicketTransition(ctx, tk, nil, nil, "")

	waitFor(t, 3*time.Second, "external MCP pulse resource updated notification", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(updatedURIs) > 0
	})

	mu.Lock()
	gotURI := updatedURIs[0]
	mu.Unlock()
	if gotURI != PulseResourceURI {
		t.Fatalf("expected resource updated URI %q, got %q", PulseResourceURI, gotURI)
	}

	// Reading the `pulse` resource now returns the latest emitted pulse.
	res1, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: PulseResourceURI})
	if err != nil {
		t.Fatalf("read updated pulse resource: %v", err)
	}
	if len(res1.Contents) == 0 || !strings.Contains(res1.Contents[0].Text, tk.SID+" → review") {
		t.Fatalf("expected ticket transition in pulse resource, got:\n%+v", res1.Contents)
	}
}

func TestPulseDebounceSetting(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()

	if got := cs.h.pulseDebounce(ctx); got != 20*time.Second {
		t.Fatalf("default pulseDebounce = %v, want 20s", got)
	}
	sec := 45
	if err := cs.srv.applySettings(ctx, SettingsUpdate{PulseDebounceSeconds: &sec}, true); err != nil {
		t.Fatalf("applySettings pulse_debounce_seconds: %v", err)
	}
	if got := cs.h.pulseDebounce(ctx); got != 45*time.Second {
		t.Fatalf("updated pulseDebounce = %v, want 45s", got)
	}
	if resp := cs.srv.buildSettingsResponse(ctx); resp.PulseDebounceSeconds != 45 {
		t.Fatalf("settingsResponse.PulseDebounceSeconds = %d, want 45", resp.PulseDebounceSeconds)
	}
}

func TestTicketVerifyFailTwiceProducesOneFailedPulseE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh verify commands")
	}

	cs := newChatServer(t)
	cs.h.PulseDebounce = 100 * time.Millisecond
	ctx := context.Background()

	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")

	_, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	// Connect an external MCP commander subscribed to syre://pulse to observe every pulse emitted.
	mcpSrv := NewMCPServer(cs.svc, "test")
	cTrans, sTrans := mcp.NewInMemoryTransports()
	srvSess, err := mcpSrv.Connect(ctx, sTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSess.Close()
	var mu sync.Mutex
	var pulses []string
	var sess *mcp.ClientSession
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-commander", Version: "1.0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(hctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if req.Params.URI != PulseResourceURI {
				return
			}
			if res, err := sess.ReadResource(hctx, &mcp.ReadResourceParams{URI: PulseResourceURI}); err == nil && len(res.Contents) > 0 {
				mu.Lock()
				pulses = append(pulses, res.Contents[0].Text)
				mu.Unlock()
			}
		},
	})
	sess, err = client.Connect(ctx, cTrans, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Subscribe(ctx, &mcp.SubscribeParams{URI: PulseResourceURI}); err != nil {
		t.Fatalf("subscribe syre://pulse: %v", err)
	}

	if err := cs.st.CreateProject(ctx, store.Project{Name: "PulseVerifyE2E", Autopilot: true}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{
		ProjectID:     proj.ID,
		Name:          "app",
		RemoteURL:     repoOrigin,
		DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	repo := repos[0]

	alwaysFailCmd := "printf '%s\\n' '--- FAIL: TestAlwaysBroken (0.00s)' 'FAIL\texample.com/app\t0.01s' && exit 1"
	tkFail, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindBug,
		Title:     "Verify fails twice",
		Status:    store.TicketStatusReady,
		VerifyCmd: alwaysFailCmd,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tkFail.ID, RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	}

	cs.h.SchedulePending(ctx)

	waitFor(t, 15*time.Second, "ticket reaches failed on attempt 2", func() bool {
		cur, err := cs.st.GetTicket(ctx, tkFail.ID)
		return err == nil && cur.Status == store.TicketStatusFailed && cur.Attempt == 2
	})

	// Wait for the debounced pulse to be pushed.
	waitFor(t, 5*time.Second, "failed pulse delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(pulses) >= 1
	})
	// Wait a bit more to make sure no second pulse arrives.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	got := append([]string(nil), pulses...)
	mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 pulse after 2 verify failures, got %d:\n%v", len(got), got)
	}
	if !strings.Contains(got[0], tkFail.SID+" → failed (verify)") {
		t.Fatalf("expected failed (verify) trigger for %s, got:\n%s", tkFail.SID, got[0])
	}
	if tok := protocol.EstimateTokens(got[0]); tok > protocol.PulseMaxTokens {
		t.Fatalf("e2e pulse exceeded %d tokens: got %d\n%s", protocol.PulseMaxTokens, tok, got[0])
	}
}
