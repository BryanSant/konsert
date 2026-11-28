package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/auth"
	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
	"github.com/coder/websocket"
)

const (
	testRecoveryKey = "recovery-key-for-tests"
	iphoneUA        = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"
)

func newAuthTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.RecoveryKey = testRecoveryKey
		c.EnableMCPHTTP = true
	})
	return srv, ts.URL
}

// call sends a JSON request with an optional Bearer token or cookie and decodes the reply.
func call(t *testing.T, method, url, bearer string, cookie *http.Cookie, body string, out any) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", iphoneUA)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s %s: %v (%s)", method, url, err, raw)
		}
	}
	return resp, string(raw)
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

// mintMCPCredential creates an MCP-only device credential with the recovery key.
func mintMCPCredential(t *testing.T, baseURL, recoveryKey string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	resp, raw := call(t, http.MethodPost, baseURL+"/api/devices/mcp", recoveryKey, nil, `{"name":"test MCP"}`, &out)
	if resp.StatusCode != http.StatusCreated || !strings.HasPrefix(out.Token, auth.DeviceSecretPrefix) {
		t.Fatalf("mint MCP credential: %d %s", resp.StatusCode, raw)
	}
	return out.Token
}

func pairPhone(t *testing.T, base string) (*http.Cookie, PairingCode) {
	t.Helper()
	var pc PairingCode
	resp, raw := call(t, http.MethodPost, base+"/api/pairing-codes", testRecoveryKey, nil, `{}`, &pc)
	if resp.StatusCode != http.StatusCreated || len(pc.Code) != 9 || !ids.Valid(ids.PairingCode, pc.SID) ||
		!strings.HasSuffix(pc.URL, "/pair/"+pc.Code) || pc.QR.Size == 0 || pc.QR.Path == "" {
		t.Fatalf("create pairing code: %d %s", resp.StatusCode, raw)
	}
	resp, raw = call(t, http.MethodPost, base+"/api/auth/pair", "", nil, `{"code":"`+strings.ToLower(pc.Code)+`","name":"Test iPhone"}`, nil)
	cookie := sessionCookie(resp)
	if resp.StatusCode != http.StatusOK || cookie == nil {
		t.Fatalf("pair: %d %s", resp.StatusCode, raw)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || !strings.HasPrefix(cookie.Value, auth.DeviceSecretPrefix) {
		t.Fatalf("session cookie flags: %+v", cookie)
	}
	return cookie, pc
}

type devicesList struct {
	Devices []struct {
		ID            string    `json:"id"`
		SID           string    `json:"sid"`
		Name          string    `json:"name"`
		Kind          string    `json:"kind"`
		LastUserAgent string    `json:"last_user_agent"`
		Current       bool      `json:"current"`
		Expired       bool      `json:"expired"`
		LastUsedAt    time.Time `json:"last_used_at"`
		ExpiresAt     time.Time `json:"expires_at"`
	} `json:"devices"`
	IdleDays int `json:"idle_days"`
}

func TestPairingLinkSignsInADevice(t *testing.T) {
	srv, base := newAuthTestServer(t)
	cookie, pc := pairPhone(t, base)

	// The cookie is a working credential, and this device is "current".
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", "", cookie, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("paired cookie on /api/fleet: %d", resp.StatusCode)
	}
	var me struct {
		Authenticated bool   `json:"authenticated"`
		User          string `json:"user"`
		Method        string `json:"method"`
		Device        struct {
			SID  string `json:"sid"`
			Kind string `json:"kind"`
		} `json:"device"`
	}
	call(t, http.MethodGet, base+"/api/auth/me", "", cookie, "", &me)
	if !me.Authenticated || me.Method != auth.MethodDevice || me.Device.Kind != store.DeviceKindPhone || !strings.HasPrefix(me.User, "Test iPhone (dv-") {
		t.Fatalf("me = %+v", me)
	}
	var list devicesList
	call(t, http.MethodGet, base+"/api/devices", "", cookie, "", &list)
	if len(list.Devices) != 1 || !list.Devices[0].Current || list.Devices[0].Name != "Test iPhone" ||
		list.Devices[0].LastUserAgent != "Safari on iPhone" || list.IdleDays != 90 {
		t.Fatalf("devices = %+v", list)
	}
	if got := list.Devices[0].ExpiresAt.Sub(list.Devices[0].LastUsedAt); got != 90*24*time.Hour {
		t.Fatalf("sliding expiry window = %v, want 90 days", got)
	}

	// Single use.
	resp, _ := call(t, http.MethodPost, base+"/api/auth/pair", "", nil, `{"code":"`+pc.Code+`"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil {
		t.Fatalf("second redemption: %d", resp.StatusCode)
	}

	// Audited by short id; the code itself is never recorded.
	events, _ := srv.store.ListAudit(context.Background(), store.AuditFilter{Limit: 50})
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
		if strings.Contains(e.Message+e.MetaJSON, pc.Code) || strings.Contains(e.Message+e.MetaJSON, strings.ReplaceAll(pc.Code, "-", "")) {
			t.Fatalf("pairing code leaked into the audit log: %+v", e)
		}
	}
	if j := strings.Join(actions, ","); !strings.Contains(j, AuditDevicePaired) || !strings.Contains(j, AuditDevicePairCode) {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestPairingCodeExpiresAndIsRateLimited(t *testing.T) {
	srv, base := newAuthTestServer(t)
	now := time.Now()
	srv.auth = auth.NewManager(auth.Options{RecoveryKey: testRecoveryKey, Devices: srv.store, Now: func() time.Time { return now }})
	pc, err := srv.MintPairingCode(context.Background(), base, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(auth.PairingCodeTTL + time.Second)
	if resp, _ := call(t, http.MethodPost, base+"/api/auth/pair", "", nil, `{"code":"`+pc.Code+`"}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired code: %d", resp.StatusCode)
	}
	// Guessing is throttled per peer after a few failures.
	var last int
	for range 8 {
		resp, _ := call(t, http.MethodPost, base+"/api/auth/pair", "", nil, `{"code":"AAAA-BBBB"}`, nil)
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after repeated bad codes: %d, want 429", last)
	}
}

