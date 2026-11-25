package controller

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

// API paths, query parameters and bodies accept an entity's id or its sid, and responses
// carry both.
func TestAPIAcceptsIDOrSID(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "refs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer srv.Hub().Close()

	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	wid := ids.UUID()
	if err := st.UpsertWorker(ctx, store.Worker{ID: wid, Name: "box", HostGroup: "box", OS: "linux", Arch: "amd64", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	w, _ := st.GetWorker(ctx, wid)
	if err := st.CreateTask(ctx, store.Task{WorkerID: wid, Agent: "mock", Workdir: "/r", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := st.ListTasks(ctx, "", "", 1)
	task := tasks[0]

	// GET /api/tasks/{id} and /api/tasks/{sid} return the same task with both identifiers.
	for _, ref := range []string{task.ID, task.SID} {
		code, body := call(http.MethodGet, "/api/tasks/"+ref, "")
		if code != http.StatusOK || body["task_id"] != task.ID || body["task_sid"] != task.SID {
			t.Fatalf("GET /api/tasks/%s = %d %v", ref, code, body)
		}
	}
	if code, _ := call(http.MethodGet, "/api/tasks/t-zzzzz", ""); code != http.StatusNotFound {
		t.Fatalf("GET unknown task sid = %d, want 404", code)
	}

	// ?worker_id= accepts the worker's sid; list items carry both task and worker sids.
	code, body := call(http.MethodGet, "/api/tasks?worker_id="+w.SID, "")
	items, _ := body["tasks"].([]any)
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("GET /api/tasks?worker_id=%s = %d %v", w.SID, code, body)
	}
	item := items[0].(map[string]any)
	if item["task_id"] != task.ID || item["task_sid"] != task.SID || item["worker_id"] != wid || item["worker_sid"] != w.SID {
		t.Fatalf("task list item = %v", item)
	}

	// Chats: POST returns both; GET by id and by sid.
	code, chat := call(http.MethodPost, "/api/chats", `{"backend":"api","title":"x"}`)
	chatID, _ := chat["id"].(string)
	chatSID, _ := chat["sid"].(string)
	if code != http.StatusCreated || !ids.IsUUIDv7(chatID) || !ids.Valid(ids.Chat, chatSID) {
		t.Fatalf("POST /api/chats = %d %v", code, chat)
	}
	for _, ref := range []string{chatID, chatSID} {
		code, body := call(http.MethodGet, "/api/chats/"+ref, "")
		if code != http.StatusOK || body["id"] != chatID || body["sid"] != chatSID {
			t.Fatalf("GET /api/chats/%s = %d %v", ref, code, body)
		}
	}

	// Bodies: a project created under a workspace named by its sid.
	code, ws := call(http.MethodPost, "/api/workspaces", `{"name":"eng"}`)
	wsID, _ := ws["id"].(string)
	wsSID, _ := ws["sid"].(string)
	if code != http.StatusCreated || !ids.IsUUIDv7(wsID) || !ids.Valid(ids.Workspace, wsSID) {
		t.Fatalf("POST /api/workspaces = %d %v", code, ws)
	}
	code, proj := call(http.MethodPost, "/api/projects", `{"name":"app","workspace_id":"`+wsSID+`"}`)
	if code != http.StatusCreated || proj["workspace_id"] != wsID {
		t.Fatalf("POST /api/projects with workspace sid = %d %v", code, proj)
	}

	// DELETE by sid.
	if code, _ := call(http.MethodDelete, "/api/projects/"+proj["sid"].(string), ""); code != http.StatusOK {
		t.Fatalf("DELETE project by sid = %d", code)
	}
	if code, _ := call(http.MethodDelete, "/api/workspaces/"+wsSID, ""); code != http.StatusOK {
		t.Fatalf("DELETE workspace by sid = %d", code)
	}
}
