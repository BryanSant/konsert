package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type recordedReq struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

// fakeController records requests and serves canned responses per "METHOD /path".
type fakeController struct {
	t        *testing.T
	mu       sync.Mutex
	reqs     []recordedReq
	handlers map[string]http.HandlerFunc
	ts       *httptest.Server
}

func newFakeController(t *testing.T) *fakeController {
	t.Helper()
	fc := &fakeController{t: t, handlers: map[string]http.HandlerFunc{}}
	fc.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fc.mu.Lock()
		fc.reqs = append(fc.reqs, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		h := fc.handlers[r.Method+" "+r.URL.Path]
		fc.mu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no route"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(fc.ts.Close)
	t.Setenv("KONSERT_URL", fc.ts.URL)
	t.Setenv("KONSERT_ADMIN_TOKEN", "admin-secret")
	return fc
}

func (fc *fakeController) handle(route string, h http.HandlerFunc) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.handlers[route] = h
}

func (fc *fakeController) requests() []recordedReq {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return append([]recordedReq(nil), fc.reqs...)
}

func jsonReply(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.MarshalWrite(w, v)
	}
}

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return out.String(), err
}

func assertMutationHeaders(t *testing.T, r recordedReq) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.Header.Get("X-Konsert-Request"); got != "1" {
		t.Errorf("X-Konsert-Request = %q", got)
	}
}

func TestSteerCmd(t *testing.T) {
	fc := newFakeController(t)
	fc.handle("POST /api/tasks/t-1/steer", jsonReply(200, map[string]any{"ok": true, "task_id": "t-1", "status": "running"}))

	out, err := runCmd(t, newSteerCmd(), "t-1", "add", "tests")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sent input to task t-1 (status: running)") {
		t.Errorf("output = %q", out)
	}
	reqs := fc.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	assertMutationHeaders(t, reqs[0])
	var body map[string]any
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["input_text"] != "add tests" || body["append_newline"] != true {
		t.Errorf("body = %v", body)
	}
}

func TestSteerCmdNoNewlineAndStdin(t *testing.T) {
	fc := newFakeController(t)
	fc.handle("POST /api/tasks/t-2/steer", jsonReply(200, map[string]any{"ok": true, "task_id": "t-2", "status": "running"}))

	cmd := newSteerCmd()
	cmd.SetIn(strings.NewReader("y"))
	if _, err := runCmd(t, cmd, "--no-newline", "--token", "flag-tok", "t-2", "-"); err != nil {
		t.Fatal(err)
	}
	r := fc.requests()[0]
	if got := r.Header.Get("Authorization"); got != "Bearer flag-tok" {
		t.Errorf("--token not used: %q", got)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(r.Body), &body)
	if body["input_text"] != "y" || body["append_newline"] != false {
		t.Errorf("body = %v", body)
	}
}

func TestCancelCmd(t *testing.T) {
	fc := newFakeController(t)
	fc.handle("POST /api/tasks/t-3/cancel", jsonReply(200, map[string]any{"ok": true, "task_id": "t-3", "status": "canceled"}))

	out, err := runCmd(t, newCancelCmd(), "t-3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cancel requested for task t-3 (status: canceled)") {
		t.Errorf("output = %q", out)
	}
	assertMutationHeaders(t, fc.requests()[0])
}

func TestCmdErrorMessages(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "KONSERT_ADMIN_TOKEN"},
		{403, "forbidden"},
		{404, "task not found (HTTP 404)"},
		{413, "too large"},
		{415, "content type"},
		{429, "rate limited"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			fc := newFakeController(t)
			fc.handle("POST /api/tasks/t-4/cancel", jsonReply(tc.status, map[string]string{"error": "task not found"}))
			_, err := runCmd(t, newCancelCmd(), "t-4")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLogsCmd(t *testing.T) {
	fc := newFakeController(t)
	fc.handle("GET /api/tasks/t-5", jsonReply(200, map[string]any{"task_id": "t-5", "status": "completed"}))
	fc.handle("GET /api/tasks/t-5/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after_id") == "0" {
			jsonReply(200, map[string]any{"events": []map[string]any{
				{"id": 1, "task_id": "t-5", "kind": "milestone", "message": "cloning", "created_at": time.Now()},
				{"id": 2, "task_id": "t-5", "kind": "exit", "message": "exit 0", "created_at": time.Now()},
			}})(w, r)
			return
		}
		jsonReply(200, map[string]any{"events": []any{}})(w, r)
	})

	out, err := runCmd(t, newLogsCmd(), "t-5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "milestone") || !strings.Contains(out, "cloning") || !strings.Contains(out, "exit 0") {
		t.Errorf("output = %q", out)
	}
	for _, r := range fc.requests() {
		if r.Header.Get("Authorization") != "Bearer admin-secret" {
			t.Errorf("%s %s missing bearer token", r.Method, r.Path)
		}
	}
}

