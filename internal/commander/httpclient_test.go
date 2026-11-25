package commander

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastRetry() retryPolicy {
	return retryPolicy{MaxAttempts: 4, BaseDelay: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond, MaxRetryAfter: 2 * time.Second}
}

const okSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

func TestNewHTTPClientHasTimeouts(t *testing.T) {
	tr, ok := newHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.IdleConnTimeout <= 0 || tr.TLSHandshakeTimeout <= 0 {
		t.Fatalf("missing timeouts: header=%v idle=%v tls=%v", tr.ResponseHeaderTimeout, tr.IdleConnTimeout, tr.TLSHandshakeTimeout)
	}
	for _, p := range []interface{ Name() string }{NewOpenAIProvider("", ""), NewAnthropicProvider("", ""), NewGeminiProvider("", "")} {
		var o httpOpts
		switch v := p.(type) {
		case *OpenAIProvider:
			o = v.httpOpts
		case *AnthropicProvider:
			o = v.httpOpts
		case *GeminiProvider:
			o = v.httpOpts
		}
		if o.idleTimeout <= 0 || o.retry.MaxAttempts < 2 || o.client.Transport.(*http.Transport).ResponseHeaderTimeout <= 0 {
			t.Fatalf("%s: provider not using bounded client/retry/watchdog: %+v", p.Name(), o)
		}
	}
}

func TestRetryOn429HonoursRetryAfter(t *testing.T) {
	var hits atomic.Int32
	var firstAt, secondAt atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			firstAt.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		secondAt.Store(time.Now().UnixNano())
		_, _ = w.Write([]byte(okSSE))
	}))
	defer srv.Close()
	p := NewOpenAIProvider("k", srv.URL)
	p.retry = fastRetry()
	msg, err := p.Complete(context.Background(), "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil || msg.Content != "ok" {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", hits.Load())
	}
	if gap := time.Duration(secondAt.Load() - firstAt.Load()); gap < 900*time.Millisecond {
		t.Fatalf("Retry-After: 1 not honoured, retried after %v", gap)
	}
}

func TestRetryOn5xxWithBackoffThenGiveUp(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	defer srv.Close()
	p := NewAnthropicProvider("k", srv.URL)
	p.retry = fastRetry()
	_, err := p.Complete(context.Background(), "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected final 503 error, got %v", err)
	}
	if hits.Load() != 4 {
		t.Fatalf("expected 4 attempts, got %d", hits.Load())
	}
}

func TestNoRetryOnClientErrorOrLongRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		status int
		ra     string
	}{{http.StatusBadRequest, ""}, {http.StatusTooManyRequests, "3600"}} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if tc.ra != "" {
				w.Header().Set("Retry-After", tc.ra)
			}
			w.WriteHeader(tc.status)
		}))
		p := NewGeminiProvider("k", srv.URL)
		p.retry = fastRetry()
		start := time.Now()
		_, err := p.Complete(context.Background(), "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
		srv.Close()
		if err == nil || hits.Load() != 1 || time.Since(start) > time.Second {
			t.Fatalf("status %d ra=%q: err=%v hits=%d elapsed=%v", tc.status, tc.ra, err, hits.Load(), time.Since(start))
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d, ok := parseRetryAfter("2.5", now); !ok || d != 2500*time.Millisecond {
		t.Fatalf("seconds: %v %v", d, ok)
	}
	if d, ok := parseRetryAfter(now.Add(7*time.Second).Format(http.TimeFormat), now); !ok || d != 7*time.Second {
		t.Fatalf("http date: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter("soon", now); ok {
		t.Fatal("garbage accepted")
	}
}

func TestStreamIdleWatchdog(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select { // hang mid-stream
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := NewOpenAIProvider("k", srv.URL)
	p.idleTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err := p.Complete(context.Background(), "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("expected ErrStreamIdle, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("watchdog too slow: %v", time.Since(start))
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := NewGeminiProvider("k", srv.URL)
	p.client.Transport.(*http.Transport).ResponseHeaderTimeout = 150 * time.Millisecond
	p.retry = fastRetry()
	start := time.Now()
	_, err := p.Complete(context.Background(), "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("expected header timeout error quickly, got %v after %v", err, time.Since(start))
	}
}

func TestRequestsHonourContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	p := NewAnthropicProvider("k", srv.URL)
	start := time.Now()
	_, err := p.Complete(ctx, "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("expected ctx deadline, got %v after %v", err, time.Since(start))
	}

	// Cancellation during a retry backoff also returns promptly.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	p2 := NewOpenAIProvider("k", srv2.URL)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	start = time.Now()
	_, err = p2.Complete(ctx2, "m", "", []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("expected ctx deadline during backoff, got %v after %v", err, time.Since(start))
	}
}
