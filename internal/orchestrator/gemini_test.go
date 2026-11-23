package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
)

const geminiTestKey = "AIzaSyTESTKEY0123456789abcdefghij"

func TestGeminiKeyInHeaderAndModelEscaped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("query string must be empty, got %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("x-goog-api-key"); got != geminiTestKey {
			t.Errorf("x-goog-api-key = %q", got)
		}
		if !strings.HasSuffix(r.URL.EscapedPath(), "/models/odd%2Fmodel:generateContent") {
			t.Errorf("model not path-escaped: %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer srv.Close()

	p := NewGeminiProvider(geminiTestKey, srv.URL)
	msg, err := p.Complete(context.Background(), "odd/model", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil || msg.Content != "ok" {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
}

func TestGeminiErrorsNeverContainKey(t *testing.T) {
	// 1. Network failure: *url.Error embeds the request URL.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	p := NewGeminiProvider(geminiTestKey, deadURL)
	p.retry.MaxAttempts = 1
	_, err := p.Complete(context.Background(), "gemini-x", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatal("expected network error")
	}
	if strings.Contains(err.Error(), geminiTestKey) {
		t.Fatalf("network error leaks key: %v", err)
	}

	// 2. A server that echoes the key back in its error body, surfaced through
	// the Service: both OnError and the returned error must be scrubbed.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"API key not valid: ` + r.Header.Get("x-goog-api-key") + ` (url ?key=` + r.Header.Get("x-goog-api-key") + `)"}}`))
	}))
	defer echo.Close()
	svc, _, _ := newTestService(t, config.OrchestratorConfig{Provider: "gemini", APIKey: geminiTestKey, BaseURL: echo.URL, Model: "gemini-x"})
	var reported string
	err = svc.HandleUserMessage(context.Background(), "s1", "hi", StreamCallbacks{
		OnError: func(_, msg string) { reported = msg },
	})
	if err == nil || reported == "" {
		t.Fatalf("expected error, got err=%v reported=%q", err, reported)
	}
	for _, s := range []string{err.Error(), reported} {
		if strings.Contains(s, geminiTestKey) || strings.Contains(s, "AIza") {
			t.Fatalf("error leaks key: %q", s)
		}
		if !strings.Contains(s, "[REDACTED]") {
			t.Fatalf("expected redaction marker in %q", s)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := map[string]string{
		"https://x/y?key=abc123&alt=sse":       "https://x/y?key=[REDACTED]&alt=sse",
		"Authorization: Bearer tok.en-123":     "Authorization: [REDACTED] [REDACTED]",
		"got sk-proj-abcdefghijkl back":        "got [REDACTED] back",
		`{"x-api-key": "ant-secret-value"}`:    `{"x-api-key": "[REDACTED]"}`,
		"plain message with nothing sensitive": "plain message with nothing sensitive",
	}
	for in, want := range cases {
		if got := RedactSecrets(in); got != want {
			t.Errorf("RedactSecrets(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RedactSecrets("my custom-secret-xyz leaked", "custom-secret-xyz"); got != "my [REDACTED] leaked" {
		t.Errorf("explicit secret not redacted: %q", got)
	}
}
