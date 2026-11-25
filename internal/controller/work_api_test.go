package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

func TestWorkModelRESTEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "work-rest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var r *http.Request
		if body != "" {
			r, _ = http.NewRequest(method, ts.URL+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r, _ = http.NewRequest(method, ts.URL+path, nil)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.UnmarshalRead(resp.Body, &out)
		return resp.StatusCode, out
	}

	// Create & update project with verify_cmd and autonomy defaults.
	code, proj := req(http.MethodPost, "/api/projects", `{"name":"Billing","verify_cmd":"go test ./...","autonomy":{"retry_limit":2,"cross_review":true}}`)
	if code != http.StatusCreated || proj["verify_cmd"] != "go test ./..." {
		t.Fatalf("POST /api/projects = %d %v", code, proj)
	}
	projID := proj["id"].(string)

	// Register repo under /api/projects/{id}/repos.
	code, repo := req(http.MethodPost, "/api/projects/"+projID+"/repos", `{"name":"api","remote_url":"https://github.com/acme/api.git","default_branch":"main","verify_cmd":"go test ./pkg/..."}`)
	if code != http.StatusCreated || repo["remote_url"] != "https://github.com/acme/api.git" {
		t.Fatalf("POST repo = %d %v", code, repo)
	}
	repoID := repo["id"].(string)

	// Duplicate remote_url returns 409 Conflict.
	code, _ = req(http.MethodPost, "/api/projects/"+projID+"/repos", `{"name":"api2","remote_url":"https://github.com/acme/api.git"}`)
	if code != http.StatusConflict {
		t.Fatalf("duplicate repo = %d, want 409", code)
	}

	// Create initiative -> epic -> two features.
	code, initT := req(http.MethodPost, "/api/tickets", `{"project_id":"`+projID+`","kind":"initiative","title":"Autopilot"}`)
	if code != http.StatusCreated {
		t.Fatalf("create initiative = %d %v", code, initT)
	}
	initID := initT["id"].(string)

	code, epicT := req(http.MethodPost, "/api/tickets", `{"project_id":"`+projID+`","parent_id":"`+initID+`","kind":"epic","title":"Work model"}`)
	if code != http.StatusCreated {
		t.Fatalf("create epic = %d %v", code, epicT)
	}
	epicID := epicT["id"].(string)

	code, f1 := req(http.MethodPost, "/api/tickets", `{"project_id":"`+projID+`","parent_id":"`+epicID+`","kind":"feature","title":"Schema","status":"done"}`)
	if code != http.StatusCreated {
		t.Fatalf("create f1 = %d %v", code, f1)
	}
	f1ID := f1["id"].(string)

	code, f2 := req(http.MethodPost, "/api/tickets", `{"project_id":"`+projID+`","parent_id":"`+epicID+`","kind":"feature","title":"Store","status":"ready"}`)
	if code != http.StatusCreated {
		t.Fatalf("create f2 = %d %v", code, f2)
	}
	f2ID := f2["id"].(string)

	// Invalid hierarchy returns 400.
	code, _ = req(http.MethodPost, "/api/tickets", `{"project_id":"`+projID+`","parent_id":"`+epicID+`","kind":"initiative","title":"Bad"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid hierarchy = %d, want 400", code)
	}

	// Edges + cycle rejection (409).
	code, _ = req(http.MethodPost, "/api/tickets/"+f2ID+"/edges", `{"blocked_by_id":"`+f1ID+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("add edge = %d", code)
	}
	code, _ = req(http.MethodPost, "/api/tickets/"+f1ID+"/edges", `{"blocked_by_id":"`+f2ID+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("cycle edge = %d, want 409", code)
	}

	// Repo bindings & external refs.
	code, _ = req(http.MethodPut, "/api/tickets/"+f2ID+"/repos/"+repoID, `{"base_branch":"main","target_branch":"feat/store"}`)
	if code != http.StatusOK {
		t.Fatalf("put repo binding = %d", code)
	}
	code, _ = req(http.MethodPost, "/api/tickets/"+f2ID+"/refs", `{"tracker":"ka","external_id":"SYR-Y03N8","version":"1"}`)
	if code != http.StatusOK {
		t.Fatalf("put external ref = %d", code)
	}

	// GET /api/tickets/{id} for initiative includes computed rollup.
	code, gotInit := req(http.MethodGet, "/api/tickets/"+initID, "")
	if code != http.StatusOK {
		t.Fatalf("GET initiative = %d %v", code, gotInit)
	}
	rollup, ok := gotInit["rollup"].(map[string]any)
	if !ok || int(rollup["total"].(float64)) != 2 || int(rollup["done"].(float64)) != 1 {
		t.Fatalf("unexpected initiative rollup: %+v", gotInit["rollup"])
	}

	// List tickets filtered by project/parent/status.
	code, listOut := req(http.MethodGet, "/api/tickets?project_id="+projID+"&parent_id="+epicID+"&status=ready", "")
	if code != http.StatusOK {
		t.Fatalf("list tickets = %d %v", code, listOut)
	}
	tickets, _ := listOut["tickets"].([]any)
	if len(tickets) != 1 {
		t.Fatalf("expected 1 filtered ticket, got %d", len(tickets))
	}

	// PATCH ticket status.
	code, patched := req(http.MethodPatch, "/api/tickets/"+f2ID, `{"status":"running"}`)
	if code != http.StatusOK || patched["status"] != store.TicketStatusRunning {
		t.Fatalf("PATCH ticket = %d %v", code, patched)
	}
}
