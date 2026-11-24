package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

type fakeDevices struct {
	mu      sync.Mutex
	byHash  map[string]store.Device
	lookups int
	touches int
}

func (f *fakeDevices) GetDeviceBySecretHash(_ context.Context, hash string) (store.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	d, ok := f.byHash[hash]
	if !ok {
		return store.Device{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeDevices) TouchDevice(_ context.Context, id string, at, exp time.Time, ip, ua string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches++
	for h, d := range f.byHash {
		if d.ID == id {
			d.LastUsedAt, d.ExpiresAt, d.LastIP, d.LastUserAgent = at, exp, ip, ua
			f.byHash[h] = d
		}
	}
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newDeviceManager(t *testing.T, kind string) (*Manager, *fakeDevices, *clock, string) {
	t.Helper()
	secret, hash, err := NewDeviceSecret()
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)}
	fd := &fakeDevices{byHash: map[string]store.Device{hash: {
		ID: "d1", SID: "dv-7qk3m", Name: "Pixel", Kind: kind, SecretHash: hash,
		CreatedAt: clk.Now(), LastUsedAt: clk.Now(), ExpiresAt: clk.Now().Add(DefaultIdleExpiry),
	}}}
	m := NewManager(Options{RecoveryKey: "recovery-secret", Devices: fd, Now: clk.Now})
	return m, fd, clk, secret
}

func cookieReq(secret string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: secret})
	r.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/131.0 Mobile Safari/537.36")
	return r
}

