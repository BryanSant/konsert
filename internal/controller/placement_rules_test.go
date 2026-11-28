package controller

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Rules beat scores but never policy (SYR-880CP): a prefer rule wins over a better
// score, an avoid rule loses to a worse one, a require rule narrows, and a machine the
// policy refuses is never chosen whatever the rules say. Every placement is logged.
func TestPlacementRulesBeatScoresNeverPolicy(t *testing.T) {
	cs := newChatServer(t)
	ctx := t.Context()
	small := testWorkerInfo("w-a-small", "small")
	big := testWorkerInfo("w-z-big", "big")
	cs.connectWorker(t, big, nil, "w-z-big")
	time.Sleep(10 * time.Millisecond)
	cs.connectWorker(t, small, nil, "w-a-small")
	cs.h.mu.Lock()
	cs.h.workerMetrics["w-a-small"] = &protocol.WorkerMetrics{MemTotalMB: 8192, MemUsedMB: 4096}
	cs.h.workerMetrics["w-z-big"] = &protocol.WorkerMetrics{MemTotalMB: 65536, MemUsedMB: 8192}
	cs.h.mu.Unlock()

	dispatch := func(prompt string) DispatchResult {
		t.Helper()
		res, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/src/train", Prompt: prompt, WorkKey: "ml/train"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = cs.api(t, "POST", "/api/tasks/"+res.TaskID+"/cancel", nil)
		return res
	}
	rule := func(body map[string]any) string {
		t.Helper()
		code, raw := cs.api(t, "POST", "/api/placement/rules", body)
		if code != 201 {
			t.Fatalf("create rule: %d %s", code, raw)
		}
		var r store.PlacementRule
		_ = json.Unmarshal(raw, &r)
		return r.ID
	}
	// Known 3 GB work: by score it goes to big, with far more room to spare.
	if err := cs.st.UpsertWorkProfile(ctx, store.WorkProfile{WorkKey: "ml/train", MachineClass: "linux/amd64", Runs: 3, PeakRSSMaxMB: 3000}); err != nil {
		t.Fatal(err)
	}
	if got := dispatch("by score").WorkerID; got != "w-z-big" {
		t.Fatalf("by score: %s", got)
	}
	// Avoid big for this work key: small wins, though big scores better.
	avoid := rule(map[string]any{"scope_kind": "work_key", "scope_value": "ml/*", "effect": "avoid", "target_kind": "machine", "target": "big"})
	if got := dispatch("avoid big").WorkerID; got != "w-a-small" {
		t.Fatalf("avoid: %s", got)
	}
	cs.api(t, "POST", "/api/placement/rules/"+avoid+"/end", nil)
	// Require small for all work.
	req := rule(map[string]any{"scope_kind": "any", "effect": "require", "target_kind": "machine", "target": "small"})
	if got := dispatch("require small").WorkerID; got != "w-a-small" {
		t.Fatalf("require: %s", got)
	}
	// Never policy: small refuses all work (control-only); the require rule then
	// leaves nothing, and the task queues rather than going against the policy.
	pol, _ := json.Marshal(protocol.MachinePolicy{Role: protocol.MachineRoleControlOnly})
	if err := cs.st.SetWorkerPolicy(ctx, "w-a-small", string(pol)); err != nil {
		t.Fatal(err)
	}
	if got := dispatch("require small, but its policy says no").WorkerID; got != "" {
		t.Fatalf("placed on %s against its policy", got)
	}
	cs.api(t, "POST", "/api/placement/rules/"+req+"/end", nil)

	// Every placement was logged, with who decided and why.
	code, raw := cs.api(t, "GET", "/api/placement/log?limit=10", nil)
	var log struct {
		Entries []struct {
			Subject  string          `json:"subject"`
			Decider  string          `json:"decider"`
			WorkerID string          `json:"worker_id"`
			Detail   PlacementDetail `json:"detail"`
		} `json:"entries"`
	}
	if code != 200 || json.Unmarshal(raw, &log) != nil || len(log.Entries) != 4 {
		t.Fatalf("log: %d %s", code, raw)
	}
	newest := log.Entries[0]
	if newest.Decider != "user" || newest.WorkerID != "" || !strings.HasPrefix(newest.Subject, "task:") {
		t.Fatalf("newest entry: %+v", newest)
	}
	avoided := log.Entries[2] // "avoid big"
	if avoided.WorkerID != "w-a-small" || len(avoided.Detail.Rules) != 1 || len(avoided.Detail.Ranked) != 2 || avoided.Detail.Ranked[1].Rule >= 0 {
		t.Fatalf("avoid entry: %+v", avoided)
	}
	required := log.Entries[1]
	if len(required.Detail.Rejected) != 1 || !strings.Contains(required.Detail.Rejected[0].Reason, "placement rule requires") {
		t.Fatalf("require entry: %+v", required.Detail)
	}
}

