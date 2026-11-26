package protocol

import (
	"fmt"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/tokenbudget"
)

func TestProtocolTokenBudgets(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		render func() string
	}{
		{
			name:   "largest_multi_repo_packet_with_contracts",
			budget: tokenbudget.Packet,
			render: func() string {
				var contracts []string
				for i := 1; i <= 8; i++ {
					contracts = append(contracts, fmt.Sprintf(
						"SYR-7QK%02d (done): POST /v2/billing/accounts/%d/rotate returns {access_token, refresh_token, family_id, expires_in}; handler in api/pkg/auth/rotate_%d.go",
						i, i, i,
					))
				}
				pkt := DispatchPacket{
					TicketSID: "SYR-7QK9Z",
					Project:   "billing-platform",
					Repos: []DispatchPacketRepo{
						{Name: "api", Path: "/var/lib/syred/work/.syre/tickets/SYR-7QK9Z/api", Branch: "feat/jwt-refresh-family-rotation"},
						{Name: "web", Path: "/var/lib/syred/work/.syre/tickets/SYR-7QK9Z/web", Branch: "feat/jwt-refresh-family-rotation"},
						{Name: "sdk", Path: "/var/lib/syred/work/.syre/tickets/SYR-7QK9Z/sdk", Branch: "feat/jwt-refresh-family-rotation"},
						{Name: "worker", Path: "/var/lib/syred/work/.syre/tickets/SYR-7QK9Z/worker", Branch: "feat/jwt-refresh-family-rotation"},
					},
					Objective: "Rotate refresh tokens every 15 minutes across api, web, sdk, and background worker; revoke entire token family on reuse detection and retry once on 401.",
					Acceptance: []string{
						"Rotate on /v2/auth/refresh and invalidate the entire token family on reuse.",
						"Web fetch wrapper retries once on 401 after refreshing credentials.",
						"SDK client surfaces structured AuthExpiredError after retry exhaustion.",
						"Worker background refresh avoids stampede with jittered singleflight.",
					},
					Contracts: contracts,
					Verify:    "api: go test ./pkg/auth/...; web: npm --prefix web test; sdk: go test ./...; worker: go test ./...",
					Tools:     []string{"ticket_get", "ticket_check", "ticket_comment", "ticket_followup", "ask_user"},
				}
				return pkt.Text()
			},
		},
		{
			name:   "failed_digest_with_long_tail",
			budget: tokenbudget.Digest,
			render: func() string {
				var tests []string
				for i := 1; i <= 15; i++ {
					tests = append(tests, fmt.Sprintf("TestRefreshTokenFamilyRotationEdgeCase%02d", i))
				}
				var tail []string
				for i := 1; i <= 30; i++ {
					tail = append(tail, fmt.Sprintf("api/pkg/auth/handler_test.go:%d: expected status 401 Unauthorized after reuse detection, got 200 OK (family=fam-%04d)", 100+i, i))
				}
				d := RunDigest{
					TicketSID: "SYR-7QK9Z",
					Attempt:   3,
					Status:    DigestStatusFailed,
					Machine:   "build-linux-01",
					Agent:     "claude",
					Model:     "claude-opus-4-6",
					DurationS: 842,
					Git: map[string]string{
						"api":    "7 files, +312 -48",
						"web":    "4 files, +129 -19",
						"sdk":    "3 files, +88 -12",
						"worker": "2 files, +41 -6",
					},
					Verify: &DigestVerify{
						Cmd:    "api: go test ./pkg/auth/...; web: npm --prefix web test",
						Exit:   1,
						Passed: 128,
						Failed: 15,
					},
					Interface: "RefreshHandler rotates token families; api/pkg/auth/handler.go",
					Followups: []string{
						"Legacy /v1/token endpoint still issues static refresh tokens",
						"Add Prometheus counter for reuse-detection revocations",
					},
					Summary: "Verification failed in api/pkg/auth after 3 attempts: reuse detection returned 200 instead of 401 when concurrent refresh requests raced on the same family.",
					Failure: &DigestFailure{
						Category: ErrKindVerify,
						Tests:    tests,
						Tail:     tail,
					},
				}
				return d.Text()
			},
		},
		{
			name:   "busiest_pulse",
			budget: tokenbudget.Pulse,
			render: func() string {
				var notable []string
				var triggers []PulseTrigger
				for i := 1; i <= 10; i++ {
					notable = append(notable, fmt.Sprintf("node-%02d throttled max=1 until 18:00", i))
					triggers = append(triggers, PulseTrigger{
						Kind:          PulseTriggerReview,
						TicketSID:     fmt.Sprintf("SYR-7Q%03d", i),
						UnblockedSIDs: []string{fmt.Sprintf("SYR-8A%03d", i), fmt.Sprintf("SYR-8B%03d", i)},
						Followups:     2,
					})
				}
				triggers = append(triggers,
					PulseTrigger{Kind: PulseTriggerFailed, TicketSID: "SYR-9F001", Category: ErrKindVerify, Followups: 1},
					PulseTrigger{Kind: PulseTriggerNeedsYou, TicketSID: "SYR-9N001", Category: "question"},
					PulseTrigger{Kind: PulseTriggerNeedsDecomposition, TicketSIDs: []string{"SYR-9D001", "SYR-9D002", "SYR-9D003", "SYR-9D004"}},
					PulseTrigger{Kind: PulseTriggerDirectiveExpiring, Summary: "dir-01 machine.pause win-desk expiring"},
				)
				p := BoardPulse{
					AutopilotState: "on",
					Time:           "14:32",
					FleetUp:        18,
					FleetTotal:     20,
					FleetNotable:   notable,
					Ready:          42,
					Dispatchable:   19,
					Running:        14,
					NeedsYou:       6,
					NeedsYouSIDs:   []string{"SYR-9N001", "SYR-9N002", "SYR-9N003", "SYR-9N004"},
					Review:         11,
					Triggers:       triggers,
				}
				return p.Text()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.render()
			if strings.TrimSpace(got) == "" {
				t.Fatalf("%s rendered empty output", tc.name)
			}
			tokenbudget.Assert(t, tc.name, got, tc.budget)
		})
	}
}
