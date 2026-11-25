package chat

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
)

type countingEngine struct {
	agent string
	calls atomic.Int32
	block chan struct{} // when set, Probe ignores ctx and waits on it
	usage []protocol.AgentRateLimit
}

func (e *countingEngine) Agent() string { return e.agent }
func (e *countingEngine) Probe(_ context.Context, a Account) protocol.AgentAccount {
	e.calls.Add(1)
	if e.block != nil {
		<-e.block
	}
	// CheckedAt is left zero so Capabilities stamps it with its (possibly fake) clock;
	// a wall-clock stamp made TestCapabilitiesUsageOverlay depend on the real date.
	return protocol.AgentAccount{ID: "ignored", Auth: protocol.AgentAuthStatus{State: protocol.AuthOK}, Usage: e.usage,
		Models: []protocol.AgentModel{{ID: "m1", DisplayName: "M1"}}}
}
func (e *countingEngine) Open(context.Context, Account, protocol.AgentChatOpen, Emit) (Session, error) {
	return nil, nil
}

func TestCapabilitiesReport(t *testing.T) {
	home := isolateHome(t)
	clock := newFakeClock()
	accounts := NewAccountRegistry(AccountOptions{
		HomeDir: home, DisableWrappers: true, LookPath: fakeBins("/bin", "claude", "grok"),
		Configured: []config.AgentAccountConfig{{ID: "claude2", Agent: "claude", Label: "Claude 2", TokenFile: "~/missing"}},
	})
	claude := &countingEngine{agent: "claude"}
	statusCalls := atomic.Int32{}
	caps := NewCapabilities(CapabilitiesOptions{
		Accounts: accounts,
		Engine: func(agent string) (Engine, bool) {
			if agent == "claude" {
				return claude, true
			}
			return nil, false
		},
		Status: func(_ context.Context, a Account) protocol.AgentAccount {
			statusCalls.Add(1)
			return protocol.AgentAccount{Auth: protocol.AgentAuthStatus{State: protocol.AuthLoggedOut}}
		},
		Now: clock.Now,
	})
	rep := caps.Report(context.Background(), false)
	if len(rep.Accounts) != 3 {
		t.Fatalf("accounts = %+v", rep.Accounts)
	}
	byID := map[string]protocol.AgentAccount{}
	for _, a := range rep.Accounts {
		byID[a.ID] = a
	}
	if a := byID["claude"]; !a.ChatSupported || a.Auth.State != protocol.AuthOK || a.Label != "Claude" || !a.IsDefault || len(a.Models) != 1 {
		t.Fatalf("claude = %+v", a)
	}
	if a := byID["grok"]; a.ChatSupported || a.Auth.State != protocol.AuthLoggedOut || a.Agent != "grok" {
		t.Fatalf("grok = %+v", a)
	}
	if a := byID["claude2"]; a.Auth.State != protocol.AuthLoggedOut || a.Auth.Detail == "" || a.ChatSupported {
		t.Fatalf("claude2 (missing token file) = %+v", a)
	}

	// Cached within the TTL; re-probed after it or when forced.
	caps.Report(context.Background(), false)
	if claude.calls.Load() != 1 || statusCalls.Load() != 1 {
		t.Fatalf("not cached: engine=%d status=%d", claude.calls.Load(), statusCalls.Load())
	}
	clock.Advance(DefaultProbeTTL + time.Second)
	caps.Report(context.Background(), false)
	caps.Report(context.Background(), true)
	if claude.calls.Load() != 3 {
		t.Fatalf("engine calls = %d", claude.calls.Load())
	}

	// Failure-triggered invalidation is rate limited per account.
	if !caps.Invalidate("claude") || caps.Invalidate("claude") {
		t.Fatal("Invalidate rate limit")
	}
	clock.Advance(DefaultForceInterval + time.Second)
	if !caps.Invalidate("claude") {
		t.Fatal("Invalidate after interval")
	}
}

func TestCapabilitiesProbeTimeout(t *testing.T) {
	home := isolateHome(t)
	accounts := NewAccountRegistry(AccountOptions{HomeDir: home, DisableWrappers: true, LookPath: fakeBins("/bin", "claude")})
	eng := &countingEngine{agent: "claude", block: make(chan struct{})}
	defer close(eng.block)
	caps := NewCapabilities(CapabilitiesOptions{
		Accounts: accounts, Timeout: 50 * time.Millisecond,
		Engine: func(string) (Engine, bool) { return eng, true },
	})
	start := time.Now()
	rep := caps.Report(context.Background(), false)
	if time.Since(start) > 5*time.Second {
		t.Fatal("probe timeout not enforced")
	}
	if rep.Accounts[0].Auth.State != protocol.AuthUnknown || rep.Accounts[0].Auth.Detail != "capability check timed out" {
		t.Fatalf("timed out account = %+v", rep.Accounts[0])
	}
}

func TestCapabilitiesUsageOverlay(t *testing.T) {
	home := isolateHome(t)
	clock := newFakeClock()
	accounts := NewAccountRegistry(AccountOptions{HomeDir: home, DisableWrappers: true, LookPath: fakeBins("/bin", "claude")})
	reset := clock.Now().Add(3 * time.Hour)
	eng := &countingEngine{agent: "claude", usage: []protocol.AgentRateLimit{{Window: "seven_day", UsedPercent: 60, State: "ok"}}}
	caps := NewCapabilities(CapabilitiesOptions{
		Accounts: accounts, Now: clock.Now,
		Engine: func(string) (Engine, bool) { return eng, true },
	})
	caps.Report(context.Background(), false)

	rl := protocol.AgentRateLimit{Window: "seven_day", State: "exhausted", ResetsAt: &reset}
	if !caps.NoteRateLimit("claude", rl) {
		t.Fatal("exhaustion not reported as a change")
	}
	if caps.NoteRateLimit("claude", rl) {
		t.Fatal("repeat reported as a change")
	}
	if until, ok := caps.Exhausted("claude"); !ok || !until.Equal(reset) {
		t.Fatalf("Exhausted = %v %v", until, ok)
	}
	check := func(rep protocol.AgentCapabilitiesReport, state string) {
		t.Helper()
		u := rep.Accounts[0].Usage
		if len(u) != 1 || u[0].State != state {
			t.Fatalf("usage = %+v, want state %s", u, state)
		}
	}
	got := caps.Cached()
	check(got, "exhausted")
	if got.Accounts[0].Usage[0].UsedPercent != 100 || !got.Accounts[0].Usage[0].ResetsAt.Equal(reset) {
		t.Fatalf("exhausted window = %+v", got.Accounts[0].Usage[0])
	}
	// A (lagging) probe does not clear it before the reset time...
	clock.Advance(time.Hour)
	check(caps.Report(context.Background(), true), "exhausted")
	// ...but after the reset the probe is trusted again.
	clock.Advance(3 * time.Hour)
	check(caps.Report(context.Background(), true), "ok")
	if _, ok := caps.Exhausted("claude"); ok {
		t.Fatal("still exhausted after reset")
	}

	// A non-exhausted observation newer than the probe is shown; older ones are not.
	clock.Advance(time.Second)
	caps.NoteRateLimit("claude", protocol.AgentRateLimit{Window: "five_hour", UsedPercent: 30, State: "ok"})
	if u := caps.Cached().Accounts[0].Usage; len(u) != 2 {
		t.Fatalf("usage = %+v", u)
	}
}
