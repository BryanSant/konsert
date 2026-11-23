package controller

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/coder/websocket"
)

func newSecurityTestServer(t *testing.T, mutate func(*config.ControllerConfig)) (*Server, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.DefaultControllerConfig()
	cfg.NoAuth = true
	if mutate != nil {
		mutate(&cfg)
	}
	srv := NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Hub().Close()
	})
	return srv, ts
}

func doReq(t *testing.T, method, url string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestClientWSRejectsCrossOrigin(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.AllowedOrigins = []string{"http://localhost:4200"}
	})
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/client"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Cross-site page: rejected even though --no-auth lets everything else in.
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		t.Fatal("expected cross-origin /ws/client upgrade to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp)
	}

	// Same port on another loopback name is still a different origin.
	_, resp, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://localhost:3000"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for localhost:3000, got err=%v resp=%+v", err, resp)
	}

	// Same origin (the hub's own host) and a configured extra origin are accepted.
	for _, origin := range []string{ts.URL, "http://localhost:4200"} {
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{origin}},
		})
		if err != nil {
			t.Fatalf("origin %s: expected upgrade to succeed: %v", origin, err)
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}

	// Non-browser clients (no Origin) are unaffected.
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("no-Origin client: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func TestWorkerWSWithoutOriginStillAccepted(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.FleetToken = "fleet"
		c.AdminToken = "admin"
	})
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/worker"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer fleet"}},
	})
	if err != nil {
		t.Fatalf("worker ws with bearer and no Origin must connect: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("worker ws without token must be 401, got err=%v resp=%+v", err, resp)
	}
}

func TestCSRFProtectionOnStateChangingAPI(t *testing.T) {
	_, ts := newSecurityTestServer(t, nil)
	body := `{"name":"Eng"}`

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"text/plain simple request", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"form-urlencoded", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"missing content type", map[string]string{}, http.StatusUnsupportedMediaType},
		{"cross-site origin", map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, http.StatusForbidden},
		{"null origin", map[string]string{"Content-Type": "application/json", "Origin": "null"}, http.StatusForbidden},
		{"sec-fetch-site cross-site", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"sec-fetch-site same-site", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"same-origin json", map[string]string{"Content-Type": "application/json", "Origin": ts.URL, "Sec-Fetch-Site": "same-origin"}, http.StatusCreated},
		{"json with charset", map[string]string{"Content-Type": "application/json; charset=utf-8"}, http.StatusCreated},
		{"custom header", map[string]string{"Content-Type": "text/plain", csrfHeader: "1"}, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doReq(t, http.MethodPost, ts.URL+"/api/workspaces", strings.NewReader(body), tc.headers)
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	// Multipart is only accepted on the attachment upload endpoint.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "note.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.WriteField("session_id", "s1")
	_ = mw.Close()
	resp := doReq(t, http.MethodPost, ts.URL+"/api/workspaces", bytes.NewReader(buf.Bytes()),
		map[string]string{"Content-Type": mw.FormDataContentType()})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart to /api/workspaces: got %d, want 415", resp.StatusCode)
	}
	resp = doReq(t, http.MethodPost, ts.URL+"/api/attachments", bytes.NewReader(buf.Bytes()),
		map[string]string{"Content-Type": mw.FormDataContentType(), "Origin": ts.URL})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("same-origin multipart upload: got %d (%s), want 201", resp.StatusCode, b)
	}
	var uploaded struct {
		RemotePath string `json:"remote_path"`
	}
	if err := json.UnmarshalRead(resp.Body, &uploaded); err == nil && uploaded.RemotePath != "" {
		t.Cleanup(func() { _ = os.Remove(uploaded.RemotePath) })
	}
	// A cross-site multipart form post (a CORS "simple" request) is rejected.
	resp = doReq(t, http.MethodPost, ts.URL+"/api/attachments", bytes.NewReader(buf.Bytes()),
		map[string]string{"Content-Type": mw.FormDataContentType(), "Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site multipart upload: got %d, want 403", resp.StatusCode)
	}

	// Bodyless state changes (e.g. DELETE from the CLI) need no Content-Type,
	// but still may not come from a foreign origin.
	resp = doReq(t, http.MethodDelete, ts.URL+"/api/chat/sessions/missing", nil, nil)
	if resp.StatusCode == http.StatusUnsupportedMediaType || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("bodyless DELETE without Origin should pass the guard, got %d", resp.StatusCode)
	}
	resp = doReq(t, http.MethodDelete, ts.URL+"/api/chat/sessions/missing", nil, map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin DELETE: got %d, want 403", resp.StatusCode)
	}

	// Safe methods are untouched.
	resp = doReq(t, http.MethodGet, ts.URL+"/api/workspaces", nil, map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: got %d, want 200", resp.StatusCode)
	}
}

