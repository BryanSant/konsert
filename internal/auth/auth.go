package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const SessionCookieName = "konsert_session"

// Manager handles authentication for workers, browser PWA sessions, and MCP HTTP clients.
type Manager struct {
	fleetToken            string
	adminToken            string
	mcpToken              string
	noAuth                bool
	trustTailscaleHeaders bool
	cookieSecret          []byte
}

// Options configures auth.Manager.
type Options struct {
	FleetToken            string
	AdminToken            string
	MCPToken              string
	NoAuth                bool
	TrustTailscaleHeaders bool
}

// NewManager creates an auth.Manager.
func NewManager(opts Options) *Manager {
	secret := make([]byte, 32)
	if opts.AdminToken != "" {
		sum := sha256.Sum256([]byte("konsert-cookie-key:" + opts.AdminToken))
		copy(secret, sum[:])
	} else {
		_, _ = rand.Read(secret)
	}
	mcpTok := opts.MCPToken
	if mcpTok == "" {
		mcpTok = opts.AdminToken
	}
	return &Manager{
		fleetToken:            opts.FleetToken,
		adminToken:            opts.AdminToken,
		mcpToken:              mcpTok,
		noAuth:                opts.NoAuth,
		trustTailscaleHeaders: opts.TrustTailscaleHeaders,
		cookieSecret:          secret,
	}
}

// GenerateToken returns a cryptographically random hex token of byteLen bytes (2*byteLen hex chars).
func GenerateToken(byteLen int) (string, error) {
	if byteLen <= 0 {
		byteLen = 32
	}
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func constantTimeEquals(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func extractBearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// CheckFleetAuth verifies the outbound worker WebSocket Authorization header.
func (m *Manager) CheckFleetAuth(r *http.Request) bool {
	if m.noAuth || m.fleetToken == "" {
		return true
	}
	tok := extractBearer(r)
	if tok == "" {
		return false
	}
	return constantTimeEquals(tok, m.fleetToken)
}

// CheckMCPAuth verifies bearer token authentication on Streamable HTTP/SSE MCP endpoints.
func (m *Manager) CheckMCPAuth(r *http.Request) bool {
	if m.noAuth || m.mcpToken == "" {
		return true
	}
	tok := extractBearer(r)
	if tok == "" {
		return false
	}
	return constantTimeEquals(tok, m.mcpToken)
}

// CheckAdminToken verifies a login passphrase/admin token.
func (m *Manager) CheckAdminToken(candidate string) bool {
	if m.noAuth || m.adminToken == "" {
		return true
	}
	return constantTimeEquals(candidate, m.adminToken)
}

// AuthRequired reports whether browser authentication is currently enforced.
func (m *Manager) AuthRequired() bool {
	return !m.noAuth && m.adminToken != ""
}

// AuthenticateBrowser verifies browser PWA / REST API / CLI requests via:
// 1. --no-auth (or unset admin token)
// 2. Trusted Tailscale identity header (when --trust-tailscale-headers is enabled)
// 3. Bearer admin token (for CLI / API automation)
// 4. Signed HttpOnly session cookie
func (m *Manager) AuthenticateBrowser(r *http.Request) (string, bool) {
	if !m.AuthRequired() {
		return "local", true
	}
	if m.trustTailscaleHeaders {
		if tsUser := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsUser != "" {
			return tsUser, true
		}
	}
	if tok := extractBearer(r); tok != "" && constantTimeEquals(tok, m.adminToken) {
		return "token", true
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return m.verifySessionValue(cookie.Value)
}

// SetSessionCookie issues a signed HttpOnly, SameSite=Strict session cookie.
func (m *Manager) SetSessionCookie(w http.ResponseWriter, r *http.Request, user string) {
	exp := time.Now().UTC().Add(7 * 24 * time.Hour).Unix()
	val := m.signSessionValue(user, exp)
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   7 * 24 * 3600,
	})
}

// ClearSessionCookie removes the session cookie.
func (m *Manager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func (m *Manager) signSessionValue(user string, exp int64) string {
	payload := fmt.Sprintf("%s|%d", user, exp)
	mac := hmac.New(sha256.New, m.cookieSecret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	encPayload := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encPayload + "." + sig
}

func (m *Manager) verifySessionValue(val string) (string, bool) {
	encPayload, sig, ok := strings.Cut(val, ".")
	if !ok {
		return "", false
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, m.cookieSecret)
	mac.Write(rawPayload)
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !constantTimeEquals(sig, expectedSig) {
		return "", false
	}
	user, expStr, ok := strings.Cut(string(rawPayload), "|")
	if !ok {
		return "", false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().UTC().Unix() > exp {
		return "", false
	}
	return user, true
}
