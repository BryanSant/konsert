package controller

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/forge"
	"github.com/BryanSant/syre/internal/secrets"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/store/sqlite"
)

type noopCredRunner struct{}

func (noopCredRunner) Run(context.Context, string, []string, []string, string) (string, error) {
	return "", context.Canceled
}

func newForgeTestServer(t *testing.T, cfgMod func(*config.ControllerConfig)) (*Server, func()) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = ":memory:"
	cfg.NoAuth = true
	if cfgMod != nil {
		cfgMod(&cfg)
	}
	srv := NewServer(cfg, st, nil, "test")
	// Isolate credential discovery from host git/gh/glab binaries.
	srv.forges.resolver = forge.NewResolver(forge.ResolverOptions{
		Detector: &forge.Detector{Overrides: map[string]forge.Kind{}},
		Creds: &forge.CredDiscovery{
			Runner:         noopCredRunner{},
			Store:          srv.forges.tokenStore,
			GlabConfigPath: "/nonexistent/glab/config.yml",
		},
	})
	return srv, func() { _ = st.Close() }
}

func TestForgeTokenEndpointsEncryptedAndAudited(t *testing.T) {
	srv, cleanup := newForgeTestServer(t, nil)
	defer cleanup()

	secretTok := "fj_super_secret_token_999"

	// PUT /api/forges/codeberg.org/token
	req := httptest.NewRequest(http.MethodPut, "/api/forges/codeberg.org/token",
		bytes.NewBufferString(`{"token":"`+secretTok+`","kind":"forgejo"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretTok) {
		t.Fatalf("PUT response leaked token")
	}

	// Verify encrypted in SQLite settings.
	saved, err := srv.store.GetAllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw := saved["forge_token:codeberg.org"]
	if !secrets.IsSealed(raw) || strings.Contains(raw, secretTok) {
		t.Fatalf("token not sealed in settings: %q", raw)
	}

	// GET /api/forges lists host and source, never the token.
	req = httptest.NewRequest(http.MethodGet, "/api/forges", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/forges status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secretTok) {
		t.Fatalf("GET /api/forges leaked token")
	}
	var listResp struct {
		Forges []forgeHostResponse `json:"forges"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Forges) != 1 || listResp.Forges[0].Host != "codeberg.org" || listResp.Forges[0].CredentialSource != "pasted" {
		t.Fatalf("unexpected /api/forges: %+v", listResp.Forges)
	}

	// DELETE /api/forges/codeberg.org/token
	req = httptest.NewRequest(http.MethodDelete, "/api/forges/codeberg.org/token", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE token status = %d", rec.Code)
	}

	// Verify audit events recorded without ever containing the secret token.
	events, err := srv.store.ListAudit(context.Background(), store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var sawSet, sawDel bool
	for _, e := range events {
		if strings.Contains(e.Message, secretTok) || strings.Contains(e.MetaJSON, secretTok) {
			t.Fatalf("audit event leaked token: %+v", e)
		}
		if e.Action == AuditForgeTokenSet {
			sawSet = true
		}
		if e.Action == AuditForgeTokenDelete {
			sawDel = true
		}
	}
	if !sawSet || !sawDel {
		t.Fatalf("missing audit events: set=%v del=%v", sawSet, sawDel)
	}
}

func TestForgeDeviceFlowEndpoints(t *testing.T) {
	var polls atomic.Int32
	fakeOAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = w.Write([]byte(`{
				"device_code":"dc-secret",
				"user_code":"TEST-CODE",
				"verification_uri":"https://github.com/login/device",
				"expires_in":600,
				"interval":5
			}`))
		case "/login/oauth/access_token":
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gho_device_secret_777"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeOAuth.Close()

	// GHES and self-managed GitLab hosts without a configured client ID -> 501 Not
	// Implemented (built-in client IDs only apply to github.com and gitlab.com).
	srvNoID, cleanupNoID := newForgeTestServer(t, nil)
	defer cleanupNoID()
	for _, tc := range []struct {
		host string
		kind string
	}{
		{"github.corp.example", "github"},
		{"gitlab.corp.example", "gitlab"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/forges/"+tc.host+"/device",
			bytes.NewBufferString(`{"kind":"`+tc.kind+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srvNoID.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: expected 501 without client ID, got %d: %s", tc.host, rec.Code, rec.Body.String())
		}
	}
	if !srvNoID.forges.device.Available("gitlab.com", forge.GitLab) {
		t.Fatalf("gitlab.com should be device-flow available with no config")
	}
	if srvNoID.forges.device.Available("gitlab.corp.example", forge.GitLab) {
		t.Fatalf("self-managed gitlab host should not be device-flow available without config")
	}

	// With client ID -> start + poll pending + poll authorized.
	oauthHost := strings.TrimPrefix(fakeOAuth.URL, "http://")
	srv, cleanup := newForgeTestServer(t, func(cfg *config.ControllerConfig) {
		cfg.Forges.GitHubClientID = "cid-gh"
	})
	defer cleanup()
	srv.forges.device.HTTPClient = fakeOAuth.Client()
	srv.forges.device.Scheme = "http"

	startReq := httptest.NewRequest(http.MethodPost, "/api/forges/"+oauthHost+"/device",
		bytes.NewBufferString(`{"kind":"github"}`))
	startReq.Header.Set("Content-Type", "application/json")
	startRec := httptest.NewRecorder()
	srv.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start device status = %d, body = %s", startRec.Code, startRec.Body.String())
	}
	if strings.Contains(startRec.Body.String(), "dc-secret") {
		t.Fatalf("start response leaked device_code")
	}
	var startResp struct {
		ID       string `json:"id"`
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &startResp); err != nil {
		t.Fatal(err)
	}
	if startResp.ID == "" || startResp.UserCode != "TEST-CODE" {
		t.Fatalf("unexpected start response: %+v", startResp)
	}

	// First poll -> pending
	p1Rec := httptest.NewRecorder()
	srv.ServeHTTP(p1Rec, httptest.NewRequest(http.MethodGet, "/api/forges/"+oauthHost+"/device/"+startResp.ID, nil))
	if p1Rec.Code != http.StatusOK || !strings.Contains(p1Rec.Body.String(), `"pending"`) {
		t.Fatalf("poll 1 = %d %s", p1Rec.Code, p1Rec.Body.String())
	}

	// Second poll -> authorized, token stored encrypted, never returned
	p2Rec := httptest.NewRecorder()
	srv.ServeHTTP(p2Rec, httptest.NewRequest(http.MethodGet, "/api/forges/"+oauthHost+"/device/"+startResp.ID, nil))
	if p2Rec.Code != http.StatusOK || !strings.Contains(p2Rec.Body.String(), `"authorized"`) {
		t.Fatalf("poll 2 = %d %s", p2Rec.Code, p2Rec.Body.String())
	}
	if strings.Contains(p2Rec.Body.String(), "gho_device_secret_777") {
		t.Fatalf("poll 2 leaked access token")
	}

	// Stored token is present and decryptable with source="device".
	tok, ok, err := srv.forges.tokenStore.GetToken(context.Background(), oauthHost)
	if err != nil || !ok || tok.Token != "gho_device_secret_777" || tok.Source != "device" {
		t.Fatalf("stored device token: %+v ok=%v err=%v", tok, ok, err)
	}
}

func TestForgeDetectEndpoint(t *testing.T) {
	fakeForgejo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"9.0.1"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeForgejo.Close()

	fakeUnknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer fakeUnknown.Close()

	srv, cleanup := newForgeTestServer(t, nil)
	defer cleanup()

	// Well-known host via host field
	req := httptest.NewRequest(http.MethodPost, "/api/forges/detect", bytes.NewBufferString(`{"host":"github.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detect github.com = %d %s", rec.Code, rec.Body.String())
	}
	var ghResp struct {
		Host    string     `json:"host"`
		Kind    forge.Kind `json:"kind"`
		APIBase string     `json:"api_base"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ghResp); err != nil {
		t.Fatal(err)
	}
	if ghResp.Host != "github.com" || ghResp.Kind != forge.GitHub || ghResp.APIBase != "https://api.github.com" {
		t.Fatalf("unexpected github detect response: %+v", ghResp)
	}

	// SCP-style remote URL via remote_url field
	req = httptest.NewRequest(http.MethodPost, "/api/forges/detect", bytes.NewBufferString(`{"remote_url":"git@gitlab.com:acme/platform/api.git"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detect gitlab scp = %d %s", rec.Code, rec.Body.String())
	}
	var glResp struct {
		Host    string     `json:"host"`
		Kind    forge.Kind `json:"kind"`
		APIBase string     `json:"api_base"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &glResp); err != nil {
		t.Fatal(err)
	}
	if glResp.Host != "gitlab.com" || glResp.Kind != forge.GitLab || glResp.APIBase != "https://gitlab.com/api/v4" {
		t.Fatalf("unexpected gitlab detect response: %+v", glResp)
	}

	// Fake HTTP Forgejo server (explicit http:// origin URL) -> detected and recorded in GET /api/forges
	req = httptest.NewRequest(http.MethodPost, "/api/forges/detect", bytes.NewBufferString(`{"remote_url":"`+fakeForgejo.URL+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detect fake forgejo = %d %s", rec.Code, rec.Body.String())
	}
	var fjResp struct {
		Host    string     `json:"host"`
		Kind    forge.Kind `json:"kind"`
		APIBase string     `json:"api_base"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fjResp); err != nil {
		t.Fatal(err)
	}
	wantHost := strings.TrimPrefix(fakeForgejo.URL, "http://")
	if fjResp.Host != wantHost || fjResp.Kind != forge.Forgejo || fjResp.APIBase != fakeForgejo.URL+"/api/v1" {
		t.Fatalf("unexpected forgejo detect response: %+v", fjResp)
	}

	// Host appears in GET /api/forges with credential_source="none"
	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/api/forges", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/forges = %d", listRec.Code)
	}
	var listResp struct {
		Forges []forgeHostResponse `json:"forges"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	var sawFake bool
	for _, f := range listResp.Forges {
		if f.Host == wantHost && f.Kind == forge.Forgejo && f.CredentialSource == "none" {
			sawFake = true
		}
	}
	if !sawFake {
		t.Fatalf("expected %s in GET /api/forges, got %+v", wantHost, listResp.Forges)
	}

	// Unknown host -> 422
	req = httptest.NewRequest(http.MethodPost, "/api/forges/detect", bytes.NewBufferString(`{"remote_url":"`+fakeUnknown.URL+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for unknown forge, got %d: %s", rec.Code, rec.Body.String())
	}

	// Empty input -> 400
	req = httptest.NewRequest(http.MethodPost, "/api/forges/detect", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty detect input, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestForgeEndpointsRequireAuth(t *testing.T) {
	srv, cleanup := newForgeTestServer(t, func(cfg *config.ControllerConfig) {
		cfg.NoAuth = false
		cfg.RecoveryKey = "test-recovery-key"
	})
	defer cleanup()

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/forges", ""},
		{http.MethodPost, "/api/forges/detect", `{"host":"github.com"}`},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 unauthenticated, got %d", tc.method, tc.path, rec.Code)
		}
	}
}
