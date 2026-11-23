package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

// TestRemoteClientPassesCSRFGuard checks that state-changing CLI calls get past
// the hub's guard (no 403/415) and that errors surface as *APIError.
func TestRemoteClientPassesCSRFGuard(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultControllerConfig()
	cfg.AdminToken = "admin-tok"
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := NewRemoteFleetClient(ts.URL, "admin-tok")
	for name, call := range map[string]func() error{
		"cancel": func() error { _, err := client.CancelTask(ctx, "missing-task"); return err },
		"steer":  func() error { _, err := client.SteerTask(ctx, "missing-task", "hi", false); return err },
	} {
		err := call()
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: err = %v, want *APIError", name, err)
		}
		if apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "task not found" {
			t.Errorf("%s: got HTTP %d %q, want 404 task not found", name, apiErr.StatusCode, apiErr.Message)
		}
	}

	bad := NewRemoteFleetClient(ts.URL, "wrong")
	_, err = bad.CancelTask(ctx, "missing-task")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: err = %v, want HTTP 401", err)
	}
}