func TestRevokeIsImmediateAndClosesSockets(t *testing.T) {
	_, base := newAuthTestServer(t)
	cookie, _ := pairPhone(t, base)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Cookie", cookie.Name+"="+cookie.Value)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws/client", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial /ws/client: %v", err)
	}
	defer conn.CloseNow()
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				readErr <- err
				return
			}
		}
	}()

	var list devicesList
	call(t, http.MethodGet, base+"/api/devices", testRecoveryKey, nil, "", &list)
	resp, raw := call(t, http.MethodDelete, base+"/api/devices/"+list.Devices[0].SID, testRecoveryKey, nil, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %s", resp.StatusCode, raw)
	}
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", "", cookie, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked cookie still works: %d", resp.StatusCode)
	}
	select {
	case err := <-readErr:
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation && !errors.Is(err, io.EOF) && err == nil {
			t.Fatalf("socket closed with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked device's socket stayed open")
	}
	call(t, http.MethodGet, base+"/api/devices", testRecoveryKey, nil, "", &list)
	if len(list.Devices) != 0 {
		t.Fatalf("revoked device still listed: %+v", list)
	}
}

func TestRenameAndLogout(t *testing.T) {
	_, base := newAuthTestServer(t)
	cookie, _ := pairPhone(t, base)
	var list devicesList
	call(t, http.MethodGet, base+"/api/devices", "", cookie, "", &list)
	resp, raw := call(t, http.MethodPatch, base+"/api/devices/"+list.Devices[0].ID, "", cookie, `{"name":"  Pocket\tphone  "}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: %d %s", resp.StatusCode, raw)
	}
	call(t, http.MethodGet, base+"/api/devices", "", cookie, "", &list)
	if list.Devices[0].Name != "Pocket phone" {
		t.Fatalf("renamed = %q", list.Devices[0].Name)
	}
	resp, _ = call(t, http.MethodPost, base+"/api/auth/logout", "", cookie, `{}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", "", cookie, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("signing out did not revoke the device")
	}
}

