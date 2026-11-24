package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KONSERT_CONFIG_DIR", dir)
	t.Setenv("KONSERT_URL", "")
	t.Setenv("KONSERT_TOKEN", "")
	t.Setenv("KONSERT_TOKENS_FILE", "")
	return dir
}

func TestResolveHubOrder(t *testing.T) {
	dir := isolateConfig(t)
	// Nothing configured: the default local hub, no credential.
	if got := resolveHub("", "", ""); got.URL != defaultHubURL || got.Token != "" {
		t.Fatalf("defaults: %+v", got)
	}
	// The local hub's recovery key is used for a loopback URL only.
	tokens := filepath.Join(dir, "hub", tokensFileName)
	if err := writeTokensFile(tokens, persistedTokens{RecoveryKey: "local-rk"}); err != nil {
		t.Fatal(err)
	}
	if err := recordLocalHub("[::]:9123", tokens); err != nil {
		t.Fatal(err)
	}
	if got := resolveHub("", "", ""); got.URL != "http://127.0.0.1:9123" || got.Token != "local-rk" || got.Source != "tokens file" {
		t.Fatalf("local hub: %+v", got)
	}
	if got := resolveHub("https://elsewhere.example:8420", "", ""); got.Token != "" {
		t.Fatalf("recovery key offered to a remote hub: %+v", got)
	}
	// A saved login wins for its hub and becomes the default hub.
	creds, path, _ := loadCredentials()
	creds.Hubs["https://elsewhere.example:8420"] = savedCredential{Token: "kdv_saved"}
	creds.Current = "https://elsewhere.example:8420"
	if err := saveCredentials(creds, path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %o", info.Mode().Perm())
	}
	if got := resolveHub("", "", ""); got.URL != "https://elsewhere.example:8420" || got.Token != "kdv_saved" {
		t.Fatalf("saved login: %+v", got)
	}
	if got := resolveHub("ELSEWHERE.example:8420/", "", ""); got.URL != "http://elsewhere.example:8420" || got.Token != "" {
		t.Fatalf("scheme matters for the saved credential: %+v", got)
	}
	// Flags and env beat everything.
	t.Setenv("KONSERT_TOKEN", "env-tok")
	if got := resolveHub("", "", ""); got.Token != "env-tok" {
		t.Fatalf("env: %+v", got)
	}
	if got := resolveHub("http://x:1", "flag-tok", ""); got.URL != "http://x:1" || got.Token != "flag-tok" {
		t.Fatalf("flags: %+v", got)
	}
}

// authHub runs an auth-enabled hub with a known recovery key.
func authHub(t *testing.T) (*controller.Server, string) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultControllerConfig()
	cfg.RecoveryKey = "rk-test"
	srv := controller.NewServer(cfg, st, nil, "test")
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		srv.Hub().Close()
		_ = st.Close()
	})
	return srv, ts.URL
}

func TestLoginApprovedInBrowserSavesCredential(t *testing.T) {
	isolateConfig(t)
	_, base := authHub(t)
	hub, err := normalizeHubURL(base)
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	codeRe := regexp.MustCompile(`Code: ([0-9A-Z]{4}-[0-9A-Z]{4})`)
	approved := make(chan error, 1)
	// The "browser": wait for the printed code, then approve it with a full credential.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if m := codeRe.FindStringSubmatch(out.String()); m != nil {
				c := newHubClient(hubTarget{URL: hub, Token: "rk-test"}, "test")
				approved <- c.DoJSON(context.Background(), http.MethodPost, "/api/auth/device-code/"+m[1]+"/approve", struct{}{}, nil)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		approved <- context.DeadlineExceeded
	}()
	sleep := func(time.Duration) { time.Sleep(20 * time.Millisecond) }
	if err := runLogin(context.Background(), &out, hub, "build box", sleep); err != nil {
		t.Fatalf("login: %v\n%s", err, out.String())
	}
	if err := <-approved; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/approve/") || !strings.Contains(out.String(), "Signed in to "+hub+" as build box (dv-") {
		t.Fatalf("output:\n%s", out.String())
	}
	// Later commands use the saved credential automatically.
	got := resolveHub("", "", "")
	if got.URL != hub || got.Source != "login" {
		t.Fatalf("after login: %+v", got)
	}
	if _, err := newHubClient(got, "cli").FleetStatus(context.Background()); err != nil {
		t.Fatalf("saved credential rejected: %v", err)
	}

	// konsert logout revokes it on the hub and forgets it.
	var lo bytes.Buffer
	cmd := newLogoutCmd()
	cmd.SetOut(&lo)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := newHubClient(got, "cli").FleetStatus(context.Background()); err == nil {
		t.Fatal("credential still works after logout")
	}
	if creds, _, _ := loadCredentials(); len(creds.Hubs) != 0 || creds.Current != "" {
		t.Fatalf("credentials after logout: %+v", creds)
	}
}

func TestPairCommandPrintsOneLine(t *testing.T) {
	isolateConfig(t)
	_, base := authHub(t)
	var out bytes.Buffer
	cmd := newPairCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--url", base, "--token", "rk-test"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out.String())
	if !regexp.MustCompile(`^Sign in: http://\S+/pair/[0-9A-Z]{4}-[0-9A-Z]{4} \(10 min\)$`).MatchString(line) {
		t.Fatalf("pair output %q", line)
	}
}

func TestRotateRecoveryKey(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, tokensFileName)
	if err := writeTokensFile(tokens, persistedTokens{RecoveryKey: "old"}); err != nil {
		t.Fatal(err)
	}
	// No hub listening: the file is rotated directly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + ln.Addr().String()
	_ = ln.Close()
	var out bytes.Buffer
	if err := rotateRecovery(context.Background(), &out, deadURL, tokens); err != nil {
		t.Fatal(err)
	}
	saved, _ := readTokensFile(tokens)
	if saved.RecoveryKey == "old" || saved.RecoveryKey == "" || strings.Contains(out.String(), saved.RecoveryKey) {
		t.Fatalf("offline rotation: %+v / %q", saved, out.String())
	}
	// A remote URL never receives the recovery key.
	if err := rotateRecovery(context.Background(), &out, "https://hub.example", tokens); err == nil {
		t.Fatal("rotation against a remote hub allowed")
	}
}

func TestRunServeAnnouncesSignInLink(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "konsert.db")
	cfg.ListenAddr = "127.0.0.1:0"
	if err := prepareServeConfig(&cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	addrCh := make(chan net.Addr, 1)
	go func() { done <- runServe(ctx, cfg, logger, func(a net.Addr) { addrCh <- a }) }()
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	cancel()
	<-done
	m := regexp.MustCompile(`msg="Sign in: (http://127\.0\.0\.1:\d+/pair/[0-9A-Z]{4}-[0-9A-Z]{4}) \(10 min, loopback\)"`).FindStringSubmatch(logs.String())
	if m == nil || !strings.Contains(m[1], addr.String()) {
		t.Fatalf("no sign-in line for %s in:\n%s", addr, logs.String())
	}
	// On a terminal the console handler prefixes "15:04:05 INFO ": still one 80-column line.
	if line := "15:04:05 INFO Sign in: " + m[1] + " (10 min, loopback)"; len(line) > 80 {
		t.Fatalf("sign-in line is %d columns: %q", len(line), line)
	}
	if strings.Contains(logs.String(), cfg.RecoveryKey) {
		t.Fatal("a token was logged")
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
