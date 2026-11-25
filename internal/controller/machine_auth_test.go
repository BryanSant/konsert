package controller

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/auth"
	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/coder/websocket"
)

// testMachineCredential issues a credential for workerID, as a join would.
func testMachineCredential(t *testing.T, srv *Server, workerID string) string {
	t.Helper()
	tok, _, err := srv.mintMachineCredential(context.Background(), workerID, store.MachineCredJoin)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// assertCredentialFor checks that tok is an active credential of workerID.
func assertCredentialFor(t *testing.T, srv *Server, tok, workerID string) {
	t.Helper()
	id, secret, ok := auth.ParseMachineCredential(tok)
	if !ok {
		t.Fatalf("%q is not a machine credential", tok)
	}
	cred, hash, err := srv.store.GetMachineCredential(context.Background(), id)
	if err != nil || cred.WorkerID != workerID || cred.RevokedAt != nil || !auth.VerifyMachineSecret(secret, hash) {
		t.Fatalf("credential %+v (err %v) is not an active credential of %s", cred, err, workerID)
	}
	if strings.Contains(hash, secret) {
		t.Fatal("the secret is stored")
	}
}

const machineAdminTok = "admin-tok"

func newMachineAuthServer(t *testing.T) (*Server, *httptest.Server, *time.Time) {
	t.Helper()
	srv, ts := newSecurityTestServer(t, func(c *config.ControllerConfig) {
		c.NoAuth = false
		c.RecoveryKey = machineAdminTok
		c.PublicURL = "https://hub.example.ts.net"
	})
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	srv.machines.now = func() time.Time { return now }
	return srv, ts, &now
}

// workerConn is a fake syred connection.
type fakeWorkerConn struct {
	conn   *websocket.Conn
	closed chan error
}

// dialWorker connects to /ws/worker with bearer and sends a hello as id. It returns the
// HTTP status of a refused upgrade (0 on success).
func dialWorker(t *testing.T, ts *httptest.Server, bearer, id string) (*fakeWorkerConn, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := http.Header{}
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/worker", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp == nil {
			t.Fatal(err)
		}
		return nil, resp.StatusCode
	}
	raw, _ := protocol.NewEnvelope(protocol.MsgWorkerHello, "", protocol.WorkerHelloPayload{Worker: testWorkerInfo(id, "box-"+id[:4])})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	fc := &fakeWorkerConn{conn: conn, closed: make(chan error, 1)}
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				fc.closed <- err
				return
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return fc, 0
}

func (fc *fakeWorkerConn) waitClosed(t *testing.T) websocket.StatusCode {
	t.Helper()
	select {
	case err := <-fc.closed:
		return websocket.CloseStatus(err)
	case <-time.After(5 * time.Second):
		t.Fatal("connection was not closed")
		return 0
	}
}

func apiCall(t *testing.T, ts *httptest.Server, method, path, body string, bearer string) (int, map[string]any) {
	t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	if bearer != "" {
		h["Authorization"] = "Bearer " + bearer
	}
	resp := doReq(t, method, ts.URL+path, strings.NewReader(body), h)
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

const (
	wA = "0196a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a01"
	wB = "0196a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a02"
)

func TestWorkerCredentialAuthAndRevoke(t *testing.T) {
	srv, ts, _ := newMachineAuthServer(t)
	credA := testMachineCredential(t, srv, wA)
	credB := testMachineCredential(t, srv, wB)

	for _, bad := range []string{"", "fleet-token", "smc_" + wA + "_" + strings.Repeat("x", 43)} {
		if _, code := dialWorker(t, ts, bad, wA); code != http.StatusUnauthorized {
			t.Fatalf("bearer %q: got %d, want 401", bad, code)
		}
	}
	// A wrong secret for a real credential id is refused too.
	id, _, _ := auth.ParseMachineCredential(credA)
	if _, code := dialWorker(t, ts, auth.FormatMachineCredential(id, strings.Repeat("y", 43)), wA); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: got %d", code)
	}

	// B's credential cannot register as A.
	imp, code := dialWorker(t, ts, credB, wA)
	if code != 0 {
		t.Fatalf("upgrade with B's credential: %d", code)
	}
	if st := imp.waitClosed(t); st != websocket.StatusPolicyViolation {
		t.Fatalf("impersonation close = %d, want policy violation", st)
	}
	if srv.hub.IsWorkerConnected(wA) {
		t.Fatal("B registered as A")
	}

	// A's own credential works and records its use.
	a, _ := dialWorker(t, ts, credA, wA)
	waitFor(t, 3*time.Second, "A online", func() bool { return srv.hub.IsWorkerConnected(wA) })
	if c, _, _ := srv.store.GetMachineCredential(context.Background(), id); c.LastUsedAt == nil {
		t.Fatal("last_used_at not recorded")
	}

	// Revoke disconnects at once with 4003; the credential no longer works.
	code, body := apiCall(t, ts, http.MethodPost, "/api/workers/"+wA+"/credential/revoke", "", machineAdminTok)
	if code != 200 || body["revoked"] != float64(1) {
		t.Fatalf("revoke: %d %v", code, body)
	}
	if st := a.waitClosed(t); st != websocket.StatusCode(protocol.WSCloseCredentialRevoked) {
		t.Fatalf("revoke close = %d, want %d", st, protocol.WSCloseCredentialRevoked)
	}
	if _, code := dialWorker(t, ts, credA, wA); code != http.StatusUnauthorized {
		t.Fatalf("revoked credential: got %d", code)
	}
	code, body = apiCall(t, ts, http.MethodGet, "/api/workers/"+wA+"/credential", "", machineAdminTok)
	if code != 200 || len(body["active"].([]any)) != 0 || body["revoked"] != float64(1) {
		t.Fatalf("credential info: %d %v", code, body)
	}
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/workers/"+wA+"/credential/revoke", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revoke: %d", code)
	}
	events, _ := srv.store.ListAudit(context.Background(), store.AuditFilter{Action: AuditMachineCredRevoke})
	if len(events) != 1 || events[0].WorkerID != wA {
		t.Fatalf("revoke audit = %+v", events)
	}
}

