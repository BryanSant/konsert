package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

func TestProjectsAndReposRegistry(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Project with verify_cmd and autonomy defaults.
	proj := store.Project{
		ID:        ids.UUID(),
		Name:      "Billing",
		VerifyCmd: "go test ./...",
		Autonomy: store.AutonomyPolicy{
			RetryLimit:        intPtr(3),
			CrossReview:       boolPtr(true),
			EscalateOnBlocker: boolPtr(false),
			ExpandScope:       boolPtr(true),
		},
		DeliveryMode: "pr_merge",
		MergeStyle:   "squash",
	}
	if err := st.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	gotP, err := st.GetProject(ctx, proj.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if gotP.VerifyCmd != "go test ./..." || gotP.Autonomy.RetryLimit == nil || *gotP.Autonomy.RetryLimit != 3 ||
		gotP.Autonomy.CrossReview == nil || !*gotP.Autonomy.CrossReview ||
		gotP.Autonomy.EscalateOnBlocker == nil || *gotP.Autonomy.EscalateOnBlocker ||
		gotP.DeliveryMode != "pr_merge" || gotP.MergeStyle != "squash" {
		t.Fatalf("unexpected project roundtrip: %+v", gotP)
	}

	gotP.VerifyCmd = "make test"
	gotP.Autonomy.RetryLimit = intPtr(1)
	if err := st.UpdateProject(ctx, gotP); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	gotP2, _ := st.GetProject(ctx, proj.ID)
	if gotP2.VerifyCmd != "make test" || *gotP2.Autonomy.RetryLimit != 1 {
		t.Fatalf("UpdateProject did not persist: %+v", gotP2)
	}

	// Default project cannot be deleted.
	if err := st.DeleteProject(ctx, store.DefaultProjectID); !errors.Is(err, store.ErrProtectedDefault) {
		t.Fatalf("DeleteProject(default) = %v, want ErrProtectedDefault", err)
	}

	// Repos keyed by remote_url per project.
	r1 := store.Repo{
		ID:            ids.UUID(),
		ProjectID:     proj.ID,
		Name:          "api",
		RemoteURL:     "https://github.com/acme/api.git",
		DefaultBranch: "main",
		VerifyCmd:     "go test ./pkg/...",
		DeliveryMode:  "direct",
		MergeStyle:    "rebase",
	}
	if err := st.CreateRepo(ctx, r1); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	// Duplicate remote_url in same project must fail.
	if err := st.CreateRepo(ctx, store.Repo{
		ProjectID: proj.ID,
		Name:      "api-dup",
		RemoteURL: "https://github.com/acme/api.git",
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate remote_url err = %v, want ErrDuplicate", err)
	}
	// Same remote_url in another project is allowed.
	if err := st.CreateRepo(ctx, store.Repo{
		ProjectID: store.DefaultProjectID,
		Name:      "api",
		RemoteURL: "https://github.com/acme/api.git",
	}); err != nil {
		t.Fatalf("same remote_url in different project failed: %v", err)
	}
	// Missing remote_url is rejected.
	if err := st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "bad"}); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("empty remote_url err = %v, want ErrInvalidWork", err)
	}

	gotR, err := st.GetRepo(ctx, r1.ID)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if gotR.DefaultBranch != "main" || gotR.VerifyCmd != "go test ./pkg/..." || gotR.RemoteURL != r1.RemoteURL {
		t.Fatalf("unexpected repo roundtrip: %+v", gotR)
	}

	gotR.DefaultBranch = "trunk"
	gotR.VerifyCmd = "go test ./..."
	if err := st.UpdateRepo(ctx, gotR); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	gotR2, _ := st.GetRepo(ctx, r1.ID)
	if gotR2.DefaultBranch != "trunk" || gotR2.VerifyCmd != "go test ./..." {
		t.Fatalf("UpdateRepo did not persist: %+v", gotR2)
	}

	// Deleting a project re-homes tasks to DefaultProjectID and clears repo_id.
	if err := st.CreateTask(ctx, store.Task{
		ID:        "t-rehome",
		ProjectID: proj.ID,
		RepoID:    r1.ID,
		Agent:     "claude",
		Workdir:   "/work/api",
		Prompt:    "hi",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProject(ctx, proj.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	gotT, err := st.GetTask(ctx, "t-rehome")
	if err != nil {
		t.Fatal(err)
	}
	if gotT.ProjectID != store.DefaultProjectID || gotT.RepoID != "" {
		t.Fatalf("task not rehomed on project delete: %+v", gotT)
	}
}

func TestTicketsCRUDAndHierarchyValidation(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	projA := store.Project{ID: ids.UUID(), Name: "ProjA"}
	projB := store.Project{ID: ids.UUID(), Name: "ProjB"}
	if err := st.CreateProject(ctx, projA); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProject(ctx, projB); err != nil {
		t.Fatal(err)
	}

	// Create initiative -> epic -> feature -> bug (sub-ticket).
	initT, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		Kind:      store.TicketKindInitiative,
		Title:     "Autopilot",
		Priority:  "P0",
		Status:    store.TicketStatusReady,
	})
	if err != nil {
		t.Fatalf("create initiative: %v", err)
	}
	if !ids.IsUUIDv7(initT.ID) || !ids.Valid(ids.Ticket, initT.SID) {
		t.Fatalf("bad ticket ids: %s / %s", initT.ID, initT.SID)
	}

	epicT, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		ParentID:  initT.ID,
		Kind:      store.TicketKindEpic,
		Title:     "Work model",
		Priority:  "P1",
	})
	if err != nil {
		t.Fatalf("create epic under initiative: %v", err)
	}
	if epicT.Status != store.TicketStatusBacklog {
		t.Fatalf("default status = %q, want backlog", epicT.Status)
	}

	featT, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		ParentID:  epicT.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Schema and store",
		Body:      "Implement work model tables",
		Acceptance: []store.AcceptanceItem{
			{Text: "Schema compiles", Done: true},
			{Text: "Tests pass", Done: false},
		},
		Labels:    []string{"backend", "sqlite"},
		Priority:  "P1",
		Status:    store.TicketStatusRunning,
		VerifyCmd: "go test ./internal/store/...",
		Hints: store.TicketHints{
			Agent:    "claude",
			Model:    "opus",
			OS:       "darwin",
			GPU:      true,
			MinRAMGB: 16,
			Tools:    []string{"go", "git"},
		},
		Autonomy: store.AutonomyPolicy{
			RetryLimit:  intPtr(2),
			CrossReview: boolPtr(true),
		},
	})
	if err != nil {
		t.Fatalf("create feature under epic: %v", err)
	}

	gotFeat, err := st.GetTicket(ctx, featT.ID)
	if err != nil {
		t.Fatalf("GetTicket: %v", err)
	}
	if len(gotFeat.Acceptance) != 2 || !gotFeat.Acceptance[0].Done || gotFeat.Acceptance[1].Done ||
		len(gotFeat.Labels) != 2 || gotFeat.Hints.Agent != "claude" || !gotFeat.Hints.GPU ||
		gotFeat.Autonomy.RetryLimit == nil || *gotFeat.Autonomy.RetryLimit != 2 {
		t.Fatalf("ticket roundtrip mismatch: %+v", gotFeat)
	}

	// Sub-ticket under feature is allowed by ka rules.
	subBug, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		ParentID:  featT.ID,
		Kind:      store.TicketKindBug,
		Title:     "Fix edge case",
	})
	if err != nil {
		t.Fatalf("create bug under feature: %v", err)
	}

	// Invalid hierarchy cases (ka spec §4.5):
	// 1. Initiative cannot have any parent.
	if _, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		ParentID:  epicT.ID,
		Kind:      store.TicketKindInitiative,
		Title:     "Bad initiative",
	}); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("initiative with parent err = %v, want ErrInvalidWork", err)
	}
	// 2. Epic cannot have an epic parent.
	if _, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		ParentID:  epicT.ID,
		Kind:      store.TicketKindEpic,
		Title:     "Nested epic",
	}); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("epic under epic err = %v, want ErrInvalidWork", err)
	}
	// 3. Epic cannot have a feature parent.
	if _, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projA.ID,
		ParentID:  featT.ID,
		Kind:      store.TicketKindEpic,
		Title:     "Epic under feature",
	}); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("epic under feature err = %v, want ErrInvalidWork", err)
	}
	// 4. Cross-project parent is rejected.
	if _, err := st.CreateTicket(ctx, store.Ticket{
		ProjectID: projB.ID,
		ParentID:  epicT.ID,
		Kind:      store.TicketKindFeature,
		Title:     "Cross project",
	}); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("cross-project parent err = %v, want ErrInvalidWork", err)
	}
	// 5. Demoting initiative to feature while it has an epic child must fail.
	badDemote := initT
	badDemote.Kind = store.TicketKindFeature
	if _, err := st.UpdateTicket(ctx, badDemote); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("demoting initiative with epic child err = %v, want ErrInvalidWork", err)
	}
	// 6. Parent cycle (setting featT's parent to subBug) must fail with ErrCycle.
	cycFeat := featT
	cycFeat.ParentID = subBug.ID
	if _, err := st.UpdateTicket(ctx, cycFeat); !errors.Is(err, store.ErrCycle) {
		t.Fatalf("parent cycle err = %v, want ErrCycle", err)
	}

	// ListTickets filtering.
	byParent, err := st.ListTickets(ctx, store.TicketFilter{ProjectID: projA.ID, ParentID: epicT.ID})
	if err != nil || len(byParent) != 1 || byParent[0].ID != featT.ID {
		t.Fatalf("ListTickets(parent) = %+v, %v", byParent, err)
	}
	byStatus, err := st.ListTickets(ctx, store.TicketFilter{ProjectID: projA.ID, Status: store.TicketStatusRunning})
	if err != nil || len(byStatus) != 1 || byStatus[0].ID != featT.ID {
		t.Fatalf("ListTickets(status) = %+v, %v", byStatus, err)
	}
	roots, err := st.ListTickets(ctx, store.TicketFilter{ProjectID: projA.ID, RootOnly: true})
	if err != nil || len(roots) != 1 || roots[0].ID != initT.ID {
		t.Fatalf("ListTickets(RootOnly) = %+v, %v", roots, err)
	}
}

