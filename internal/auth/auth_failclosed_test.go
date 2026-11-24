package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmptyTokensFailClosed(t *testing.T) {
	m := NewManager(Options{})

	ws := httptest.NewRequest(http.MethodGet, "/ws/worker", nil)
	if m.CheckFleetAuth(ws) {
		t.Fatal("empty fleet token must deny workers without --no-auth")
	}
	ws.Header.Set("Authorization", "Bearer ")
	if m.CheckFleetAuth(ws) {
		t.Fatal("empty bearer must not match an empty fleet token")
	}

	mcpReq := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, ok, _ := m.MCPScope(mcpReq); ok {
		t.Fatal("no credentials must deny MCP without --no-auth")
	}
	mcpReq.Header.Set("Authorization", "Bearer anything")
	if _, ok, _ := m.MCPScope(mcpReq); ok {
		t.Fatal("arbitrary bearer must not pass with no credentials configured")
	}

	if m.CheckRecoveryKey("") || m.CheckRecoveryKey("guess") {
		t.Fatal("an unset recovery key must reject every candidate")
	}
	if !m.AuthRequired() {
		t.Fatal("auth must be required unless --no-auth is set")
	}
	if _, ok := m.AuthenticateBrowser(httptest.NewRequest(http.MethodGet, "/api/fleet", nil)); ok {
		t.Fatal("browser request must be rejected with no credential")
	}
}

func TestNoAuthAllowsEverything(t *testing.T) {
	m := NewManager(Options{NoAuth: true})
	r := httptest.NewRequest(http.MethodGet, "/ws/worker", nil)
	if !m.CheckFleetAuth(r) || !m.CheckRecoveryKey("") {
		t.Fatal("--no-auth must allow")
	}
	if _, ok, admin := m.MCPScope(r); !ok || !admin {
		t.Fatal("--no-auth MCP callers hold admin scope")
	}
	if m.AuthRequired() {
		t.Fatal("--no-auth must not require auth")
	}
	if _, ok := m.AuthenticateBrowser(r); !ok {
		t.Fatal("--no-auth browser must pass")
	}
}

func TestTailscaleHeaderOnlyFromLoopback(t *testing.T) {
	m := NewManager(Options{RecoveryKey: "rk", TrustTailscaleHeaders: true})
	remote := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	remote.RemoteAddr = "203.0.113.9:5555"
	remote.Header.Set("Tailscale-User-Login", "alice@example.com")
	if _, ok := m.AuthenticateBrowser(remote); ok {
		t.Fatal("Tailscale header from a non-loopback peer must be ignored")
	}
	for _, addr := range []string{"127.0.0.1:5555", "[::1]:5555"} {
		local := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
		local.RemoteAddr = addr
		local.Header.Set("Tailscale-User-Login", "alice@example.com")
		user, ok := m.AuthenticateBrowser(local)
		if !ok || user != "alice@example.com" {
			t.Fatalf("loopback %s: got %q %v", addr, user, ok)
		}
	}
	m2 := NewManager(Options{RecoveryKey: "rk"})
	local := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	local.RemoteAddr = "127.0.0.1:5555"
	local.Header.Set("Tailscale-User-Login", "alice@example.com")
	if _, ok := m2.AuthenticateBrowser(local); ok {
		t.Fatal("Tailscale header must be ignored without --trust-tailscale-headers")
	}
}