func joinBody(code, workerID string) string {
	b, _ := json.Marshal(JoinRequest{Code: code, WorkerID: workerID, Name: "laptop", Hostname: "laptop.local", OS: "linux", Arch: "arm64"})
	return string(b)
}

func TestMachineJoinWithCode(t *testing.T) {
	srv, ts, now := newMachineAuthServer(t)

	code, body := apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", machineAdminTok)
	if code != http.StatusCreated {
		t.Fatalf("create join code: %d %v", code, body)
	}
	jc := body["code"].(string)
	jcID := body["id"].(string)
	if body["hub_url"] != "https://hub.example.ts.net" || body["command"] != "syred join https://hub.example.ts.net "+jc {
		t.Fatalf("join code response = %v", body)
	}
	if _, body := apiCall(t, ts, http.MethodGet, "/api/machines/join-codes/"+jcID, "", machineAdminTok); body["status"] != "pending" {
		t.Fatalf("status = %v", body)
	}
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated join code: %d", code)
	}

	// Malformed requests and wrong codes are refused.
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc, "not-a-uuid"), ""); code != http.StatusBadRequest {
		t.Fatalf("bad worker id: %d", code)
	}
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody("ZZZZ-ZZZZ", wA), ""); code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", code)
	}

	// The code (any case, no dash) buys exactly one credential, bound to the machine.
	code, body = apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(strings.ToLower(strings.ReplaceAll(jc, "-", "")), wA), "")
	if code != 200 || body["worker_id"] != wA || !strings.HasPrefix(body["worker_sid"].(string), "m-") || body["name"] != "laptop" {
		t.Fatalf("join: %d %v", code, body)
	}
	cred := body["credential"].(string)
	assertCredentialFor(t, srv, cred, wA)
	w, err := srv.store.GetWorker(context.Background(), wA)
	if err != nil || w.Launch != store.LaunchManual || w.OS != "linux" || w.Status != "offline" {
		t.Fatalf("joined worker = %+v %v", w, err)
	}
	if _, body := apiCall(t, ts, http.MethodGet, "/api/machines/join-codes/"+jcID, "", machineAdminTok); body["status"] != "used" || body["worker_id"] != wA || body["worker_name"] != "laptop" {
		t.Fatalf("status after use = %v", body)
	}
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc, wB), ""); code != http.StatusUnauthorized {
		t.Fatalf("reused code: %d", code)
	}
	// The machine connects with it.
	dialWorker(t, ts, cred, wA)
	waitFor(t, 3*time.Second, "joined machine online", func() bool { return srv.hub.IsWorkerConnected(wA) })

	// A new code cannot take over a machine that holds an active credential.
	_, body = apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", machineAdminTok)
	jc2 := body["code"].(string)
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc2, wA), ""); code != http.StatusConflict {
		t.Fatalf("takeover: %d", code)
	}
	// After a revoke it can re-join with that (still unused) code.
	apiCall(t, ts, http.MethodPost, "/api/workers/"+wA+"/credential/revoke", "", machineAdminTok)
	if code, body := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc2, wA), ""); code != 200 {
		t.Fatalf("re-join after revoke: %d %v", code, body)
	}

	// Codes expire after 15 minutes.
	_, body = apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", machineAdminTok)
	jc3, jc3ID := body["code"].(string), body["id"].(string)
	*now = now.Add(joinCodeTTL)
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc3, wB), ""); code != http.StatusUnauthorized {
		t.Fatalf("expired code: %d", code)
	}
	if _, body := apiCall(t, ts, http.MethodGet, "/api/machines/join-codes/"+jc3ID, "", machineAdminTok); body["status"] != "expired" {
		t.Fatalf("expired status = %v", body)
	}

	for _, action := range []string{AuditMachineJoinCode, AuditMachineJoin, AuditMachineJoinFailed} {
		if ev, _ := srv.store.ListAudit(context.Background(), store.AuditFilter{Action: action}); len(ev) == 0 {
			t.Errorf("no %s audit event", action)
		}
	}
}