func TestTicketEdgesCycleRejection(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	mk := func(title string) store.Ticket {
		t.Helper()
		tk, err := st.CreateTicket(ctx, store.Ticket{
			ProjectID: store.DefaultProjectID,
			Kind:      store.TicketKindFeature,
			Title:     title,
		})
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	a, b, c, d := mk("A"), mk("B"), mk("C"), mk("D")

	// Self-loop rejected.
	if err := st.AddTicketEdge(ctx, a.ID, a.ID); !errors.Is(err, store.ErrCycle) {
		t.Fatalf("self-loop err = %v, want ErrCycle", err)
	}

	// A blocked_by B, B blocked_by C.
	if err := st.AddTicketEdge(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("A->B: %v", err)
	}
	if err := st.AddTicketEdge(ctx, b.ID, c.ID); err != nil {
		t.Fatalf("B->C: %v", err)
	}
	// Direct 2-node cycle B blocked_by A rejected.
	if err := st.AddTicketEdge(ctx, b.ID, a.ID); !errors.Is(err, store.ErrCycle) {
		t.Fatalf("2-node cycle err = %v, want ErrCycle", err)
	}
	// Transitive 3-node cycle C blocked_by A rejected.
	if err := st.AddTicketEdge(ctx, c.ID, a.ID); !errors.Is(err, store.ErrCycle) {
		t.Fatalf("3-node cycle err = %v, want ErrCycle", err)
	}
	// Diamond DAG allowed: A blocked_by D, D blocked_by C.
	if err := st.AddTicketEdge(ctx, a.ID, d.ID); err != nil {
		t.Fatalf("A->D: %v", err)
	}
	if err := st.AddTicketEdge(ctx, d.ID, c.ID); err != nil {
		t.Fatalf("D->C: %v", err)
	}

	edges, err := st.ListTicketEdges(ctx, a.ID)
	if err != nil || len(edges) != 2 {
		t.Fatalf("ListTicketEdges(A) = %+v, %v", edges, err)
	}
	blocking, err := st.ListTicketBlocking(ctx, c.ID)
	if err != nil || len(blocking) != 2 {
		t.Fatalf("ListTicketBlocking(C) = %+v, %v", blocking, err)
	}

	// Removing B->C breaks the chain so C->A becomes valid.
	if err := st.RemoveTicketEdge(ctx, b.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveTicketEdge(ctx, d.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTicketEdge(ctx, c.ID, a.ID); err != nil {
		t.Fatalf("C->A after removing chain failed: %v", err)
	}
}

func TestTicketRepoBindingsAndExternalRefs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	otherProj := store.Project{ID: ids.UUID(), Name: "Other"}
	if err := st.CreateProject(ctx, otherProj); err != nil {
		t.Fatal(err)
	}
	repo1 := store.Repo{ID: ids.UUID(), ProjectID: store.DefaultProjectID, Name: "api", RemoteURL: "https://github.com/org/api.git"}
	repoOther := store.Repo{ID: ids.UUID(), ProjectID: otherProj.ID, Name: "other", RemoteURL: "https://github.com/org/other.git"}
	if err := st.CreateRepo(ctx, repo1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRepo(ctx, repoOther); err != nil {
		t.Fatal(err)
	}
	tk, err := st.CreateTicket(ctx, store.Ticket{ProjectID: store.DefaultProjectID, Kind: store.TicketKindFeature, Title: "Bind me"})
	if err != nil {
		t.Fatal(err)
	}

	// Cross-project repo binding rejected.
	if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repoOther.ID}); !errors.Is(err, store.ErrInvalidWork) {
		t.Fatalf("cross-project binding err = %v, want ErrInvalidWork", err)
	}

	if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:     tk.ID,
		RepoID:       repo1.ID,
		BaseBranch:   "main",
		TargetBranch: "feat/bind",
	}); err != nil {
		t.Fatalf("PutTicketRepoBinding: %v", err)
	}
	// Upsert updates branches.
	if err := st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
		TicketID:     tk.ID,
		RepoID:       repo1.ID,
		BaseBranch:   "develop",
		TargetBranch: "feat/bind-v2",
	}); err != nil {
		t.Fatal(err)
	}
	bindings, err := st.ListTicketRepoBindings(ctx, tk.ID)
	if err != nil || len(bindings) != 1 || bindings[0].BaseBranch != "develop" || bindings[0].TargetBranch != "feat/bind-v2" {
		t.Fatalf("ListTicketRepoBindings = %+v, %v", bindings, err)
	}
	if err := st.RemoveTicketRepoBinding(ctx, tk.ID, repo1.ID); err != nil {
		t.Fatal(err)
	}

	// External refs.
	if err := st.PutExternalRef(ctx, store.ExternalRef{
		TicketID:   tk.ID,
		Tracker:    "ka",
		ExternalID: "SYR-Y03N8",
		URL:        "https://example.com/SYR-Y03N8",
		Version:    "v1",
	}); err != nil {
		t.Fatalf("PutExternalRef: %v", err)
	}
	refs, err := st.ListExternalRefs(ctx, tk.ID)
	if err != nil || len(refs) != 1 || refs[0].ExternalID != "SYR-Y03N8" || refs[0].Version != "v1" {
		t.Fatalf("ListExternalRefs = %+v, %v", refs, err)
	}
	if err := st.RemoveExternalRef(ctx, tk.ID, "ka", "SYR-Y03N8"); err != nil {
		t.Fatal(err)
	}
}