func TestPlacementRuleScopes(t *testing.T) {
	sc := placementScope{ProjectID: "p1", ProjectSID: "p-abcde", Repo: "api", Branch: "main", WorkKey: "p-abcde/tk-1/api/test"}
	for _, c := range []struct {
		kind, value string
		want        bool
	}{
		{"any", "", true},
		{"project", "P-ABCDE", true}, {"project", "p1", true}, {"project", "p-zzzzz", false},
		{"repo", "API", true}, {"repo", "web", false},
		{"branch", "main", true}, {"branch", "Main", false},
		{"work_key", "p-abcde/tk-1/api/test", true}, {"work_key", "p-abcde/*", true}, {"work_key", "p-abcde/tk-2/*", false},
	} {
		if got := ruleApplies(store.PlacementRule{ScopeKind: c.kind, ScopeValue: c.value}, sc); got != c.want {
			t.Errorf("%s %q: %v", c.kind, c.value, got)
		}
	}
}

// A dry run says where work would go, the same as the real dispatch of the same
// inputs, and creates nothing (SYR-QY2TG).
func TestExplainPlacementMatchesRealPlacement(t *testing.T) {
	cs := newChatServer(t)
	ctx := t.Context()
	cs.connectWorker(t, testWorkerInfo("w-z-big", "big"), nil, "w-z-big")
	time.Sleep(10 * time.Millisecond)
	cs.connectWorker(t, testWorkerInfo("w-a-small", "small"), nil, "w-a-small")
	cs.h.mu.Lock()
	cs.h.workerMetrics["w-a-small"] = &protocol.WorkerMetrics{MemTotalMB: 8192, MemUsedMB: 4096}
	cs.h.workerMetrics["w-z-big"] = &protocol.WorkerMetrics{MemTotalMB: 65536, MemUsedMB: 8192}
	cs.h.mu.Unlock()
	if err := cs.st.UpsertWorkProfile(ctx, store.WorkProfile{WorkKey: "ml/train", MachineClass: "linux/amd64", Runs: 3, PeakRSSMaxMB: 6000}); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"agent": "mock", "repo_path": "/src/train", "prompt": "train", "work_key": "ml/train",
		"requirements": map[string]any{"os": "linux"}}
	code, raw := cs.api(t, "POST", "/api/placement/explain", body)
	var dry DispatchResult
	if code != 200 || json.Unmarshal(raw, &dry) != nil || dry.Placement == nil {
		t.Fatalf("explain: %d %s", code, raw)
	}
	if dry.Placement.WorkerID != "w-z-big" || dry.TaskID != "" || len(dry.Placement.Detail.Ranked) != 2 || !dry.Placement.Detail.Known {
		t.Fatalf("dry run: %+v", dry.Placement)
	}
	if small := dry.Placement.Detail.Ranked[1]; small.ID != "w-a-small" || small.Fits {
		t.Fatalf("small should not fit 6 GB: %+v", small)
	}
	// Nothing was created or remembered.
	if tasks, _ := cs.st.ListTasks(ctx, "", "", 10); len(tasks) != 0 {
		t.Fatalf("a dry run created %d tasks", len(tasks))
	}
	if _, err := cs.st.GetWorkRequirements(ctx, "ml/train"); err == nil {
		t.Fatal("a dry run remembered requirements")
	}

	// The real dispatch of the same inputs lands where the dry run said.
	code, raw = cs.api(t, "POST", "/api/tasks", body)
	var real DispatchResult
	if code >= 300 || json.Unmarshal(raw, &real) != nil {
		t.Fatalf("dispatch: %d %s", code, raw)
	}
	if real.WorkerID != dry.Placement.WorkerID {
		t.Fatalf("real placement %s, dry run said %s", real.WorkerID, dry.Placement.WorkerID)
	}
	// And inspect kind=placement explains it afterwards.
	res, err := cs.svc.Inspect(ctx, InspectRequest{Kind: protocol.InspectKindPlacement, Target: real.TaskID})
	if err != nil || !strings.Contains(res.Text, `"worker_id":"w-z-big"`) || !strings.Contains(res.Text, `"decider":"user"`) {
		t.Fatalf("inspect: %+v %v", res, err)
	}
}
