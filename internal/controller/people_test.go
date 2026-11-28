package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDisplayNamesDetectedAndOverridden(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	login := hubLogin()
	if login == "" {
		t.Skip("no current user")
	}
	old := detectNames
	detectNames = func(_ context.Context, l string) string {
		if l == login {
			return "Bryan Sant"
		}
		return ""
	}
	t.Cleanup(func() { detectNames = old })
	runCtx, stop := context.WithCancel(ctx)
	go cs.h.peopleLoop(runCtx)
	t.Cleanup(stop)
	waitFor(t, 3*time.Second, "name detected", func() bool { return cs.h.displayName(ctx, login) == "Bryan Sant" })

	_, raw := cs.api(t, http.MethodGet, "/api/people", nil)
	got := decodeInto[struct {
		People []personView `json:"people"`
	}](t, raw)
	if len(got.People) != 1 || got.People[0].Login != login || got.People[0].DisplayName != "Bryan Sant" || got.People[0].Override != "" {
		t.Fatalf("people = %s", raw)
	}
	// The user's own name wins over detection; clearing it falls back.
	if err := cs.st.SetSettings(ctx, map[string]string{displayNameSettingPrefix + login: "Bryan"}); err != nil {
		t.Fatal(err)
	}
	if n := cs.h.displayName(ctx, login); n != "Bryan" {
		t.Fatalf("override: %q", n)
	}
	if err := cs.st.SetSettings(ctx, map[string]string{displayNameSettingPrefix + login: ""}); err != nil {
		t.Fatal(err)
	}
	if n := cs.h.displayName(ctx, login); n != "Bryan Sant" {
		t.Fatalf("after clearing: %q", n)
	}
	// "Your name" in Settings names the hub's owner, from any device.
	if code, raw := cs.api(t, http.MethodPut, "/api/people/me", map[string]string{"display_name": "  Bryan   S. "}); code != http.StatusOK ||
		decodeInto[personView](t, raw).DisplayName != "Bryan S." {
		t.Fatalf("set my name: %d %s", code, raw)
	}
	if n := cs.h.displayName(ctx, "local"); n != "Bryan S." {
		t.Fatalf("local is the owner: %q", n)
	}
	if code, _ := cs.api(t, http.MethodPut, "/api/people/me", map[string]string{"display_name": strings.Repeat("x", 81)}); code != http.StatusBadRequest {
		t.Fatalf("long name accepted: %d", code)
	}
	if n := cs.h.displayName(ctx, "agent/claude"); n != "" {
		t.Fatalf("agents keep their labels: %q", n)
	}
}
