package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

func TestParseHubURL(t *testing.T) {
	cases := []struct{ in, base, ws string }{
		{"https://desk.example.ts.net", "https://desk.example.ts.net", "wss://desk.example.ts.net/ws/worker"},
		{"http://desk:8420/", "http://desk:8420", "ws://desk:8420/ws/worker"},
		{"desk:8420", "http://desk:8420", "ws://desk:8420/ws/worker"},
		{"https://h.example/konsert", "https://h.example/konsert", "wss://h.example/konsert/ws/worker"},
	}
	for _, c := range cases {
		base, ws, err := parseHubURL(c.in)
		if err != nil || base != c.base || ws != c.ws {
			t.Errorf("parseHubURL(%q) = %q %q %v", c.in, base, ws, err)
		}
	}
	for _, bad := range []string{"", "ftp://h", "https://user:pw@h", "https://h/?x=1"} {
		if _, _, err := parseHubURL(bad); err == nil {
			t.Errorf("parseHubURL(%q) accepted", bad)
		}
	}
}

func decodeJSONResp(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.UnmarshalRead(resp.Body, v); err != nil {
		t.Fatal(err)
	}
}

// requestJoin against a real hub: the code buys a credential once.
func TestRequestJoinAgainstHub(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultControllerConfig()
	cfg.AdminToken = "admin-tok"
	srv := controller.NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	defer srv.Hub().Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/machines/join-codes", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create code: %v %v", resp, err)
	}
	var jc controller.JoinCodeResponse
	decodeJSONResp(t, resp, &jc)

	const id = "0196a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a77"
	res, err := requestJoin(ctx, ts.URL, joinRequest{Code: jc.Code, WorkerID: id, Name: "laptop"})
	if err != nil || !strings.HasPrefix(res.Credential, "kmc_") || res.WorkerID != id || res.Name != "laptop" {
		t.Fatalf("join = %+v %v", res, err)
	}
	if _, err := requestJoin(ctx, ts.URL, joinRequest{Code: jc.Code, WorkerID: id}); err == nil ||
		!strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("second join with an active credential: %v", err)
	}
}