func TestLogsCmdFollowPollsWithAfterID(t *testing.T) {
	fc := newFakeController(t)
	var mu sync.Mutex
	polls := 0
	fc.handle("GET /api/tasks/t-6", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		status := "running"
		if polls >= 2 {
			status = "completed"
		}
		jsonReply(200, map[string]any{"task_id": "t-6", "status": status})(w, r)
	})
	fc.handle("GET /api/tasks/t-6/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		after := r.URL.Query().Get("after_id")
		var events []map[string]any
		switch {
		case after == "0":
			events = []map[string]any{{"id": 1, "task_id": "t-6", "kind": "milestone", "message": "first", "created_at": time.Now()}}
		case after == "1" && polls == 1:
			events = []map[string]any{{"id": 2, "task_id": "t-6", "kind": "exit", "message": "second", "created_at": time.Now()}}
		}
		if events == nil {
			polls++
			events = []map[string]any{}
		}
		jsonReply(200, map[string]any{"events": events})(w, r)
	})

	out, err := runCmd(t, newLogsCmd(), "-f", "--interval", "10ms", "t-6")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") || !strings.Contains(out, "task t-6 completed") {
		t.Errorf("output = %q", out)
	}
	sawAfter2 := false
	for _, r := range fc.requests() {
		if r.Path == "/api/tasks/t-6/events" && strings.Contains(r.Query, "after_id=2") {
			sawAfter2 = true
		}
	}
	if !sawAfter2 {
		t.Error("follow never polled with after_id=2")
	}
}

func TestStatusCmdShowsVersionsAndMismatch(t *testing.T) {
	fc := newFakeController(t)
	fc.handle("GET /api/fleet", jsonReply(200, map[string]any{
		"controller_version": "1.4.0",
		"workers": []map[string]any{
			{"id": "w-same", "name": "same", "status": "online", "version": "v1.4.0"},
			{"id": "w-old", "name": "old", "status": "online", "version": "1.3.2"},
			{"id": "w-dev", "name": "dev", "status": "online", "version": "dev"},
			{"id": "w-none", "name": "none", "status": "offline"},
		},
	}))

	out, err := runCmd(t, newStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Controller version: 1.4.0") {
		t.Errorf("missing controller version: %q", out)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	checks := map[string]struct {
		want     string
		mismatch bool
	}{
		"w-same": {"v1.4.0", false},
		"w-old":  {"1.3.2 (mismatch)", true},
		"w-dev":  {"dev", false},
		"w-none": {"unknown", false},
	}
	for id, c := range checks {
		l := lines[id]
		if !strings.Contains(l, c.want) || strings.Contains(l, "mismatch") != c.mismatch {
			t.Errorf("%s line = %q, want %q (mismatch=%v)", id, l, c.want, c.mismatch)
		}
	}
	if !strings.Contains(out, "warning: 1 worker(s)") {
		t.Errorf("missing mismatch warning: %q", out)
	}
	if r := fc.requests()[0]; r.Header.Get("Authorization") != "Bearer admin-secret" {
		t.Errorf("status missing bearer token")
	}
}

func TestStatusCmdDevControllerNeverMismatches(t *testing.T) {
	fc := newFakeController(t)
	fc.handle("GET /api/fleet", jsonReply(200, map[string]any{
		"controller_version": "dev",
		"workers":            []map[string]any{{"id": "w-1", "name": "one", "status": "online", "version": "1.0.0"}},
	}))
	out, err := runCmd(t, newStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "mismatch") || strings.Contains(out, "warning") {
		t.Errorf("dev controller flagged a mismatch: %q", out)
	}
}
