package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubOpen replaces the browser and the hub starter for one test.
func stubOpen(t *testing.T, start func(dir, logFile string) error) *[]string {
	t.Helper()
	var opened []string
	prevOpen, prevStart := openURL, startHubProcess
	openURL = func(link string) error { opened = append(opened, link); return nil }
	startHubProcess = func(dir, logFile string) error {
		if start == nil {
			t.Fatal("the hub was started although it was running")
		}
		return start(dir, logFile)
	}
	t.Cleanup(func() { openURL, startHubProcess = prevOpen, prevStart })
	return &opened
}

// localAuthHub runs an auth-enabled hub recorded as this machine's hub.
func localAuthHub(t *testing.T, dir string) string {
	t.Helper()
	_, base := authHub(t)
	tokens := filepath.Join(dir, "hub", tokensFileName)
	if err := writeTokensFile(tokens, persistedTokens{RecoveryKey: "rk-test"}); err != nil {
		t.Fatal(err)
	}
	if err := recordLocalHub(strings.TrimPrefix(base, "http://"), tokens); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestOpenWithRunningHubOpensSignedInLink(t *testing.T) {
	dir := isolateConfig(t)
	base := localAuthHub(t, dir)
	opened := stubOpen(t, nil)
	var out bytes.Buffer
	if err := runOpen(context.Background(), &out, openOptions{}); err != nil {
		t.Fatalf("open: %v\n%s", err, out.String())
	}
	if len(*opened) != 1 || !strings.HasPrefix((*opened)[0], base+"/pair/") {
		t.Fatalf("opened %v", *opened)
	}
	// The link signs the browser in once, from this machine.
	code := strings.TrimPrefix((*opened)[0], base+"/pair/")
	redeem := func() int {
		res, err := http.Post(base+"/api/auth/pair", "application/json", strings.NewReader(`{"code":"`+code+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := redeem(); got != http.StatusOK {
		t.Fatalf("redeem: %d", got)
	}
	if got := redeem(); got != http.StatusUnauthorized {
		t.Fatalf("second redeem: %d", got)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "logs", "open.log")); !strings.Contains(string(log), "opening Syre at") {
		t.Fatalf("open.log: %q", log)
	}
}

func TestOpenNoBrowserPrintsLink(t *testing.T) {
	dir := isolateConfig(t)
	base := localAuthHub(t, dir)
	opened := stubOpen(t, nil)
	var out bytes.Buffer
	if err := runOpen(context.Background(), &out, openOptions{noBrowser: true}); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 || !strings.Contains(out.String(), base+"/pair/") {
		t.Fatalf("opened %v, out %q", *opened, out.String())
	}
}

func TestOpenStartsHubWhenDown(t *testing.T) {
	dir := isolateConfig(t)
	// A dead hub recorded from an earlier run: open starts one where it ran before.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	hubDir := filepath.Join(dir, "oldhub")
	if err := recordLocalHub(strings.TrimPrefix(deadURL, "http://"), filepath.Join(hubDir, tokensFileName)); err != nil {
		t.Fatal(err)
	}
	var startedIn string
	opened := stubOpen(t, func(d, logFile string) error {
		startedIn = d
		localAuthHub(t, dir) // the "hub" comes up and records itself
		return nil
	})
	var out bytes.Buffer
	if err := runOpen(context.Background(), &out, openOptions{}); err != nil {
		t.Fatalf("open: %v\n%s", err, out.String())
	}
	if startedIn != hubDir {
		t.Fatalf("hub started in %q, want %q", startedIn, hubDir)
	}
	if len(*opened) != 1 || !strings.Contains((*opened)[0], "/pair/") {
		t.Fatalf("opened %v", *opened)
	}
}
