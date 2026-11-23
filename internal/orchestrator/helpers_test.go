package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

// fakeExecutor records tool executions.
type fakeExecutor struct {
	mu    sync.Mutex
	calls []string
	args  []string
}

func (f *fakeExecutor) ToolDefinitions() []ToolDef {
	return []ToolDef{{Name: "fleet_status", Description: "status", Parameters: map[string]any{"type": "object"}}}
}

func (f *fakeExecutor) ExecuteTool(_ context.Context, name string, args json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	f.args = append(f.args, string(args))
	return map[string]string{"ok": "yes"}, nil
}

// scriptedProvider returns canned replies in order.
type scriptedProvider struct {
	mu      sync.Mutex
	replies []Message
	errs    []error
	seen    [][]Message
}

func (p *scriptedProvider) Name() string { return "openai" }

func (p *scriptedProvider) Complete(_ context.Context, _ string, _ string, msgs []Message, _ []ToolDef, onDelta func(string)) (Message, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := append([]Message(nil), msgs...)
	p.seen = append(p.seen, cp)
	i := len(p.seen) - 1
	if i < len(p.errs) && p.errs[i] != nil {
		return Message{}, p.errs[i]
	}
	if i >= len(p.replies) {
		return p.replies[len(p.replies)-1], nil
	}
	if onDelta != nil && p.replies[i].Content != "" {
		onDelta(p.replies[i].Content)
	}
	return p.replies[i], nil
}

func newTestService(t *testing.T, cfg config.OrchestratorConfig) (*Service, store.Store, *fakeExecutor) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ex := &fakeExecutor{}
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "sk-test-key-123456"
	}
	return NewService(cfg, st, ex), st, ex
}
