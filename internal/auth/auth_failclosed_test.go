package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmptyTokensFailClosed(t *testing.T) {
	m := NewManager(Options{})

	mcpReq := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if m.CheckMCPAuth(mcpReq) {
		t.Fatal("empty mcp/admin token must deny MCP without --no-auth")
	}
	mcpReq.Header.Set("Authorization", "Bearer anything")
	if m.CheckMCPAuth(mcpReq) {
		t.Fatal("arbitrary bearer must not pass with empty tokens")
	}

	if m.CheckAdminToken("") || m.CheckAdminToken("guess") {
		t.Fatal("empty admin token must reject every login")
	}
	if !m.AuthRequired() {
		t.Fatal("auth must be required unless --no-auth is set")
	}
	if _, ok := m.AuthenticateBrowser(httptest.NewRequest(http.MethodGet, "/api/fleet", nil)); ok {
		t.Fatal("browser request must be rejected with no admin token")
	}
}

func TestNoAuthAllowsEverything(t *testing.T) {
	m := NewManager(Options{NoAuth: true})
	r := httptest.NewRequest(http.MethodGet, "/ws/worker", nil)
	if !m.CheckMCPAuth(r) || !m.CheckAdminToken("") {
		t.Fatal("--no-auth must allow")
	}
	if ok, admin := m.MCPScope(r); !ok || !admin {
		t.Fatal("--no-auth MCP callers hold admin scope")
	}
	if m.AuthRequired() {
		t.Fatal("--no-auth must not require auth")
	}
	if _, ok := m.AuthenticateBrowser(r); !ok {
		t.Fatal("--no-auth browser must pass")
	}
}

func TestMCPScope(t *testing.T) {
	m := NewManager(Options{AdminToken: "admin", MCPToken: "mcp"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer mcp")
	if ok, admin := m.MCPScope(r); !ok || admin {
		t.Fatalf("mcp token: ok=%v admin=%v, want ok and not admin", ok, admin)
	}
	r.Header.Set("Authorization", "Bearer admin")
	if ok, admin := m.MCPScope(r); !ok || !admin {
		t.Fatalf("admin token: ok=%v admin=%v, want ok and admin", ok, admin)
	}
	r.Header.Set("Authorization", "Bearer nope")
	if ok, _ := m.MCPScope(r); ok {
		t.Fatal("wrong token must be rejected")
	}

	// The MCP token defaults to the admin token, which carries admin scope.
	m2 := NewManager(Options{AdminToken: "admin"})
	r.Header.Set("Authorization", "Bearer admin")
	if ok, admin := m2.MCPScope(r); !ok || !admin {
		t.Fatal("defaulted mcp token equals admin token and holds admin scope")
	}
}

func TestTailscaleHeaderOnlyFromLoopback(t *testing.T) {
	m := NewManager(Options{AdminToken: "admin", TrustTailscaleHeaders: true})

	remote := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	remote.RemoteAddr = "192.0.2.10:5555"
	remote.Header.Set("Tailscale-User-Login", "mallory@example.com")
	if _, ok := m.AuthenticateBrowser(remote); ok {
		t.Fatal("tailscale header from a non-loopback peer must be ignored")
	}

	for _, addr := range []string{"127.0.0.1:40000", "[::1]:40000"} {
		local := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
		local.RemoteAddr = addr
		local.Header.Set("Tailscale-User-Login", "alice@example.com")
		user, ok := m.AuthenticateBrowser(local)
		if !ok || user != "alice@example.com" {
			t.Fatalf("%s: expected tailscale user accepted, got %q ok=%v", addr, user, ok)
		}
	}

	// Without the flag the header is never trusted, even from loopback.
	m2 := NewManager(Options{AdminToken: "admin"})
	local := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	local.RemoteAddr = "127.0.0.1:1"
	local.Header.Set("Tailscale-User-Login", "alice@example.com")
	if _, ok := m2.AuthenticateBrowser(local); ok {
		t.Fatal("header must be ignored without --trust-tailscale-headers")
	}
}
