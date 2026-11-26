// Package tokenbudget centralizes token estimation and CI token-budget limits
// from docs/design/autopilot.md ("Token budgets as tests").
package tokenbudget

import "testing"

// Hard token budgets enforced in CI.
const (
	Packet        = 300
	Digest        = 250
	Pulse         = 120
	ToolSchemas   = 2500
	FleetStatus20 = 400
)

// Estimate returns a deterministic ceiling(len(s)/4) token approximation.
func Estimate(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}

// Assert fails the test if payload exceeds budget tokens, reporting the measured
// size and the budget.
func Assert(t testing.TB, name, payload string, budget int) int {
	t.Helper()
	got := Estimate(payload)
	if got > budget {
		t.Fatalf("token budget %q exceeded: measured %d tokens (%d bytes), budget <= %d", name, got, len(payload), budget)
	}
	t.Logf("token budget %q: %d / %d tokens (%d bytes)", name, got, budget, len(payload))
	return got
}