func TestRecoveryKeyLoginAndRotation(t *testing.T) {
	srv, base := newAuthTestServer(t)
	if resp, _ := call(t, http.MethodPost, base+"/api/auth/login", "", nil, `{"token":"wrong"}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong recovery key: %d", resp.StatusCode)
	}
	resp, raw := call(t, http.MethodPost, base+"/api/auth/login", "", nil, `{"token":"`+testRecoveryKey+`"}`, nil)
	cookie := sessionCookie(resp)
	if resp.StatusCode != http.StatusOK || cookie == nil || cookie.Value == testRecoveryKey {
		t.Fatalf("recovery login: %d %s", resp.StatusCode, raw)
	}

	// Without a tokens file behind it the key cannot be rotated here.
	if resp, _ := call(t, http.MethodPost, base+"/api/auth/recovery-key/rotate", "", cookie, `{}`, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("rotate without rotator: %d", resp.StatusCode)
	}
	var persisted string
	srv.SetRecoveryKeyRotator(func(k string) error { persisted = k; return nil })
	if resp, raw := call(t, http.MethodPost, base+"/api/auth/recovery-key/rotate", testRecoveryKey, nil, `{}`, nil); resp.StatusCode != http.StatusOK || strings.Contains(raw, persisted) {
		t.Fatalf("rotate: %d %s", resp.StatusCode, raw)
	}
	if persisted == "" || persisted == testRecoveryKey {
		t.Fatal("rotation persisted no new key")
	}
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", testRecoveryKey, nil, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("old recovery key still works after rotation")
	}
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", persisted, nil, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("new recovery key does not work")
	}
	// Devices are unaffected by rotation.
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", "", cookie, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("rotation signed out a device")
	}
}

func TestCLIDeviceCodeLogin(t *testing.T) {
	_, base := newAuthTestServer(t)
	browser, _ := pairPhone(t, base)

	var start struct {
		Code      string `json:"code"`
		PollToken string `json:"poll_token"`
		Interval  int    `json:"interval"`
	}
	resp, raw := call(t, http.MethodPost, base+"/api/auth/device-code", "", nil, `{"name":"build box"}`, &start)
	if resp.StatusCode != http.StatusCreated || start.PollToken == "" || start.Interval < 1 {
		t.Fatalf("start: %d %s", resp.StatusCode, raw)
	}
	var startRaw map[string]any
	_ = json.Unmarshal([]byte(raw), &startRaw)
	if !strings.HasSuffix(startRaw["verify_url"].(string), "/approve/"+start.Code) {
		t.Fatalf("verify_url = %v", startRaw["verify_url"])
	}
	poll := func() (string, string) {
		var p struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		call(t, http.MethodPost, base+"/api/auth/device-code/poll", "", nil, `{"poll_token":"`+startRaw["poll_token"].(string)+`"}`, &p)
		return p.Status, p.Token
	}
	if st, tok := poll(); st != "pending" || tok != "" {
		t.Fatalf("poll before approval: %s %q", st, tok)
	}
	// Approving needs a signed-in browser; the page shows what asks to sign in.
	if resp, _ := call(t, http.MethodGet, base+"/api/auth/device-code/"+start.Code, "", nil, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approval page: %d", resp.StatusCode)
	}
	var info struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	call(t, http.MethodGet, base+"/api/auth/device-code/"+start.Code, "", browser, "", &info)
	if info.Name != "build box" || info.Kind != "cli" || info.Status != "pending" {
		t.Fatalf("approval info = %+v", info)
	}
	if resp, raw := call(t, http.MethodPost, base+"/api/auth/device-code/"+start.Code+"/approve", "", browser, `{}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d %s", resp.StatusCode, raw)
	}
	st, tok := poll()
	if st != "approved" || !strings.HasPrefix(tok, auth.DeviceSecretPrefix) {
		t.Fatalf("poll after approval: %s %q", st, tok)
	}
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", tok, nil, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("CLI credential does not work")
	}
	if st, tok := poll(); st != "redeemed" || tok != "" {
		t.Fatalf("second poll: %s %q", st, tok)
	}

	// A denied request never yields a credential.
	resp, raw = call(t, http.MethodPost, base+"/api/auth/device-code", "", nil, `{}`, nil)
	var s2 map[string]any
	_ = json.Unmarshal([]byte(raw), &s2)
	call(t, http.MethodPost, base+"/api/auth/device-code/"+s2["code"].(string)+"/deny", "", browser, `{}`, nil)
	var p2 struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	call(t, http.MethodPost, base+"/api/auth/device-code/poll", "", nil, `{"poll_token":"`+s2["poll_token"].(string)+`"}`, &p2)
	if resp.StatusCode != http.StatusCreated || p2.Status != "denied" || p2.Token != "" {
		t.Fatalf("denied: %+v", p2)
	}
	// A login code is not a pairing code.
	if resp, _ := call(t, http.MethodPost, base+"/api/auth/pair", "", nil, `{"code":"`+s2["code"].(string)+`"}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login code redeemed as a pairing code: %d", resp.StatusCode)
	}
}

func TestMCPCredentialIsMCPOnly(t *testing.T) {
	_, base := newAuthTestServer(t)
	tok := mintMCPCredential(t, base, testRecoveryKey)
	if resp, _ := call(t, http.MethodGet, base+"/api/fleet", tok, nil, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("MCP credential on REST: %d, want 403", resp.StatusCode)
	}
	sess := connectMCPHTTP(t, base, tok)
	if _, err := sess.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("MCP credential on /mcp: %v", err)
	}
}

func TestDeviceIdleDaysSetting(t *testing.T) {
	srv, base := newAuthTestServer(t)
	if resp, raw := call(t, http.MethodPut, base+"/api/settings", testRecoveryKey, nil, `{"device_idle_days":0}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("idle 0: %d %s", resp.StatusCode, raw)
	}
	var out settingsResponse
	if resp, raw := call(t, http.MethodPut, base+"/api/settings", testRecoveryKey, nil, `{"device_idle_days":30}`, &out); resp.StatusCode != http.StatusOK || out.DeviceIdleDays != 30 {
		t.Fatalf("idle 30: %d %s", resp.StatusCode, raw)
	}
	if srv.auth.IdleExpiry() != 30*24*time.Hour {
		t.Fatalf("idle expiry not applied: %v", srv.auth.IdleExpiry())
	}
}

