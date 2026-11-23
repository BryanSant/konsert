package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
)

func TestValidateProvider(t *testing.T) {
	for _, ok := range []string{"", "openai", "Anthropic", " gemini "} {
		if err := ValidateProvider(ok); err != nil {
			t.Errorf("ValidateProvider(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"antropic", "ollama", "gpt"} {
		err := ValidateProvider(bad)
		if !errors.Is(err, ErrUnknownProvider) || !strings.Contains(err.Error(), bad) {
			t.Errorf("ValidateProvider(%q) = %v, want ErrUnknownProvider naming it", bad, err)
		}
	}
}

func TestUnknownProviderNeverFallsBackToOpenAI(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(okSSE))
	}))
	defer srv.Close()

	svc, _, _ := newTestService(t, config.OrchestratorConfig{Provider: "antropic", BaseURL: srv.URL, APIKey: "sk-secret-key-123456"})
	if !errors.Is(svc.ProviderError(), ErrUnknownProvider) {
		t.Fatalf("ProviderError = %v", svc.ProviderError())
	}
	var reported string
	err := svc.HandleUserMessage(context.Background(), "s1", "hi", StreamCallbacks{OnError: func(_, m string) { reported = m }})
	if !errors.Is(err, ErrUnknownProvider) || !strings.Contains(reported, "antropic") {
		t.Fatalf("expected unknown-provider error, got err=%v reported=%q", err, reported)
	}
	if hits.Load() != 0 {
		t.Fatalf("no request may be sent for an unknown provider, got %d", hits.Load())
	}

	// Hot reload: a valid provider clears the error, an invalid one sets it again.
	if err := svc.UpdateConfig(config.OrchestratorConfig{Provider: "openai", BaseURL: srv.URL, APIKey: "k"}); err != nil {
		t.Fatalf("UpdateConfig(openai) = %v", err)
	}
	if err := svc.HandleUserMessage(context.Background(), "s1", "hi", StreamCallbacks{}); err != nil || hits.Load() != 1 {
		t.Fatalf("valid provider should work: err=%v hits=%d", err, hits.Load())
	}
	if err := svc.UpdateConfig(config.OrchestratorConfig{Provider: "geminii", BaseURL: srv.URL, APIKey: "k"}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("UpdateConfig(geminii) = %v", err)
	}
	if err := svc.HandleUserMessage(context.Background(), "s1", "hi", StreamCallbacks{}); !errors.Is(err, ErrUnknownProvider) || hits.Load() != 1 {
		t.Fatalf("expected unknown-provider error after reload, got %v (hits=%d)", err, hits.Load())
	}
}