func TestTicketRollupsComputed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	initT, err := st.CreateTicket(ctx, store.Ticket{Kind: store.TicketKindInitiative, Title: "Init"})
	if err != nil {
		t.Fatal(err)
	}
	epic1, err := st.CreateTicket(ctx, store.Ticket{ParentID: initT.ID, Kind: store.TicketKindEpic, Title: "Epic 1"})
	if err != nil {
		t.Fatal(err)
	}
	epic2, err := st.CreateTicket(ctx, store.Ticket{ParentID: initT.ID, Kind: store.TicketKindEpic, Title: "Epic 2"})
	if err != nil {
		t.Fatal(err)
	}

	// Under epic1: 1 done, 1 running, 1 ready blocked by running.
	tDone, _ := st.CreateTicket(ctx, store.Ticket{ParentID: epic1.ID, Kind: store.TicketKindFeature, Title: "Done", Status: store.TicketStatusDone})
	tRun, _ := st.CreateTicket(ctx, store.Ticket{ParentID: epic1.ID, Kind: store.TicketKindFeature, Title: "Running", Status: store.TicketStatusRunning})
	tBlockedByEdge, _ := st.CreateTicket(ctx, store.Ticket{ParentID: epic1.ID, Kind: store.TicketKindFeature, Title: "Blocked", Status: store.TicketStatusReady})
	if err := st.AddTicketEdge(ctx, tBlockedByEdge.ID, tRun.ID); err != nil {
		t.Fatal(err)
	}
	// Under epic2: 1 needs_you (counts as blocked), 1 ready blocked by tDone (NOT blocked since blocker is done).
	_, _ = st.CreateTicket(ctx, store.Ticket{ParentID: epic2.ID, Kind: store.TicketKindBug, Title: "Needs human", Status: store.TicketStatusNeedsYou})
	tUnblocked, _ := st.CreateTicket(ctx, store.Ticket{ParentID: epic2.ID, Kind: store.TicketKindChore, Title: "Ready", Status: store.TicketStatusReady})
	if err := st.AddTicketEdge(ctx, tUnblocked.ID, tDone.ID); err != nil {
		t.Fatal(err)
	}

	rEpic1, err := st.GetTicketRollup(ctx, epic1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rEpic1.Total != 3 || rEpic1.Done != 1 || rEpic1.Running != 1 || rEpic1.Blocked != 1 {
		t.Fatalf("epic1 rollup = %+v, want total=3 done=1 running=1 blocked=1", rEpic1)
	}

	rEpic2, err := st.GetTicketRollup(ctx, epic2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rEpic2.Total != 2 || rEpic2.Done != 0 || rEpic2.Running != 0 || rEpic2.Blocked != 1 {
		t.Fatalf("epic2 rollup = %+v, want total=2 done=0 running=0 blocked=1", rEpic2)
	}

	// Initiative rolls up all non-container descendants across both epics (5 total, not 7).
	rInit, err := st.GetTicketRollup(ctx, initT.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rInit.Total != 5 || rInit.Done != 1 || rInit.Running != 1 || rInit.Blocked != 2 {
		t.Fatalf("initiative rollup = %+v, want total=5 done=1 running=1 blocked=2", rInit)
	}

	// Complete tRun -> tBlockedByEdge is no longer blocked, done becomes 2, running becomes 0.
	tRun.Status = store.TicketStatusDone
	if _, err := st.UpdateTicket(ctx, tRun); err != nil {
		t.Fatal(err)
	}
	rInitAfter, err := st.GetTicketRollup(ctx, initT.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rInitAfter.Total != 5 || rInitAfter.Done != 2 || rInitAfter.Running != 0 || rInitAfter.Blocked != 1 {
		t.Fatalf("initiative rollup after completion = %+v, want total=5 done=2 running=0 blocked=1", rInitAfter)
	}
}