func TestBestBaseURL(t *testing.T) {
	if u, lo := BestBaseURL(":8420", "https://syre.example.ts.net/"); u != "https://syre.example.ts.net" || lo {
		t.Fatalf("public: %s %v", u, lo)
	}
	if u, lo := BestBaseURL("127.0.0.1:9000", ""); u != "http://127.0.0.1:9000" || !lo {
		t.Fatalf("loopback: %s %v", u, lo)
	}
	if u, lo := BestBaseURL("10.0.0.5:8420", ""); u != "http://10.0.0.5:8420" || lo {
		t.Fatalf("specific: %s %v", u, lo)
	}
	if u, _ := BestBaseURL("[::]:8420", ""); !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, ":8420") {
		t.Fatalf("wildcard: %s", u)
	}
}

// syre open's sign-in codes: issued only to this machine over loopback, valid a minute,
// and redeemable only from this machine (not through a proxy that relays from loopback).
func TestLauncherSignInCodeIsLocalOnly(t *testing.T) {
	srv, base := newAuthTestServer(t)
	post := func(path, body string, headers map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testRecoveryKey)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}
	// Through a proxy (a forwarding header): refused.
	if resp, raw := post("/api/pairing-codes", `{"launcher":true}`, map[string]string{"X-Forwarded-For": "100.64.0.9"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("proxied launcher code: %d %s", resp.StatusCode, raw)
	}
	mint := func() PairingCode {
		resp, raw := post("/api/pairing-codes", `{"launcher":true}`, nil)
		var pc PairingCode
		if resp.StatusCode != http.StatusCreated || json.Unmarshal([]byte(raw), &pc) != nil {
			t.Fatalf("launcher code: %d %s", resp.StatusCode, raw)
		}
		return pc
	}
	pc := mint()
	if !strings.HasPrefix(pc.URL, base+"/pair/") || time.Until(pc.ExpiresAt) > auth.LaunchCodeTTL+time.Second {
		t.Fatalf("launcher code: %+v", pc)
	}
	// Redeemed through a proxy: invalid (and not used up).
	req, _ := http.NewRequest(http.MethodPost, base+"/api/auth/pair", strings.NewReader(`{"code":"`+pc.Code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "100.64.0.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || sessionCookie(resp) != nil {
		t.Fatalf("proxied redemption: %d", resp.StatusCode)
	}
	// From this machine: signed in, audited as the launcher.
	resp, raw := call(t, http.MethodPost, base+"/api/auth/pair", "", nil, `{"code":"`+pc.Code+`","name":"Launcher"}`, nil)
	if resp.StatusCode != http.StatusOK || sessionCookie(resp) == nil {
		t.Fatalf("local redemption: %d %s", resp.StatusCode, raw)
	}
	events, _ := srv.store.ListAudit(context.Background(), store.AuditFilter{Action: AuditDevicePaired, Limit: 5})
	if len(events) != 1 || !strings.Contains(events[0].Message, "via the local launcher") {
		t.Fatalf("audit: %+v", events)
	}
}
