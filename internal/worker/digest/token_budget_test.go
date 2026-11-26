package digest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/tokenbudget"
)

func TestDigestBuildTokenBudgets(t *testing.T) {
	var longFailureOut strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&longFailureOut, "--- FAIL: TestBillingRotationScenario%02d (0.42s)\n", i)
		fmt.Fprintf(&longFailureOut, "    \x1b[31mhandler_test.go:%d: expected HTTP 401 on reused refresh token, got 200 OK\x1b[0m\n", 100+i)
	}
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&longFailureOut, "\x1b[33mstderr[%02d]: timeout waiting for distributed lock on family_id=fam-%04d in api/pkg/auth/store.go\x1b[0m\n", i, i)
	}
	longFailureOut.WriteString("FAIL\tgithub.com/acme/billing/pkg/auth\t4.210s\n")

	var longQuestionOut strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&longQuestionOut, "line %02d: inspecting migration schema and backwards compatibility constraints for /v1/token\n", i)
	}

	cases := []struct {
		name  string
		input Input
	}{
		{
			name: "failed_digest_with_long_tail",
			input: Input{
				TicketSID:  "SYR-7QK9Z",
				TaskStatus: protocol.TaskStatusFailed,
				Machine:    "build-linux-01",
				Agent:      "claude",
				Model:      "claude-opus-4-6",
				Duration:   615 * time.Second,
				ExitCode:   1,
				Summary:    "go test failed across 12 refresh-token rotation tests due to lock contention and stale family state",
				Output:     []byte(longFailureOut.String()),
				Git: map[string]*protocol.GitSummary{
					"api":    {DiffStat: " 6 files changed, 240 insertions(+), 38 deletions(-)"},
					"web":    {DiffStat: " 3 files changed, 95 insertions(+), 14 deletions(-)"},
					"worker": {DiffStat: " 2 files changed, 44 insertions(+), 7 deletions(-)"},
				},
			},
		},
		{
			name: "multi_repo_pass_with_interface_and_followups",
			input: Input{
				TicketSID:  "SYR-7QK3M",
				TaskStatus: protocol.TaskStatusCompleted,
				Machine:    "bocabox",
				Agent:      "claude",
				Model:      "claude-opus-4-6",
				Duration:   412 * time.Second,
				Summary:    "Implemented refresh token rotation with family reuse detection and single 401 retry across api, web, and sdk.",
				FinalMessage: "Completed implementation.\n" +
					"Interface: RefreshHandler rotates token families and returns 401 on reuse; api/pkg/auth/handler.go\n" +
					"Followup: Legacy /v1/token endpoint still issues static refresh tokens\n" +
					"Followup: Add dashboard alert for elevated family revocation rates\n",
				Git: map[string]*protocol.GitSummary{
					"api": {DiffStat: " 5 files changed, 184 insertions(+), 29 deletions(-)"},
					"web": {DiffStat: " 3 files changed, 76 insertions(+), 12 deletions(-)"},
					"sdk": {DiffStat: " 2 files changed, 52 insertions(+), 4 deletions(-)"},
				},
			},
		},
		{
			name: "needs_you_with_long_context",
			input: Input{
				TicketSID:  "SYR-7QK4P",
				TaskStatus: protocol.TaskStatusWaitingInput,
				Machine:    "mini-mac",
				Agent:      "codex",
				Model:      "gpt-5",
				Duration:   180 * time.Second,
				Summary:    "waiting for operator decision on legacy token endpoint compatibility",
				Question:   "Should legacy /v1/token clients be migrated to rotating refresh tokens immediately or kept on static tokens behind a feature flag?",
				Output:     []byte(longQuestionOut.String()),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Build(tc.input)
			got := d.Text()
			if strings.TrimSpace(got) == "" {
				t.Fatalf("%s produced empty digest", tc.name)
			}
			tokenbudget.Assert(t, tc.name, got, tokenbudget.Digest)
		})
	}
}
