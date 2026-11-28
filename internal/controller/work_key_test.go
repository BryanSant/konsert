package controller

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestDispatchSetsWorkKeyAndRemembersRequirements(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	// No machine is online: the tasks queue, keeping their key and requirements.
	res, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/src/demo", Prompt: "run the tests",
		Kind: "Test", Requirements: &WorkRequirements{OS: "linux", MinRAMGB: 16, Tools: map[string]string{"go": "1.26"}, Auth: []string{"gh"}}})
	if err != nil {
		t.Fatal(err)
	}
	task, _ := cs.st.GetTask(ctx, res.TaskID)
	if !strings.HasSuffix(task.WorkKey, "/adhoc/demo/test") || task.WorkKind != "test" {
		t.Fatalf("work key %q kind %q", task.WorkKey, task.WorkKind)
	}
	spec, _ := cs.st.GetTaskStartSpec(ctx, res.TaskID)
	var sel WorkerSelector
	if err := json.Unmarshal([]byte(spec.SelectorJSON), &sel); err != nil || sel.OS != "linux" || sel.MinRAMMB != 16*1024 ||
		sel.RequiredTools["go"] != "1.26" || len(sel.RequiredAuth) != 1 {
		t.Fatalf("requirements on the task: %+v (%v)", sel, err)
	}

	// Later work of the same key declares nothing: it inherits them, and says so.
	res2, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/src/demo", Prompt: "run the tests again", Kind: "test"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = cs.st.GetTaskStartSpec(ctx, res2.TaskID)
	sel = WorkerSelector{}
	_ = json.Unmarshal([]byte(spec.SelectorJSON), &sel)
	if sel.OS != "linux" || sel.MinRAMMB != 16*1024 || !strings.Contains(strings.Join(res2.Warnings, " "), "inherited from work key") {
		t.Fatalf("inherited: %+v %v", sel, res2.Warnings)
	}

	// The dispatcher's own key wins; a selector setting wins over an inherited one.
	res3, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/src/demo", Prompt: "train",
		WorkKey: "ml/resnet", WorkerSelector: WorkerSelector{OS: "darwin"}})
	if err != nil {
		t.Fatal(err)
	}
	if task, _ = cs.st.GetTask(ctx, res3.TaskID); task.WorkKey != "ml/resnet" {
		t.Fatalf("override: %q", task.WorkKey)
	}

	if _, err := cs.svc.DispatchTask(userCtx(), DispatchRequest{Agent: "mock", RepoPath: "/src/demo", Prompt: "x", Kind: "no spaces please"}); err == nil {
		t.Fatal("a kind with spaces was accepted")
	}
}

func TestWorkRequirementsKeepTheSelectorsOwn(t *testing.T) {
	sel := WorkerSelector{OS: "darwin", RequiredTools: map[string]string{"go": "1.25"}}
	WorkRequirements{OS: "linux", Arch: "arm64", Tools: map[string]string{"go": "1.26", "docker": ""}, Virtualization: "bare_metal"}.apply(&sel)
	if sel.OS != "darwin" || sel.Arch != "arm64" || sel.RequiredTools["go"] != "1.25" || sel.RequiredTools["docker"] != "" ||
		len(sel.RequiredTools) != 2 || sel.Virtualization != "bare_metal" {
		t.Fatalf("merged: %+v", sel)
	}
	_ = protocol.TaskStatusQueued
}

// A capability a machine declares with a probe is selectable like any tool (SYR-M7F13).
func TestDeclaredProbeIsSelectable(t *testing.T) {
	facts := func(tools map[string]string) string {
		raw, _ := json.Marshal(protocol.MachineFacts{Tools: tools})
		return string(raw)
	}
	workers := []store.Worker{
		{ID: "plain", Status: "online", MaxConcurrent: 2, Agents: []string{"mock"}, FactsJSON: facts(map[string]string{"git": "2.50"})},
		{ID: "mac", Status: "online", MaxConcurrent: 2, Agents: []string{"mock"}, FactsJSON: facts(map[string]string{"git": "2.50", "xcode": "26.1", "android-sdk": "installed"})},
	}
	var sel WorkerSelector
	WorkRequirements{Tools: map[string]string{"xcode": "26", "android-sdk": ""}}.apply(&sel)
	if w, ok := SelectWorker(workers, nil, sel, "mock", ""); !ok || w.ID != "mac" {
		t.Fatalf("picked %+v %v", w, ok)
	}
	sel = WorkerSelector{}
	WorkRequirements{Tools: map[string]string{"xcode": "27"}}.apply(&sel)
	if w, ok := SelectWorker(workers, nil, sel, "mock", ""); ok {
		t.Fatalf("xcode 27 placed on %s", w.ID)
	}
}
