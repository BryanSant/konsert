package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceFlowUnavailable(t *testing.T) {
	ctx := context.Background()
	client := &DeviceFlowClient{}

	if _, err := client.Start(ctx, "github.corp.example", GitHub); !errors.Is(err, ErrDeviceFlowUnavailable) {
		t.Fatalf("GHES without client ID: err = %v, want ErrDeviceFlowUnavailable", err)
	}
	if !client.Available("github.com", GitHub) {
		t.Fatalf("github.com should use the built-in client ID")
	}
	if id, err := client.clientIDFor("github.com", GitHub); err != nil || id != DefaultGitHubClientID {
		t.Fatalf("github.com client ID = %q, %v; want built-in", id, err)
	}
	if client.Available("github.corp.example", GitHub) {
		t.Fatalf("GHES must not use the built-in client ID")
	}
	if _, err := client.Start(ctx, "gitlab.corp.example", GitLab); !errors.Is(err, ErrDeviceFlowUnavailable) {
		t.Fatalf("self-managed gitlab without client ID: err = %v, want ErrDeviceFlowUnavailable", err)
	}
	if !client.Available("gitlab.com", GitLab) {
		t.Fatalf("gitlab.com should use the built-in client ID")
	}
	if id, err := client.clientIDFor("gitlab.com", GitLab); err != nil || id != DefaultGitLabClientID {
		t.Fatalf("gitlab.com client ID = %q, %v; want built-in", id, err)
	}
	if client.Available("gitlab.corp.example", GitLab) {
		t.Fatalf("self-managed gitlab must not use the built-in client ID")
	}
	client.GitHubClientID = "cid"
	if id, err := client.clientIDFor("github.com", GitHub); err != nil || id != "cid" {
		t.Fatalf("github.com override client ID = %q, %v; want %q", id, err, "cid")
	}
	client.GitLabClientID = "gl-override"
	if id, err := client.clientIDFor("gitlab.com", GitLab); err != nil || id != "gl-override" {
		t.Fatalf("gitlab.com override client ID = %q, %v; want %q", id, err, "gl-override")
	}
	if !client.Available("gitlab.corp.example", GitLab) {
		t.Fatalf("self-managed gitlab should be available with explicit client ID")
	}
	if _, err := client.Start(ctx, "codeberg.org", Forgejo); !errors.Is(err, ErrDeviceFlowUnavailable) {
		t.Fatalf("forgejo: err = %v, want ErrDeviceFlowUnavailable", err)
	}
}

func TestDeviceFlowGitHubStartPollSlowDownAndAuthorize(t *testing.T) {
	ctx := context.Background()
	var pollCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "gh-client-123" {
				t.Errorf("unexpected start form: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{
				"device_code": "dc-1",
				"user_code": "WDJB-MJHT",
				"verification_uri": "https://github.com/login/device",
				"expires_in": 900,
				"interval": 5
			}`))
		case "/login/oauth/access_token":
			n := pollCount.Add(1)
			switch n {
			case 1:
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			case 2:
				_, _ = w.Write([]byte(`{"error":"slow_down","interval":10}`))
			case 3:
				_, _ = w.Write([]byte(`{"access_token":"gho_test_token_123"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	host := strings.TrimPrefix(srv.URL, "http://")
	df := &DeviceFlowClient{
		GitHubClientID: "gh-client-123",
		HTTPClient:     srv.Client(),
		Scheme:         "http",
		Now:            func() time.Time { return now },
	}

	start, err := df.Start(ctx, host, GitHub)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.UserCode != "WDJB-MJHT" || start.Interval != 5 || start.DeviceCode != "dc-1" {
		t.Fatalf("unexpected start: %+v", start)
	}

	p1, err := df.Poll(ctx, host, GitHub, start.DeviceCode, start.Interval, start.ExpiresAt)
	if err != nil || p1.Status != DevicePending {
		t.Fatalf("poll 1: %+v err=%v", p1, err)
	}

	p2, err := df.Poll(ctx, host, GitHub, start.DeviceCode, start.Interval, start.ExpiresAt)
	if err != nil || p2.Status != DeviceSlowDown || p2.Interval != 10 {
		t.Fatalf("poll 2: %+v err=%v", p2, err)
	}

	p3, err := df.Poll(ctx, host, GitHub, start.DeviceCode, p2.Interval, start.ExpiresAt)
	if err != nil || p3.Status != DeviceAuthorized || p3.AccessToken != "gho_test_token_123" {
		t.Fatalf("poll 3: %+v err=%v", p3, err)
	}
}

func TestDeviceFlowGitLabExpiredAndDenied(t *testing.T) {
	ctx := context.Background()
	var mode atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize_device":
			_, _ = w.Write([]byte(`{
				"device_code": "gl-dc",
				"user_code": "ABCD-1234",
				"verification_uri": "https://gitlab.example/oauth/device",
				"verification_uri_complete": "https://gitlab.example/oauth/device?user_code=ABCD-1234",
				"expires_in": 60,
				"interval": 5
			}`))
		case "/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			if mode.Load() == 0 {
				_, _ = w.Write([]byte(`{"error":"expired_token"}`))
			} else {
				_, _ = w.Write([]byte(`{"error":"access_denied"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	host := strings.TrimPrefix(srv.URL, "http://")
	df := &DeviceFlowClient{
		GitLabClientID: "gl-client-123",
		HTTPClient:     srv.Client(),
		Scheme:         "http",
		Now:            func() time.Time { return now },
	}

	start, err := df.Start(ctx, host, GitLab)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Server-side expired_token
	pExp, err := df.Poll(ctx, host, GitLab, start.DeviceCode, start.Interval, start.ExpiresAt)
	if err != nil || pExp.Status != DeviceExpired {
		t.Fatalf("expected expired, got %+v err=%v", pExp, err)
	}

	// Server-side access_denied
	mode.Store(1)
	pDen, err := df.Poll(ctx, host, GitLab, start.DeviceCode, start.Interval, start.ExpiresAt)
	if err != nil || pDen.Status != DeviceDenied {
		t.Fatalf("expected denied, got %+v err=%v", pDen, err)
	}

	// Local clock expiration short-circuits without hitting server
	now = now.Add(2 * time.Minute)
	pLocalExp, err := df.Poll(ctx, host, GitLab, start.DeviceCode, start.Interval, start.ExpiresAt)
	if err != nil || pLocalExp.Status != DeviceExpired {
		t.Fatalf("expected local expired, got %+v err=%v", pLocalExp, err)
	}
}
