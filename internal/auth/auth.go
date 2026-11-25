// Package auth authenticates workers, browsers, the CLI and MCP clients.
//
// Every signed-in browser, phone, CLI and MCP client is a device with its own random
// credential (see device.go); the hub stores only its SHA-256. Browsers carry it in an
// HttpOnly, SameSite=Strict cookie, the CLI and MCP clients as a Bearer token. Expiry
// slides: a credential is renewed on use and expires only after the configured idle
// period. The recovery key is a hidden emergency credential kept in the hub's tokens
// file; it works as a Bearer token and on the login page's fallback form.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

// SessionCookieName is the browser cookie that carries the device credential.
const SessionCookieName = "syre_session"

// cookieMaxAge is the browser-side lifetime of the session cookie. The hub enforces
// the real (sliding) expiry; 400 days is the most browsers accept.
const cookieMaxAge = 400 * 24 * 3600

// DefaultIdleExpiry is how long a device credential may go unused before it expires.
const DefaultIdleExpiry = 90 * 24 * time.Hour

// Manager handles authentication for browser PWA sessions, the CLI and MCP HTTP
// clients. Worker (syred) connections authenticate with per-machine credentials
// (see machine.go), which the controller checks against its store.
type Manager struct {
	recoveryKey           atomic.Pointer[string]
	noAuth                bool
	trustTailscaleHeaders bool
	devices               DeviceStore
	idle                  atomic.Int64 // time.Duration
	now                   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedDevice // keyed by secret hash
}

// Options configures auth.Manager.
type Options struct {
	RecoveryKey           string
	NoAuth                bool
	TrustTailscaleHeaders bool
	// Devices looks up device credentials; nil disables them (recovery key only).
	Devices DeviceStore
	// IdleExpiry is the sliding expiry; 0 means DefaultIdleExpiry.
	IdleExpiry time.Duration
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
}

// NewManager creates an auth.Manager.
func NewManager(opts Options) *Manager {
	m := &Manager{
		noAuth:                opts.NoAuth,
		trustTailscaleHeaders: opts.TrustTailscaleHeaders,
		devices:               opts.Devices,
		now:                   opts.Now,
		cache:                 make(map[string]cachedDevice),
	}
	if m.now == nil {
		m.now = time.Now
	}
	m.SetRecoveryKey(opts.RecoveryKey)
	m.SetIdleExpiry(opts.IdleExpiry)
	return m
}

// SetRecoveryKey replaces the recovery key (rotation). Empty disables it.
func (m *Manager) SetRecoveryKey(k string) {
	m.recoveryKey.Store(&k)
}

// SetIdleExpiry sets the sliding expiry; d <= 0 means DefaultIdleExpiry.
func (m *Manager) SetIdleExpiry(d time.Duration) {
	if d <= 0 {
		d = DefaultIdleExpiry
	}
	m.idle.Store(int64(d))
}

// IdleExpiry returns the sliding expiry.
func (m *Manager) IdleExpiry() time.Duration { return time.Duration(m.idle.Load()) }

// Now returns the manager's clock reading.
func (m *Manager) Now() time.Time { return m.now().UTC() }

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

// CheckRecoveryKey verifies a candidate recovery key. An unset key denies every
// candidate unless --no-auth is set.
func (m *Manager) CheckRecoveryKey(candidate string) bool {
	if m.noAuth {
		return true
	}
	k := *m.recoveryKey.Load()
	if k == "" || candidate == "" {
		return false
	}
	return constantTimeEquals(candidate, k)
}

// AuthRequired reports whether browser authentication is currently enforced. Only an
// explicit --no-auth disables it.
func (m *Manager) AuthRequired() bool {
	return !m.noAuth
}

// NoAuth reports whether authentication was explicitly disabled with --no-auth.
func (m *Manager) NoAuth() bool { return m.noAuth }

// HasBearer reports whether the request carries an Authorization: Bearer credential.
// Such requests are not driven by ambient browser credentials (cookies).
func HasBearer(r *http.Request) bool { return extractBearer(r) != "" }

// IsLoopbackRemote reports whether the request's direct TCP peer is a loopback address.
func IsLoopbackRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Authentication methods reported in Identity.Method.
const (
	MethodNone      = "none" // --no-auth
	MethodTailscale = "tailscale"
	MethodRecovery  = "recovery"
	MethodDevice    = "device"
)

// Identity is the authenticated caller.
type Identity struct {
	// User names the caller in the audit log.
	User   string
	Method string
	// Device is set when Method is MethodDevice.
	Device *store.Device
}

// DeviceID returns the caller's device id, or "".
func (i Identity) DeviceID() string {
	if i.Device == nil {
		return ""
	}
	return i.Device.ID
}

// Authenticate verifies browser PWA / REST API / CLI requests via:
//  1. --no-auth
//  2. Trusted Tailscale identity header (only with --trust-tailscale-headers and only
//     from a loopback peer, i.e. a request proxied by Tailscale Serve on this host)
//  3. Bearer recovery key (emergency / automation)
//  4. Bearer device credential (CLI, MCP)
//  5. Device credential in the session cookie (browsers)
func (m *Manager) Authenticate(r *http.Request) (Identity, bool) {
	if m.noAuth {
		return Identity{User: "local", Method: MethodNone}, true
	}
	if m.trustTailscaleHeaders && IsLoopbackRemote(r) {
		if tsUser := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); tsUser != "" {
			return Identity{User: tsUser, Method: MethodTailscale}, true
		}
	}
	if tok := extractBearer(r); tok != "" {
		if m.CheckRecoveryKey(tok) {
			return Identity{User: "recovery key", Method: MethodRecovery}, true
		}
		return m.authenticateDevice(r, tok)
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Identity{}, false
	}
	return m.authenticateDevice(r, cookie.Value)
}

// AuthenticateBrowser is Authenticate returning only the audit user name.
func (m *Manager) AuthenticateBrowser(r *http.Request) (string, bool) {
	id, ok := m.Authenticate(r)
	return id.User, ok
}

// MCPScope authenticates a /mcp request (Bearer only) and reports whether the caller
// holds the admin scope. An MCP-kind device credential is authenticated but not admin
// (it may not redirect or replace the LLM API key); the recovery key, other device
// credentials and --no-auth are admin.
func (m *Manager) MCPScope(r *http.Request) (id Identity, ok, admin bool) {
	if m.noAuth {
		return Identity{User: "local", Method: MethodNone}, true, true
	}
	tok := extractBearer(r)
	if tok == "" {
		return Identity{}, false, false
	}
	if m.CheckRecoveryKey(tok) {
		return Identity{User: "recovery key", Method: MethodRecovery}, true, true
	}
	id, ok = m.authenticateDevice(r, tok)
	if !ok {
		return Identity{}, false, false
	}
	return id, true, id.Device.Kind != "mcp"
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SetSessionCookie stores a device credential in the HttpOnly, SameSite=Strict session
// cookie (Secure over HTTPS).
func SetSessionCookie(w http.ResponseWriter, r *http.Request, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secureRequest(r),
		MaxAge:   cookieMaxAge,
	})
}

// ClearSessionCookie removes the session cookie.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secureRequest(r),
		MaxAge:   -1,
	})
}

// SessionSecret returns the device credential presented by the request (Bearer or
// cookie), or "".
func SessionSecret(r *http.Request) string {
	if tok := extractBearer(r); tok != "" {
		return tok
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		return c.Value
	}
	return ""
}
