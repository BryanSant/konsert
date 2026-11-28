package controller

import (
	"net/http"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

func TestNormalizeWorkspaceFolder(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"~":                 "",
		"src":               "src",
		"~/src/":            "src",
		`~\source\repos`:    "source/repos",
		" ~/code//work/./ ": "code/work",
	} {
		if got, err := normalizeWorkspaceFolder(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/home/me/src", `\\server\share`, `C:\src`, "c:src", "~/../etc", "src/../..", "~/~other", "a:b"} {
		if got, err := normalizeWorkspaceFolder(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestWorkspaceFolderSuggestionsSettingAndDispatch(t *testing.T) {
	cs := newChatServer(t)
	info := func(id, name string, folders ...protocol.HomeFolder) protocol.WorkerInfo {
		wi := testWorkerInfo(id, name)
		wi.Facts = &protocol.MachineFacts{CPUCores: 4, HomeFolders: folders}
		return wi
	}
	cs.connectWorker(t, info("w-1", "box-1", protocol.HomeFolder{Path: "src", Repos: 3}, protocol.HomeFolder{Path: "Projects", Repos: 9}), nil, "w-1")
	fw2 := cs.connectWorker(t, info("w-2", "box-2", protocol.HomeFolder{Path: "src", Repos: 1}, protocol.HomeFolder{Path: "projects", Repos: 1}), nil, "w-2")
	cs.connectWorker(t, info("w-3", "box-3", protocol.HomeFolder{Path: "src", Repos: 0}), nil, "w-3")

	// On more machines first, then more repos; case-only differences are one folder.
	waitFor(t, 3*time.Second, "facts stored", func() bool { sg, _ := cs.h.workspaceSuggestions(t.Context()); return len(sg) == 2 })
	_, raw := cs.api(t, http.MethodGet, "/api/workspace-folders", nil)
	res := decodeInto[struct {
		Suggestions []workspaceSuggestion `json:"suggestions"`
		Total       int                   `json:"total"`
		Effective   string                `json:"effective"`
	}](t, raw)
	if res.Total != 3 || len(res.Suggestions) != 2 || res.Suggestions[0].Path != "src" || res.Suggestions[0].Count != 3 || res.Suggestions[1].Count != 2 || res.Suggestions[1].Repos != 10 || res.Effective != "src" {
		t.Fatalf("suggestions = %s", raw)
	}

	// The setting is validated and stored home-relative.
	if code, _ := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"workspace_folder": "/abs"}); code != http.StatusBadRequest {
		t.Fatalf("absolute folder accepted: %d", code)
	}
	if code, raw := cs.api(t, http.MethodPut, "/api/settings", map[string]any{"workspace_folder": `~\Projects\`}); code != http.StatusOK || decodeInto[settingsResponse](t, raw).WorkspaceFolder != "Projects" {
		t.Fatalf("set folder: %d %s", code, raw)
	}

	// A Commander chat across machines starts work there, created where it lands.
	mkChat(t, cs.st, "c-1", true)
	dispatch := func() protocol.TaskStartPayload {
		t.Helper()
		if _, err := cs.svc.ManageWork(commanderCtx("c-1"), ManageWorkInput{Action: "dispatch", Items: []WorkItemInput{{Machine: "box-2", Agent: "mock", Prompt: "p"}}}); err != nil {
			t.Fatal(err)
		}
		env := fw2.expect(protocol.MsgTaskStart, 3*time.Second)
		p, _ := protocol.DecodePayload[protocol.TaskStartPayload](env)
		return p
	}
	if p := dispatch(); p.Workdir != "~/Projects" || !p.CreateWorkdir {
		t.Fatalf("fleet default: workdir %q create %v", p.Workdir, p.CreateWorkdir)
	}
	// The chat's own folder wins; a bad one is refused.
	if code, _ := cs.api(t, http.MethodPut, "/api/chats/c-1/work-folder", map[string]string{"folder": "../x"}); code != http.StatusBadRequest {
		t.Fatalf("bad chat folder accepted: %d", code)
	}
	if code, raw := cs.api(t, http.MethodPut, "/api/chats/c-1/work-folder", map[string]string{"folder": "~/sandbox"}); code != http.StatusOK || decodeInto[chatWorkFolderView](t, raw).Effective != "sandbox" {
		t.Fatalf("chat folder: %d %s", code, raw)
	}
	if p := dispatch(); p.Workdir != "~/sandbox" || !p.CreateWorkdir {
		t.Fatalf("chat folder: workdir %q create %v", p.Workdir, p.CreateWorkdir)
	}
	// A chat limited to one machine starts at home as before.
	if err := cs.st.SetChatScope(t.Context(), "c-1", store.MachineTarget{Kind: store.TargetMachine, ID: "w-2"}); err != nil {
		t.Fatal(err)
	}
	if p := dispatch(); p.Workdir != "~" || p.CreateWorkdir {
		t.Fatalf("single machine: workdir %q create %v", p.Workdir, p.CreateWorkdir)
	}
}