func TestDeviceCredentialCookieAndBearer(t *testing.T) {
	m, _, _, secret := newDeviceManager(t, store.DeviceKindPhone)
	id, ok := m.Authenticate(cookieReq(secret))
	if !ok || id.Method != MethodDevice || id.DeviceID() != "d1" || id.User != "Pixel (dv-7qk3m)" {
		t.Fatalf("cookie: %+v %v", id, ok)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	if id, ok := m.Authenticate(r); !ok || id.DeviceID() != "d1" {
		t.Fatalf("bearer: %+v %v", id, ok)
	}
	for _, bad := range []string{"", secret + "x", "kdv_nope", strings.TrimPrefix(secret, DeviceSecretPrefix), "recovery-secretx"} {
		if _, ok := m.Authenticate(cookieReq(bad)); ok {
			t.Fatalf("credential %q accepted", bad)
		}
	}
	// The recovery key works as a Bearer, not as a cookie.
	r = httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	r.Header.Set("Authorization", "Bearer recovery-secret")
	if id, ok := m.Authenticate(r); !ok || id.Method != MethodRecovery || id.Device != nil {
		t.Fatalf("recovery bearer: %+v %v", id, ok)
	}
	if _, ok := m.Authenticate(cookieReq("recovery-secret")); ok {
		t.Fatal("recovery key accepted as a cookie")
	}
	m.SetRecoveryKey("rotated")
	if _, ok := m.Authenticate(r); ok {
		t.Fatal("old recovery key accepted after rotation")
	}
}

// A device used at least every idle period never expires; an idle one does.
func TestSlidingExpiry(t *testing.T) {
	m, fd, clk, secret := newDeviceManager(t, store.DeviceKindPhone)
	for range 10 { // ~2 years of monthly use
		clk.Add(30 * 24 * time.Hour)
		if _, ok := m.Authenticate(cookieReq(secret)); !ok {
			t.Fatalf("device used monthly expired at %s", clk.Now())
		}
	}
	d, _ := fd.GetDeviceBySecretHash(context.Background(), HashSecret(secret))
	if !d.LastUsedAt.Equal(clk.Now()) || !d.ExpiresAt.Equal(clk.Now().Add(DefaultIdleExpiry)) || d.LastUserAgent != "Chrome on Android" {
		t.Fatalf("touch not recorded: %+v", d)
	}
	clk.Add(DefaultIdleExpiry + time.Minute)
	if _, ok := m.Authenticate(cookieReq(secret)); ok {
		t.Fatal("device idle past the expiry still accepted")
	}

	// A shorter idle setting takes effect for existing devices at once.
	m2, _, clk2, secret2 := newDeviceManager(t, store.DeviceKindBrowser)
	clk2.Add(10 * 24 * time.Hour)
	m2.SetIdleExpiry(7 * 24 * time.Hour)
	if _, ok := m2.Authenticate(cookieReq(secret2)); ok {
		t.Fatal("device idle past a shortened expiry still accepted")
	}
}

func TestTouchIsThrottledAndCached(t *testing.T) {
	m, fd, clk, secret := newDeviceManager(t, store.DeviceKindBrowser)
	for range 50 {
		clk.Add(time.Second)
		if _, ok := m.Authenticate(cookieReq(secret)); !ok {
			t.Fatal("rejected")
		}
	}
	if fd.touches != 0 {
		t.Fatalf("%d touches within the throttle interval, want 0", fd.touches)
	}
	if fd.lookups > 3 {
		t.Fatalf("%d store lookups for 50 requests in 50s, want the cache to absorb them", fd.lookups)
	}
	clk.Add(touchEvery)
	m.Authenticate(cookieReq(secret))
	if fd.touches != 1 {
		t.Fatalf("touches = %d, want 1 after the interval", fd.touches)
	}
}

func TestRevocationIsImmediateAfterInvalidate(t *testing.T) {
	m, fd, _, secret := newDeviceManager(t, store.DeviceKindBrowser)
	if _, ok := m.Authenticate(cookieReq(secret)); !ok {
		t.Fatal("rejected")
	}
	now := time.Now()
	fd.mu.Lock()
	for h, d := range fd.byHash {
		d.RevokedAt = &now
		fd.byHash[h] = d
	}
	fd.mu.Unlock()
	m.Invalidate("d1")
	if _, ok := m.Authenticate(cookieReq(secret)); ok {
		t.Fatal("revoked device accepted after Invalidate")
	}
}

func TestMCPScopeByDeviceKind(t *testing.T) {
	for kind, wantAdmin := range map[string]bool{store.DeviceKindMCP: false, store.DeviceKindCLI: true, store.DeviceKindBrowser: true} {
		m, _, _, secret := newDeviceManager(t, kind)
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		if _, ok, admin := m.MCPScope(r); !ok || admin != wantAdmin {
			t.Fatalf("%s: ok=%v admin=%v", kind, ok, admin)
		}
		// Cookies never authenticate /mcp.
		if _, ok, _ := m.MCPScope(cookieReq(secret)); ok {
			t.Fatalf("%s: cookie accepted on /mcp", kind)
		}
	}
	m, _, _, _ := newDeviceManager(t, store.DeviceKindMCP)
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer recovery-secret")
	if _, ok, admin := m.MCPScope(r); !ok || !admin {
		t.Fatal("recovery key must hold the MCP admin scope")
	}
}

func TestPairingCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		code, hash, err := NewPairingCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 9 || code[4] != '-' {
			t.Fatalf("code %q is not XXXX-XXXX", code)
		}
		canon, err := NormalizePairingCode(code)
		if err != nil || HashPairingCode(canon) != hash {
			t.Fatalf("normalize %q: %q %v", code, canon, err)
		}
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("ambiguous letter in %q", code)
		}
		seen[code] = true
	}
	if len(seen) < 199 {
		t.Fatalf("only %d distinct codes in 200", len(seen))
	}
	for in, want := range map[string]string{"7kq2-m9tx": "7KQ2M9TX", " 7KQ2 M9TX ": "7KQ2M9TX", "oIl1-0000": "0111" + "0000"} {
		if got, err := NormalizePairingCode(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "7KQ2-M9T", "7KQ2-M9TXX", "7KQ2-M9TU", "7KQ2_M9TX", "ÄKQ2-M9TX"} {
		if _, err := NormalizePairingCode(bad); err == nil {
			t.Errorf("Normalize(%q) accepted", bad)
		}
	}
}

func TestUserAgentSummary(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36":                   "Chrome on macOS",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1": "Safari on iPhone",
		"Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0":                                                                  "Firefox on Linux",
		"konsert/dev (darwin)": "konsert/dev",
		"":                     "",
	}
	for ua, want := range cases {
		if got := UserAgentSummary(ua); got != want {
			t.Errorf("UserAgentSummary(%q) = %q, want %q", ua, got, want)
		}
	}
	if DeviceKindForUA("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)") != store.DeviceKindPhone ||
		DeviceKindForUA("Mozilla/5.0 (Macintosh)") != store.DeviceKindBrowser {
		t.Fatal("DeviceKindForUA")
	}
}

func TestWindowLimiter(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := NewWindowLimiter(3, time.Minute)
	l.Now = clk.Now
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("event %d denied", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("4th event allowed")
	}
	if !l.Allow("b") {
		t.Fatal("other key limited")
	}
	clk.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("new window denied")
	}
}
