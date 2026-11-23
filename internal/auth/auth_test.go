package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFleetAndMCPAuth(t *testing.T) {
	m := NewManager(Options{
		FleetToken: "fleet-secret",
		AdminToken: "admin-secret",
		MCPToken:   "mcp-secret",
	})

	req := httptest.NewRequest(http.MethodGet, "/ws/worker", nil)
	if m.CheckFleetAuth(req) {
		t.Fatal("expected missing fleet token rejected")
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if m.CheckFleetAuth(req) {
		t.Fatal("expected wrong fleet token rejected")
	}
	req.Header.Set("Authorization", "Bearer fleet-secret")
	if !m.CheckFleetAuth(req) {
		t.Fatal("expected valid fleet token accepted")
	}

	mcpReq := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	mcpReq.Header.Set("Authorization", "Bearer mcp-secret")
	if !m.CheckMCPAuth(mcpReq) {
		t.Fatal("expected valid mcp token accepted")
	}
}

func TestBrowserCookieAndTailscaleAuth(t *testing.T) {
	m := NewManager(Options{
		AdminToken:            "admin-secret",
		TrustTailscaleHeaders: true,
	})

	// Unauthenticated
	req := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	if _, ok := m.AuthenticateBrowser(req); ok {
		t.Fatal("expected unauthenticated request rejected")
	}

	// Login cookie flow
	rec := httptest.NewRecorder()
	m.SetSessionCookie(rec, req, "operator")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly session cookie, got %+v", cookies)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	req2.AddCookie(cookies[0])
	user, ok := m.AuthenticateBrowser(req2)
	if !ok || user != "operator" {
		t.Fatalf("expected authenticated operator, got user=%q ok=%v", user, ok)
	}

	// Tampered cookie rejected
	badCookie := *cookies[0]
	badCookie.Value += "tampered"
	req3 := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	req3.AddCookie(&badCookie)
	if _, ok := m.AuthenticateBrowser(req3); ok {
		t.Fatal("expected tampered cookie rejected")
	}

	// Tailscale identity header
	req4 := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	req4.RemoteAddr = "127.0.0.1:41000" // Tailscale Serve proxies from loopback
	req4.Header.Set("Tailscale-User-Login", "alice@example.com")
	user, ok = m.AuthenticateBrowser(req4)
	if !ok || user != "alice@example.com" {
		t.Fatalf("expected tailscale user accepted, got user=%q ok=%v", user, ok)
	}
}
