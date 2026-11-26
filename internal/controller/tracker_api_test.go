package controller

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/BryanSant/syre/internal/store"
)

func TestTrackerREST(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	if err := cs.st.CreateProject(ctx, store.Project{Name: "tracked"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]

	if code, _ := cs.api(t, http.MethodPost, "/api/projects/"+proj.ID+"/trackers", map[string]any{"kind": "jira"}); code != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d", code)
	}
	code, raw := cs.api(t, http.MethodPost, "/api/projects/"+proj.ID+"/trackers", map[string]any{
		"kind":   "ka",
		"config": map[string]any{"store_url": filepath.Join(t.TempDir(), "missing.git"), "project": "syre", "clone_dir": t.TempDir()},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	var trk store.Tracker
	if err := json.Unmarshal(raw, &trk); err != nil || trk.Kind != "ka" || !trk.Enabled {
		t.Fatalf("created tracker %+v (%v)", trk, err)
	}
	code, raw = cs.api(t, http.MethodPost, "/api/trackers/"+trk.ID+"/sync", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("sync against a missing store: %d %s", code, raw)
	}
	if got, _ := cs.st.GetTracker(ctx, trk.ID); got.LastError == "" {
		t.Fatal("the sync error was not recorded on the tracker")
	}
	if code, raw := cs.api(t, http.MethodGet, "/api/projects/"+proj.ID+"/trackers", nil); code != http.StatusOK || len(raw) == 0 {
		t.Fatalf("list: %d %s", code, raw)
	}
	if code, _ := cs.api(t, http.MethodDelete, "/api/trackers/"+trk.ID, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if _, err := cs.st.GetTracker(ctx, trk.ID); err == nil {
		t.Fatal("tracker still exists after delete")
	}
}
