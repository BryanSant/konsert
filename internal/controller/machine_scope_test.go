package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func scopeTestFleet(t *testing.T) (*chatServer, map[string]*fakeWorker) {
	t.Helper()
	cs := newChatServer(t)
	fws := map[string]*fakeWorker{}
	for _, w := range []struct{ id, name string }{{"w-1", "box-1"}, {"w-2", "box-2"}, {"w-3", "box-3"}} {
		fws[w.id] = cs.connectWorker(t, testWorkerInfo(w.id, w.name), nil, w.id)
	}
	return cs, fws
}

func mkChat(t *testing.T, st store.Store, id string, commander bool) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.CreateChatSession(context.Background(), store.ChatSession{
		ID: id, Backend: store.ChatBackendAgent, Agent: "mock", WorkerID: "w-1", FleetTools: commander,
		Status: protocol.ChatStatusIdle, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func decodeInto[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func TestMachineLabelsGroupsAndChatScopeREST(t *testing.T) {
	cs, _ := scopeTestFleet(t)

	// Labels: by machine ID or name; empty labels and unknown machines are refused.
	if code, raw := cs.api(t, http.MethodPost, "/api/workers/w-1/labels", map[string]string{"label": " Work "}); code != http.StatusOK ||
		!slices.Equal(decodeInto[machineLabelsUpdate](t, raw).Labels, []string{"Work"}) {
		t.Fatalf("add label: %d %s", code, raw)
	}
	if code, raw := cs.api(t, http.MethodPost, "/api/workers/box-2/labels", map[string]string{"label": "Gaming hardware"}); code != http.StatusOK ||
		decodeInto[machineLabelsUpdate](t, raw).WorkerID != "w-2" {
		t.Fatalf("add label by name: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodPost, "/api/workers/w-1/labels", map[string]string{"label": "  "}); code != http.StatusBadRequest {
		t.Fatalf("empty label: %d", code)
	}
	if code, _ := cs.api(t, http.MethodPost, "/api/workers/w-nope/labels", map[string]string{"label": "Work"}); code != http.StatusNotFound {
		t.Fatalf("unknown machine: %d", code)
	}

	// The fleet snapshot carries labels and groups.
	_, raw := cs.api(t, http.MethodGet, "/api/fleet", nil)
	fleet := decodeInto[struct {
		Workers []WorkerStatusView   `json:"workers"`
		Groups  []store.MachineGroup `json:"machine_groups"`
	}](t, raw)
	labelsOf := map[string][]string{}
	for _, w := range fleet.Workers {
		labelsOf[w.ID] = w.Labels
	}
	if !slices.Equal(labelsOf["w-1"], []string{"Work"}) || !slices.Equal(labelsOf["w-2"], []string{"Gaming hardware"}) || labelsOf["w-3"] != nil || fleet.Groups == nil {
		t.Fatalf("fleet labels %v groups %v", labelsOf, fleet.Groups)
	}

	// Groups: create (names or IDs), duplicate names refused, rename, delete.
	code, raw := cs.api(t, http.MethodPost, "/api/machine-groups", map[string]any{"name": "Lab", "worker_ids": []string{"box-1", "w-3"}})
	g := decodeInto[store.MachineGroup](t, raw)
	if code != http.StatusCreated || !slices.Equal(g.WorkerIDs, []string{"w-1", "w-3"}) {
		t.Fatalf("create group: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodPost, "/api/machine-groups", map[string]any{"name": "lab"}); code != http.StatusConflict {
		t.Fatalf("duplicate group: %d", code)
	}
	if code, _ := cs.api(t, http.MethodPost, "/api/machine-groups", map[string]any{"name": "Ghosts", "worker_ids": []string{"nope"}}); code != http.StatusBadRequest {
		t.Fatalf("group with unknown machine: %d", code)
	}
	if code, raw := cs.api(t, http.MethodPut, "/api/machine-groups/"+g.ID, map[string]any{"name": "Home lab", "worker_ids": []string{"w-2"}}); code != http.StatusOK ||
		decodeInto[store.MachineGroup](t, raw).Name != "Home lab" {
		t.Fatalf("update group: %d %s", code, raw)
	}

	// Chat scope: only Commander chats; targets must name things that exist.
	mkChat(t, cs.st, "c-cmd", true)
	mkChat(t, cs.st, "c-agent", false)
	if code, raw := cs.api(t, http.MethodGet, "/api/chats/c-cmd/scope", nil); code != http.StatusOK || decodeInto[chatScopeView](t, raw).Label != "All machines" {
		t.Fatalf("default scope: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/chats/c-agent/scope", map[string]string{"kind": "label", "label": "Work"}); code != http.StatusBadRequest {
		t.Fatalf("scope on an agent chat: %d", code)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/chats/c-cmd/scope", map[string]string{"kind": "group", "id": "nope"}); code != http.StatusBadRequest {
		t.Fatalf("scope with unknown group: %d", code)
	}
	code, raw = cs.api(t, http.MethodPut, "/api/chats/c-cmd/scope", map[string]string{"kind": "group", "id": "home LAB"})
	v := decodeInto[chatScopeView](t, raw)
	if code != http.StatusOK || v.Scope.ID != g.ID || v.Label != "group Home lab" || !slices.Equal(v.MachineIDs, []string{"w-2"}) {
		t.Fatalf("set group scope by name: %d %s", code, raw)
	}
	code, raw = cs.api(t, http.MethodPut, "/api/chats/c-cmd/scope", map[string]any{"kind": "machines", "ids": []string{"box-3", "w-1"}})
	if v = decodeInto[chatScopeView](t, raw); code != http.StatusOK || v.Label != "2 machines" || !slices.Equal(v.MachineIDs, []string{"w-1", "w-3"}) {
		t.Fatalf("set machines scope: %d %s", code, raw)
	}

	// Deleting the group and removing a label.
	if code, _ := cs.api(t, http.MethodDelete, "/api/machine-groups/"+g.ID, nil); code != http.StatusOK {
		t.Fatalf("delete group: %d", code)
	}
	if code, _ := cs.api(t, http.MethodDelete, "/api/machine-groups/"+g.ID, nil); code != http.StatusNotFound {
		t.Fatalf("delete deleted group: %d", code)
	}
	if code, raw := cs.api(t, http.MethodDelete, "/api/workers/w-1/labels/work", nil); code != http.StatusOK || len(decodeInto[machineLabelsUpdate](t, raw).Labels) != 0 {
		t.Fatalf("remove label: %d %s", code, raw)
	}
}

func TestCommanderScopeLimitsDispatchAndScheduler(t *testing.T) {
	cs, fws := scopeTestFleet(t)
	ctx := context.Background()
	for _, id := range []string{"w-2", "w-3"} {
		if _, err := cs.h.labelMachine(ctx, id, []string{"Work"}, false); err != nil {
			t.Fatal(err)
		}
	}
	mkChat(t, cs.st, "c-1", true)
	if err := cs.st.SetChatScope(ctx, "c-1", store.MachineTarget{Kind: store.TargetLabel, Label: "work"}); err != nil {
		t.Fatal(err)
	}
	cmd := commanderCtx("c-1")

	// Pinned outside the scope (by ID or name): refused, naming the scope.
	for _, sel := range []WorkerSelector{{WorkerID: "w-1"}, {Name: "box-1"}} {
		_, err := cs.svc.DispatchTask(cmd, DispatchRequest{WorkerSelector: sel, Agent: "mock", RepoPath: "/repo", Prompt: "p"})
		if err == nil || !strings.Contains(err.Error(), "box-1 is outside this chat's machines (label work)") {
			t.Fatalf("pinned out of scope %+v: %v", sel, err)
		}
	}

	// Unpinned: placed inside the scope; the log says why box-1 was left out.
	for i := 0; i < 4; i++ {
		res, err := cs.svc.DispatchTask(cmd, DispatchRequest{Agent: "mock", RepoPath: "/repo", Prompt: "p", Seed: string(rune('a' + i))})
		if err != nil {
			t.Fatal(err)
		}
		if res.WorkerID != "w-2" && res.WorkerID != "w-3" {
			t.Fatalf("dispatch %d placed on %q, outside the scope", i, res.WorkerID)
		}
		fws[res.WorkerID].expect(protocol.MsgTaskStart, 3*time.Second)
		if i == 0 {
			logs, _ := cs.st.ListPlacementLog(ctx, store.PlacementLogFilter{Subject: taskSubject(res.TaskID)})
			if len(logs) != 1 || !strings.Contains(logs[0].DetailJSON, "user scope (label work)") || !strings.Contains(logs[0].DetailJSON, "w-1") {
				t.Fatalf("placement log = %+v", logs)
			}
		}
	}

	// The user is not limited by a chat's scope.
	if res, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{WorkerSelector: WorkerSelector{WorkerID: "w-1"}, Agent: "mock", RepoPath: "/repo", Prompt: "p"}); err != nil || res.WorkerID != "w-1" {
		t.Fatalf("user dispatch to w-1: %+v %v", res, err)
	}
	fws["w-1"].expect(protocol.MsgTaskStart, 3*time.Second)

	// A scope that names no machine says so.
	if err := cs.st.SetChatScope(ctx, "c-1", store.MachineTarget{Kind: store.TargetLabel, Label: "GPU"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.svc.DispatchTask(cmd, DispatchRequest{Agent: "mock", RepoPath: "/repo", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "(label GPU) name no machine") {
		t.Fatalf("empty scope: %v", err)
	}

	// Work the chat queued is placed by the scheduler inside the scope only.
	if err := cs.st.SetChatScope(ctx, "c-1", store.MachineTarget{Kind: store.TargetMachine, ID: "w-3"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q-1", "q-2"} {
		if err := cs.st.CreateTask(ctx, store.Task{
			ID: id, SID: "t-" + id, Agent: "mock", Workdir: "/repo", Prompt: "p", Mode: protocol.TaskModeOneshot,
			Status: protocol.TaskStatusQueued, ParentChatID: "c-1", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cs.h.SchedulePending(ctx)
	for _, id := range []string{"q-1", "q-2"} {
		if got := mustTask(t, cs.st, id); got.WorkerID != "w-3" {
			t.Fatalf("scheduler placed %s on %q (status %s), want w-3", id, got.WorkerID, got.Status)
		}
	}
}

func TestCommanderLabelsGroupsAndSeesScopeOverMCP(t *testing.T) {
	cs, _ := scopeTestFleet(t)
	mkChat(t, cs.st, "c-1", true)
	cmd := commanderCtx("c-1")

	if _, err := cs.svc.ManageWork(cmd, ManageWorkInput{Action: "label", Items: []WorkItemInput{{Machine: "box-2", Labels: []string{"Work", "Fast"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.svc.ManageWork(cmd, ManageWorkInput{Action: "unlabel", Items: []WorkItemInput{{Machine: "w-2", Labels: []string{"fast"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.svc.ManageWork(cmd, ManageWorkInput{Action: "group", Items: []WorkItemInput{{Title: "Pair", Machine: "box-1, box-2"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.svc.ManageWork(cmd, ManageWorkInput{Action: "label", Items: []WorkItemInput{{Machine: "nope", Labels: []string{"x"}}}}); err == nil {
		t.Fatal("labeling an unknown machine succeeded")
	}
	if err := cs.st.SetChatScope(context.Background(), "c-1", store.MachineTarget{Kind: store.TargetLabel, Label: "Work"}); err != nil {
		t.Fatal(err)
	}
	res, err := cs.svc.FleetStatusView(cmd, FleetStatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	sum := res.(FleetSummaryResult)
	if !strings.HasPrefix(sum.Scope, "label Work: box-2.") || !slices.Equal(sum.MachineGroups, []string{"Pair: box-1, box-2"}) {
		t.Fatalf("summary scope %q groups %v", sum.Scope, sum.MachineGroups)
	}
	rows, _ := cs.svc.FleetStatusView(cmd, FleetStatusInput{Machine: "box-2"})
	if m := rows.(map[string]any)["machines"].([]MachineDetailRow); len(m) != 1 || !slices.Equal(m[0].Labels, []string{"Work"}) {
		t.Fatalf("machine rows = %+v", m)
	}
	if _, err := cs.svc.ManageWork(cmd, ManageWorkInput{Action: "ungroup", Items: []WorkItemInput{{Title: "pair"}}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := cs.st.ListMachineGroups(context.Background()); len(g) != 0 {
		t.Fatalf("groups after delete: %+v", g)
	}
}
