package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func readPacketGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

func TestCompileDispatchPacketGoldenSimple(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	if err := th.st.CreateProject(ctx, store.Project{
		Name:      "billing",
		VerifyCmd: "go test ./pkg/auth/...",
	}); err != nil {
		t.Fatal(err)
	}
	projs, _ := th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]

	if err := th.st.CreateRepo(ctx, store.Repo{
		ProjectID: proj.ID,
		Name:      "api",
		RemoteURL: "https://github.com/acme/api.git",
	}); err != nil {
		t.Fatal(err)
	}
	repos, _ := th.st.ListRepos(ctx, proj.ID)
	repo := repos[0]

	tk, err := th.st.CreateTicket(ctx, store.Ticket{
		SID:       "tk-10001",
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Rotate refresh tokens",
		Body:      "Rotate refresh tokens every 15m and revoke reused families.\n\nSecond paragraph should not be included in the compact packet.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Already done item must be omitted", Done: true},
			{Text: "Rotate on /auth/refresh; invalidate the token family on reuse.", Done: false},
		},
		Status: store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:     tk.ID,
		RepoID:       repo.ID,
		BaseBranch:   "main",
		TargetBranch: "feat/jwt-rotation",
	}); err != nil {
		t.Fatal(err)
	}

	pkt, err := th.h.CompileDispatchPacket(ctx, tk.ID, PacketOptions{WorkdirRoot: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	got := pkt.Text()
	want := readPacketGolden(t, "packet_simple.yaml")
	if got != want {
		t.Fatalf("simple packet mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	tokens := protocol.EstimateTokens(got)
	if tokens > protocol.PacketMaxTokens {
		t.Fatalf("simple packet tokens = %d > %d", tokens, protocol.PacketMaxTokens)
	}
	t.Logf("packet_simple tokens=%d chars=%d", tokens, len(got))
}

func TestCompileDispatchPacketGoldenMultiRepoWithContracts(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	if err := th.st.CreateProject(ctx, store.Project{
		Name:      "billing",
		VerifyCmd: "make test",
	}); err != nil {
		t.Fatal(err)
	}
	projs, _ := th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]

	if err := th.st.CreateRepo(ctx, store.Repo{
		ProjectID: proj.ID,
		Name:      "api",
		RemoteURL: "https://github.com/acme/api.git",
		VerifyCmd: "go test ./pkg/auth/...",
	}); err != nil {
		t.Fatal(err)
	}
	if err := th.st.CreateRepo(ctx, store.Repo{
		ProjectID: proj.ID,
		Name:      "web",
		RemoteURL: "https://github.com/acme/web.git",
		VerifyCmd: "npm --prefix web test",
	}); err != nil {
		t.Fatal(err)
	}
	repos, _ := th.st.ListRepos(ctx, proj.ID)
	repoByName := map[string]store.Repo{}
	for _, r := range repos {
		repoByName[r.Name] = r
	}

	// Done blocker with interface digest -> MUST appear in contracts.
	blockerDone, err := th.st.CreateTicket(ctx, store.Ticket{
		SID:       "tk-20001",
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Add /v2/auth/refresh endpoint",
		Status:    store.TicketStatusDone,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Done blocker with empty interface digest -> MUST be omitted.
	blockerEmpty, err := th.st.CreateTicket(ctx, store.Ticket{
		SID:       "tk-20000",
		ProjectID: proj.ID,
		Kind:      store.TicketKindChore,
		Title:     "Cleanup lint",
		Status:    store.TicketStatusDone,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Non-done blocker -> MUST be omitted.
	blockerPending, err := th.st.CreateTicket(ctx, store.Ticket{
		SID:       "tk-20009",
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Pending blocker",
		Status:    store.TicketStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Store two tasks on blockerDone to verify latest digest wins.
	oldTime := time.Now().UTC().Add(-2 * time.Hour)
	newTime := time.Now().UTC().Add(-1 * time.Hour)
	if err := th.st.CreateTask(ctx, store.Task{
		ID: "t-old", Agent: "claude", Workdir: "/work", Prompt: "old", Status: protocol.TaskStatusCompleted,
		TicketID: blockerDone.ID, CreatedAt: oldTime,
	}); err != nil {
		t.Fatal(err)
	}
	oldDig, _ := json.Marshal(protocol.RunDigest{Status: protocol.DigestStatusReview, Interface: "OLD interface"}, protocol.JSONOptions)
	_ = th.st.UpdateTaskDigest(ctx, "t-old", string(oldDig))

	if err := th.st.CreateTask(ctx, store.Task{
		ID: "t-new", Agent: "claude", Workdir: "/work", Prompt: "new", Status: protocol.TaskStatusCompleted,
		TicketID: blockerDone.ID, CreatedAt: newTime,
	}); err != nil {
		t.Fatal(err)
	}
	newDig, _ := json.Marshal(protocol.RunDigest{
		Status:    protocol.DigestStatusReview,
		Interface: "POST /v2/auth/refresh returns {access_token, refresh_token, expires_in}",
	}, protocol.JSONOptions)
	_ = th.st.UpdateTaskDigest(ctx, "t-new", string(newDig))

	if err := th.st.CreateTask(ctx, store.Task{
		ID: "t-empty", Agent: "claude", Workdir: "/work", Prompt: "empty", Status: protocol.TaskStatusCompleted,
		TicketID: blockerEmpty.ID, CreatedAt: newTime,
	}); err != nil {
		t.Fatal(err)
	}
	emptyDig, _ := json.Marshal(protocol.RunDigest{Status: protocol.DigestStatusReview}, protocol.JSONOptions)
	_ = th.st.UpdateTaskDigest(ctx, "t-empty", string(emptyDig))

	tk, err := th.st.CreateTicket(ctx, store.Ticket{
		SID:       "tk-20002",
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Rotate refresh tokens every 15m and retry once on 401 in the web client.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Rotate on /auth/refresh; invalidate the token family on reuse."},
			{Text: "Web fetch wrapper retries once on 401."},
		},
		Status: store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.Repo{repoByName["web"], repoByName["api"]} {
		if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
			TicketID:     tk.ID,
			RepoID:       r.ID,
			BaseBranch:   "main",
			TargetBranch: "feat/jwt-rotation",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = th.st.AddTicketEdge(ctx, tk.ID, blockerDone.ID)
	_ = th.st.AddTicketEdge(ctx, tk.ID, blockerEmpty.ID)
	_ = th.st.AddTicketEdge(ctx, tk.ID, blockerPending.ID)

	pkt, err := th.h.CompileDispatchPacket(ctx, tk.ID, PacketOptions{WorkdirRoot: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	got := pkt.Text()
	want := readPacketGolden(t, "packet_multi_repo_contracts.yaml")
	if got != want {
		t.Fatalf("multi-repo packet mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	tokens := protocol.EstimateTokens(got)
	if tokens > protocol.PacketMaxTokens {
		t.Fatalf("multi-repo packet tokens = %d > %d", tokens, protocol.PacketMaxTokens)
	}
	t.Logf("packet_multi_repo_contracts tokens=%d chars=%d", tokens, len(got))

	// Verify precedence: ticket verify_cmd overrides repo verify_cmd.
	tk.VerifyCmd = "go test ./..."
	if _, err := th.st.UpdateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	pkt2, err := th.h.CompileDispatchPacket(ctx, tk.ID, PacketOptions{WorkdirRoot: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if pkt2.Verify != "go test ./..." {
		t.Fatalf("ticket verify_cmd precedence: got %q, want %q", pkt2.Verify, "go test ./...")
	}
}

func TestDispatchPacketTokenCapTrimming(t *testing.T) {
	// Case 1: Long objective trimmed first while contracts remain intact.
	p1 := protocol.DispatchPacket{
		TicketSID:  "tk-cap1",
		Project:    "billing",
		Repos:      []protocol.DispatchPacketRepo{{Name: "api", Path: "/work/tk-cap1/api", Branch: "feat/cap"}},
		Objective:  strings.Repeat("Implement detailed rotation logic with strict auditability. ", 40),
		Acceptance: []string{"Token rotation works", "Reuse invalidates family"},
		Contracts:  []string{"tk-prev1 (done): POST /v2/auth/refresh returns token pair"},
		Verify:     "go test ./...",
		Tools:      DefaultTicketTools,
	}
	origObj := p1.Objective
	out1 := p1.Text()
	if tok := protocol.EstimateTokens(out1); tok > protocol.PacketMaxTokens {
		t.Fatalf("case 1 exceeded cap: %d > %d\n%s", tok, protocol.PacketMaxTokens, out1)
	}
	if !strings.Contains(out1, "tk-prev1 (done):") {
		t.Fatalf("case 1 dropped contract when trimming objective alone should suffice:\n%s", out1)
	}
	if !strings.Contains(out1, "...") {
		t.Fatalf("case 1 expected trimmed objective with ellipsis:\n%s", out1)
	}
	if p1.Objective != origObj {
		t.Fatalf("Text() mutated receiver Objective")
	}

	// Case 2: Many contracts exceed the budget even without objective -> contracts trimmed too.
	var manyContracts []string
	for i := 0; i < 20; i++ {
		manyContracts = append(manyContracts, "tk-blk-"+string(rune('a'+i))+" (done): exports ServiceHandler with strict validation and pagination metadata")
	}
	p2 := protocol.DispatchPacket{
		TicketSID:  "tk-cap2",
		Project:    "billing",
		Repos:      []protocol.DispatchPacketRepo{{Name: "api", Path: "/work/tk-cap2/api", Branch: "feat/cap"}},
		Objective:  "Wire up the billing dashboard with all upstream services.",
		Acceptance: []string{"Dashboard renders"},
		Contracts:  manyContracts,
		Verify:     "go test ./...",
		Tools:      DefaultTicketTools,
	}
	out2 := p2.Text()
	if tok := protocol.EstimateTokens(out2); tok > protocol.PacketMaxTokens {
		t.Fatalf("case 2 exceeded cap: %d > %d\n%s", tok, protocol.PacketMaxTokens, out2)
	}
	if strings.Contains(out2, "tk-blk-t (done):") {
		t.Fatalf("case 2 should have trimmed trailing contracts:\n%s", out2)
	}
	if !strings.Contains(out2, "tk-blk-a (done):") {
		t.Fatalf("case 2 should preserve earlier contracts that fit:\n%s", out2)
	}
}

func TestSchedulerUsesCompiledPacketPrompt(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()

	proj, repo := createProjectAndRepo(t, th.st, "Billing", true, false)
	tk, err := th.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Rotate tokens",
		Body:      "Rotate refresh tokens every 15m.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Invalidate token family on reuse"},
		},
		VerifyCmd: "go test ./...",
		Status:    store.TicketStatusReady,
		Hints:     store.TicketHints{Agent: "mock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:     tk.ID,
		RepoID:       repo.ID,
		BaseBranch:   "main",
		TargetBranch: "feat/rotate",
	}); err != nil {
		t.Fatal(err)
	}

	wi := testWorkerInfo("w-pkt", "box-pkt")
	wi.Facts = &protocol.MachineFacts{WorkdirRoot: "/work"}
	fw := th.connectWorker(t, wi, nil, "w-pkt")

	env := fw.expect(protocol.MsgTaskStart, 3*time.Second)
	p, err := protocol.DecodePayload[protocol.TaskStartPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Prompt, "ticket: "+tk.SID) {
		t.Fatalf("prompt missing compiled packet header:\n%s", p.Prompt)
	}
	wantPath := "/work/" + tk.SID + "/" + repo.Name
	if !strings.Contains(p.Prompt, "path: "+wantPath) {
		t.Fatalf("prompt missing post-placement worktree path %q:\n%s", wantPath, p.Prompt)
	}
	if !strings.HasSuffix(p.Prompt, DispatchStandingInstruction) {
		t.Fatalf("prompt missing standing instruction:\n%s", p.Prompt)
	}
}

func TestTicketPacketAndCommentsREST(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "rest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	proj, repo := createProjectAndRepo(t, st, "Billing", false, false)
	tk, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Add metrics",
		Body:      "Expose /metrics for Prometheus.",
		Acceptance: []store.AcceptanceItem{
			{Text: "Return 200 on /metrics"},
		},
		Status: store.TicketStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repo.ID, TargetBranch: "feat/metrics"})

	// Preview packet via GET /api/tickets/{sid}/packet.
	resp, err := http.Get(ts.URL + "/api/tickets/" + tk.SID + "/packet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /packet status = %d", resp.StatusCode)
	}
	var pktResp struct {
		Packet protocol.DispatchPacket `json:"packet"`
		Text   string                  `json:"text"`
		Tokens int                     `json:"tokens"`
	}
	if err := json.UnmarshalRead(resp.Body, &pktResp, protocol.JSONOptions); err != nil {
		t.Fatal(err)
	}
	if pktResp.Packet.TicketSID != tk.SID || !strings.Contains(pktResp.Text, "Add metrics") || pktResp.Tokens <= 0 || pktResp.Tokens > protocol.PacketMaxTokens {
		t.Fatalf("unexpected packet response: %+v", pktResp)
	}

	// Comments REST: initially empty, then returns created comments.
	if _, err := st.CreateTicketComment(ctx, store.TicketComment{
		TicketID: tk.ID,
		Author:   "c-12345",
		Body:     "Implemented handler; running tests.",
	}); err != nil {
		t.Fatal(err)
	}
	cResp, err := http.Get(ts.URL + "/api/tickets/" + tk.SID + "/comments")
	if err != nil {
		t.Fatal(err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /comments status = %d", cResp.StatusCode)
	}
	var commentsBody struct {
		Comments []store.TicketComment `json:"comments"`
	}
	if err := json.UnmarshalRead(cResp.Body, &commentsBody, protocol.JSONOptions); err != nil {
		t.Fatal(err)
	}
	if len(commentsBody.Comments) != 1 || commentsBody.Comments[0].Author != "c-12345" || commentsBody.Comments[0].Body != "Implemented handler; running tests." {
		t.Fatalf("unexpected comments: %+v", commentsBody.Comments)
	}

	// Unknown ticket returns 404.
	nfResp, err := http.Get(ts.URL + "/api/tickets/does-not-exist/comments")
	if err != nil {
		t.Fatal(err)
	}
	defer nfResp.Body.Close()
	if nfResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown ticket comments = %d, want 404", nfResp.StatusCode)
	}
}
