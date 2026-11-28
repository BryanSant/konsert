package controller

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestTicketRunOnLimitsDispatchAndJoinsTheChatScope(t *testing.T) {
	cs, fws := scopeTestFleet(t)
	ctx := context.Background()
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: store.DefaultProjectID, Kind: store.TicketKindFeature, Title: "Fix it", Status: store.TicketStatusBacklog})
	if err != nil {
		t.Fatal(err)
	}

	// REST: default all machines; set by machine names; unknown machines refused.
	if code, raw := cs.api(t, http.MethodGet, "/api/tickets/"+tk.SID+"/scope", nil); code != http.StatusOK || decodeInto[ticketScopeView](t, raw).Label != "All machines" {
		t.Fatalf("default Run on: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/tickets/"+tk.ID+"/scope", map[string]any{"kind": "machines", "ids": []string{"nope"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown machine: %d", code)
	}
	code, raw := cs.api(t, http.MethodPut, "/api/tickets/"+tk.ID+"/scope", map[string]any{"kind": "machines", "ids": []string{"box-2", "box-3"}})
	if v := decodeInto[ticketScopeView](t, raw); code != http.StatusOK || !slices.Equal(v.MachineIDs, []string{"w-2", "w-3"}) {
		t.Fatalf("set Run on: %d %s", code, raw)
	}

	req := func(sel WorkerSelector) DispatchRequest {
		return DispatchRequest{WorkerSelector: sel, TicketID: tk.ID, Agent: "mock", RepoPath: "/repo", Prompt: "p"}
	}
	// Pinned outside the Run on: refused, naming the ticket.
	if _, err := cs.svc.DispatchTask(userCtx(), req(WorkerSelector{WorkerID: "w-1"})); err == nil || !strings.Contains(err.Error(), "box-1 is outside ticket "+tk.SID+"'s Run on (2 machines)") {
		t.Fatalf("pinned outside Run on: %v", err)
	}
	// Unpinned: inside it.
	res, err := cs.svc.DispatchTask(userCtx(), req(WorkerSelector{}))
	if err != nil || (res.WorkerID != "w-2" && res.WorkerID != "w-3") {
		t.Fatalf("unpinned: %+v %v", res, err)
	}
	fws[res.WorkerID].expect(protocol.MsgTaskStart, 3*time.Second)
	logs, _ := cs.st.ListPlacementLog(ctx, store.PlacementLogFilter{Subject: taskSubject(res.TaskID)})
	if len(logs) != 1 || !strings.Contains(logs[0].DetailJSON, "user scope (ticket: 2 machines)") {
		t.Fatalf("placement log = %+v", logs)
	}

	// From a Commander chat both hold: the overlap is the only choice, no overlap is refused.
	mkChat(t, cs.st, "c-1", true)
	if err := cs.st.SetChatScope(ctx, "c-1", store.MachineTarget{Kind: store.TargetMachines, IDs: []string{"w-1", "w-3"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		res, err := cs.svc.DispatchTask(commanderCtx("c-1"), req(WorkerSelector{}))
		if err != nil || res.WorkerID != "w-3" {
			t.Fatalf("chat + ticket dispatch %d: %+v %v", i, res, err)
		}
		fws["w-3"].expect(protocol.MsgTaskStart, 3*time.Second)
	}
	if err := cs.st.SetChatScope(ctx, "c-1", store.MachineTarget{Kind: store.TargetMachine, ID: "w-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.svc.DispatchTask(commanderCtx("c-1"), req(WorkerSelector{})); err == nil || !strings.Contains(err.Error(), "this chat's machines (box-1) and ticket "+tk.SID+"'s Run on (2 machines) share no machine") {
		t.Fatalf("no overlap: %v", err)
	}
}

func TestTicketRunsOnlyWhereRunOnAllows(t *testing.T) {
	cs, fws := scopeTestFleet(t)
	ctx := context.Background()
	if err := cs.st.CreateProject(ctx, store.Project{Name: "RunOn"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "api", RemoteURL: "https://github.com/acme/api.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Build", Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, TargetBranch: "feat/x"})
	if _, err := cs.h.labelMachine(ctx, "w-2", []string{"Fast"}, false); err != nil {
		t.Fatal(err)
	}
	if err := cs.st.SetTicketScope(ctx, tk.ID, store.MachineTarget{Kind: store.TargetLabel, Label: "fast"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	fws["w-2"].expect(protocol.MsgAgentChatOpen, 3*time.Second)
	for _, id := range []string{"w-1", "w-3"} {
		if got := fws[id].drain(protocol.MsgAgentChatOpen, 100*time.Millisecond); len(got) != 0 {
			t.Fatalf("%s got the ticket outside its Run on", id)
		}
	}
	logs, _ := cs.st.ListPlacementLog(ctx, store.PlacementLogFilter{Subject: "ticket:" + tk.ID})
	if len(logs) == 0 || !strings.Contains(logs[0].DetailJSON, "user scope (ticket: label fast)") {
		t.Fatalf("placement log = %+v", logs)
	}
}