func TestCSRFAllowsConfiguredOrigins(t *testing.T) {
	_, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.PublicURL = "https://konsert.example.ts.net"
		c.AllowedOrigins = []string{"http://localhost:4200"}
	})
	for _, origin := range []string{"https://konsert.example.ts.net", "http://localhost:4200"} {
		resp := doReq(t, http.MethodPost, ts.URL+"/api/workspaces", strings.NewReader(`{"name":"x"}`),
			map[string]string{"Content-Type": "application/json", "Origin": origin, "Sec-Fetch-Site": "cross-site"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("origin %s: got %d, want 201", origin, resp.StatusCode)
		}
	}
	// Scheme matters for patterns that include one.
	resp := doReq(t, http.MethodPost, ts.URL+"/api/workspaces", strings.NewReader(`{"name":"x"}`),
		map[string]string{"Content-Type": "application/json", "Origin": "https://localhost:4200"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scheme mismatch: got %d, want 403", resp.StatusCode)
	}
}

func TestMCPRejectsCrossOrigin(t *testing.T) {
	_, ts := newSecurityTestServer(t, nil)
	resp := doReq(t, http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`),
		map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin /mcp: got %d, want 403", resp.StatusCode)
	}
}

func TestRequestBodyLimits(t *testing.T) {
	_, ts := newSecurityTestServer(t, nil)
	big := `{"name":"` + strings.Repeat("a", maxJSONBodyBytes+10) + `"}`

	// Known Content-Length: rejected before the handler runs.
	resp := doReq(t, http.MethodPost, ts.URL+"/api/workspaces", strings.NewReader(big),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: got %d, want 413", resp.StatusCode)
	}

	// Chunked (unknown length): MaxBytesReader trips inside the decoder.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/workspaces", io.MultiReader(strings.NewReader(big)))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunked body: got %d, want 413", resp2.StatusCode)
	}

	// The attachment endpoint has its own larger limit.
	att := `{"filename":"a.txt","data_base64":"` + strings.Repeat("A", maxAttachmentBodyBytes) + `"}`
	resp = doReq(t, http.MethodPost, ts.URL+"/api/attachments", strings.NewReader(att),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized attachment: got %d, want 413", resp.StatusCode)
	}
	// ...but a 2 MiB body reaches the handler there (it would exceed the generic JSON
	// limit). The payload is deliberately not valid base64 so nothing is written.
	mid := `{"filename":"a.txt","data_base64":"` + strings.Repeat("!", 2<<20) + `"}`
	resp = doReq(t, http.MethodPost, ts.URL+"/api/attachments", strings.NewReader(mid),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("2 MiB attachment: got %d, want 400 from the handler", resp.StatusCode)
	}
}

func TestLoginThrottle(t *testing.T) {
	srv, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.AdminToken = "right"
		c.FleetToken = "fleet"
	})
	now := time.Unix(1_700_000_000, 0)
	srv.loginLimiter.Now = func() time.Time { return now }

	login := func(tok string) int {
		resp := doReq(t, http.MethodPost, ts.URL+"/api/auth/login", strings.NewReader(`{"token":"`+tok+`"}`),
			map[string]string{"Content-Type": "application/json"})
		return resp.StatusCode
	}
	for i := 0; i < srv.loginLimiter.FreeAttempts+1; i++ {
		if got := login("wrong"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, got)
		}
	}
	// Locked out now, even with the right token.
	if got := login("right"); got != http.StatusTooManyRequests {
		t.Fatalf("locked out: got %d, want 429", got)
	}
	now = now.Add(2 * time.Second)
	if got := login("right"); got != http.StatusOK {
		t.Fatalf("after backoff with right token: got %d, want 200", got)
	}
	// Success resets the counter.
	if got := login("wrong"); got != http.StatusUnauthorized {
		t.Fatalf("after reset: got %d, want 401", got)
	}
}