func TestMachineJoinIsRateLimited(t *testing.T) {
	srv, ts, now := newMachineAuthServer(t)
	for i := range srv.machines.joinIP.FreeAttempts + 1 {
		if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody("AAAA-AAAA", wA), ""); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	// Locked out now, even with a valid code.
	_, body := apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", machineAdminTok)
	jc := body["code"].(string)
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc, wA), ""); code != http.StatusTooManyRequests {
		t.Fatalf("locked out: %d", code)
	}
	*now = now.Add(2 * time.Second)
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join", joinBody(jc, wA), ""); code != 200 {
		t.Fatalf("after the lockout: %d", code)
	}
}

func TestJoinCodesAreCapped(t *testing.T) {
	_, ts, _ := newMachineAuthServer(t)
	for range maxActiveJoinCodes {
		if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", machineAdminTok); code != http.StatusCreated {
			t.Fatalf("create: %d", code)
		}
	}
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/join-codes", "", machineAdminTok); code != http.StatusTooManyRequests {
		t.Fatalf("over the cap: %d", code)
	}
}

func TestRotateMachineCredentials(t *testing.T) {
	srv, ts, _ := newMachineAuthServer(t)
	ctx := context.Background()
	const local = "0196a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a03"
	if _, err := srv.store.EnsureWorker(ctx, store.Worker{ID: local, Name: "hub", Launch: store.LaunchLocal}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.EnsureWorker(ctx, store.Worker{ID: wB, Name: "idle"}); err != nil {
		t.Fatal(err)
	}
	credA := testMachineCredential(t, srv, wA)
	credLocal := testMachineCredential(t, srv, local)
	a, _ := dialWorker(t, ts, credA, wA)
	dialWorker(t, ts, credLocal, local)
	waitFor(t, 3*time.Second, "both online", func() bool {
		return srv.hub.IsWorkerConnected(wA) && srv.hub.IsWorkerConnected(local)
	})

	code, body := apiCall(t, ts, http.MethodPost, "/api/machines/credentials/rotate", "", machineAdminTok)
	if code != 200 {
		t.Fatalf("rotate: %d %v", code, body)
	}
	var res RotationResponse
	b, _ := json.Marshal(body)
	_ = json.Unmarshal(b, &res)
	// Only the joined machine is rotated; the hub-installed one and the machine without
	// a credential are skipped.
	if len(res.Results) != 1 || res.Results[0].WorkerID != wA || !res.Results[0].NeedsJoin || res.NeedsJoin != 1 || res.Failed != 0 {
		t.Fatalf("rotation = %+v", res)
	}
	if st := a.waitClosed(t); st != websocket.StatusCode(protocol.WSCloseCredentialRevoked) {
		t.Fatalf("rotated machine close = %d", st)
	}
	assertCredentialFor(t, srv, credLocal, local)
	if !srv.hub.IsWorkerConnected(local) {
		t.Fatal("hub-installed machine was disconnected")
	}
	if code, _ := apiCall(t, ts, http.MethodPost, "/api/machines/credentials/rotate", `{"machine":"m-zzzzz"}`, machineAdminTok); code != http.StatusNotFound {
		t.Fatalf("unknown machine: %d", code)
	}
	if ev, _ := srv.store.ListAudit(ctx, store.AuditFilter{Action: AuditMachineCredRotate}); len(ev) != 1 {
		t.Fatalf("rotate audit = %+v", ev)
	}

	// The CLI client speaks the same API.
	cli := NewRemoteFleetClient(ts.URL, machineAdminTok)
	if _, err := cli.RotateMachineCredentials(ctx, wA); err != nil {
		t.Fatal(err)
	}
	var apiErr *APIError
	if _, err := NewRemoteFleetClient(ts.URL, "").RotateMachineCredentials(ctx, ""); !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("unauthenticated CLI rotate: %v", err)
	}
}

func TestNoAuthHubAcceptsWorkersWithoutCredential(t *testing.T) {
	srv, ts := newSecurityTestServer(t, nil) // --no-auth
	dialWorker(t, ts, "", wA)
	waitFor(t, 3*time.Second, "worker online", func() bool { return srv.hub.IsWorkerConnected(wA) })
}

func TestJoinHubURL(t *testing.T) {
	srv, _ := newSecurityTestServer(t, nil)
	srv.SetListenAddr("0.0.0.0:8420")
	r := httptest.NewRequest(http.MethodPost, "/api/machines/join-codes", nil)
	r.Host = "desk.lan:8420"
	if got := srv.joinHubURL(r); got != "http://desk.lan:8420" {
		t.Fatalf("browser host: %q", got)
	}
	r.Host = "127.0.0.1:8420"
	if got := srv.joinHubURL(r); strings.Contains(got, "127.0.0.1") || !strings.HasSuffix(got, ":8420") {
		t.Fatalf("loopback must become this host's name: %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Host = "desk.example.ts.net"
	if got := srv.joinHubURL(r); got != "https://desk.example.ts.net" {
		t.Fatalf("https proxy: %q", got)
	}
	srv.cfg.PublicURL = "https://syre.example.ts.net/"
	if got := srv.joinHubURL(r); got != "https://syre.example.ts.net" {
		t.Fatalf("public url: %q", got)
	}
}
