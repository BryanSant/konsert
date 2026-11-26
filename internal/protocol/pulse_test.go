package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func TestBoardPulseGolden(t *testing.T) {
	cases := []struct {
		name  string
		pulse BoardPulse
		want  string
	}{
		{
			name: "design_example",
			pulse: BoardPulse{
				AutopilotState: "on",
				Time:           "14:32",
				FleetUp:        4,
				FleetTotal:     5,
				FleetNotable:   []string{"mini-mac throttled max=1 until 18:00", "win-desk paused until 18:00"},
				Ready:          12,
				Dispatchable:   5,
				Running:        4,
				NeedsYou:       1,
				NeedsYouSIDs:   []string{"SYR-7QK9P"},
				Review:         3,
				Triggers: []PulseTrigger{
					{
						Kind:          PulseTriggerReview,
						TicketSID:     "SYR-7QK3M",
						UnblockedSIDs: []string{"SYR-7QK4B", "SYR-7QK4C"},
						Followups:     1,
					},
				},
			},
			want: strings.Join([]string{
				"[SYRE PULSE | autopilot on | 14:32]",
				"Fleet: 4/5 up (mini-mac throttled max=1 until 18:00; win-desk paused until 18:00)",
				"Board: 12 ready (5 dispatchable) · 4 running · 1 needs you (SYR-7QK9P) · 3 review",
				"Trigger: SYR-7QK3M → review (unblocked SYR-7QK4B, SYR-7QK4C); 1 followup",
			}, "\n"),
		},
		{
			name: "batched_failed_needs_you_and_decomposition",
			pulse: BoardPulse{
				AutopilotState: "on",
				Time:           "09:15",
				FleetUp:        2,
				FleetTotal:     2,
				FleetNotable:   []string{"box-2 drained"},
				Ready:          4,
				Dispatchable:   2,
				Running:        1,
				NeedsYou:       4,
				NeedsYouSIDs:   []string{"SYR-A1", "SYR-A2", "SYR-A3", "SYR-A4"},
				Review:         1,
				Triggers: []PulseTrigger{
					{Kind: PulseTriggerFailed, TicketSID: "SYR-F1", Category: ErrKindVerify},
					{Kind: PulseTriggerNeedsYou, TicketSID: "SYR-A1", Category: "question"},
					{Kind: PulseTriggerNeedsDecomposition, TicketSIDs: []string{"SYR-D1", "SYR-D2"}},
				},
			},
			want: strings.Join([]string{
				"[SYRE PULSE | autopilot on | 09:15]",
				"Fleet: 2/2 up (box-2 drained)",
				"Board: 4 ready (2 dispatchable) · 1 running · 4 needs you (SYR-A1, SYR-A2, SYR-A3) · 1 review",
				"Trigger: SYR-F1 → failed (verify); SYR-A1 → needs you (question); needs decomposition: SYR-D1, SYR-D2",
			}, "\n"),
		},
		{
			name: "board_idle_and_directive_expiring",
			pulse: BoardPulse{
				AutopilotState: "paused",
				Time:           "18:00",
				FleetUp:        3,
				FleetTotal:     3,
				Ready:          0,
				Dispatchable:   0,
				Running:        0,
				NeedsYou:       0,
				Review:         2,
				Triggers: []PulseTrigger{
					{Kind: PulseTriggerDirectiveExpiring, Summary: "override ov-9X1 expired"},
					{Kind: PulseTriggerBoardIdle, Project: "billing"},
				},
			},
			want: strings.Join([]string{
				"[SYRE PULSE | autopilot paused | 18:00]",
				"Fleet: 3/3 up",
				"Board: 0 ready · 0 running · 0 needs you · 2 review",
				"Trigger: override ov-9X1 expired; billing board idle",
			}, "\n"),
		},
		{
			name: "needs_you_with_permission_excerpt",
			pulse: BoardPulse{
				AutopilotState: "on",
				Time:           "14:32",
				FleetUp:        2,
				FleetTotal:     2,
				Ready:          3,
				Dispatchable:   1,
				Running:        1,
				NeedsYou:       1,
				NeedsYouSIDs:   []string{"tk-ka8r5"},
				Review:         0,
				Triggers: []PulseTrigger{
					{
						Kind:      PulseTriggerNeedsYou,
						TicketSID: "tk-ka8r5",
						Category:  "question",
						Summary:   "permission: run ls -la",
					},
				},
			},
			want: strings.Join([]string{
				"[SYRE PULSE | autopilot on | 14:32]",
				"Fleet: 2/2 up",
				"Board: 3 ready (1 dispatchable) · 1 running · 1 needs you (tk-ka8r5) · 0 review",
				"Trigger: tk-ka8r5 → needs you: permission to run ls -la",
			}, "\n"),
		},
		{
			name: "needs_you_long_excerpt_capped_60",
			pulse: BoardPulse{
				AutopilotState: "on",
				Time:           "14:32",
				FleetUp:        1,
				FleetTotal:     1,
				Ready:          0,
				Dispatchable:   0,
				Running:        0,
				NeedsYou:       1,
				NeedsYouSIDs:   []string{"tk-ka8r5"},
				Review:         0,
				Triggers: []PulseTrigger{
					{
						Kind:      PulseTriggerNeedsYou,
						TicketSID: "tk-ka8r5",
						Summary:   "permission: run cd /very/long/path/to/ticket/workdir/demo && ls -la && git status -sb",
					},
				},
			},
			want: strings.Join([]string{
				"[SYRE PULSE | autopilot on | 14:32]",
				"Fleet: 1/1 up",
				"Board: 0 ready · 0 running · 1 needs you (tk-ka8r5) · 0 review",
				"Trigger: tk-ka8r5 → needs you: permission to run cd /very/long/path/to/ticket/workdir/de...",
			}, "\n"),
		},
		{
			name: "snapshot_no_triggers",
			pulse: BoardPulse{
				AutopilotState: "off",
				Time:           "12:00",
				FleetUp:        1,
				FleetTotal:     1,
				Ready:          3,
				Dispatchable:   1,
				Running:        0,
				NeedsYou:       0,
				Review:         0,
			},
			want: strings.Join([]string{
				"[SYRE PULSE | autopilot off | 12:00]",
				"Fleet: 1/1 up",
				"Board: 3 ready (1 dispatchable) · 0 running · 0 needs you · 0 review",
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pulse.Text()
			if got != tc.want {
				t.Fatalf("pulse mismatch:\ngot:\n%s\n\nwant:\n%s", got, tc.want)
			}
			if tok := EstimateTokens(got); tok > PulseMaxTokens {
				t.Fatalf("pulse exceeded %d token cap: got %d tokens\n%s", PulseMaxTokens, tok, got)
			}
		})
	}
}

func TestBoardPulseTokenCapHardLimit(t *testing.T) {
	var notable []string
	var triggers []PulseTrigger
	for i := 1; i <= 15; i++ {
		notable = append(notable, fmt.Sprintf("worker-node-%02d throttled max=1 until 18:00", i))
		triggers = append(triggers, PulseTrigger{
			Kind:          PulseTriggerReview,
			TicketSID:     fmt.Sprintf("SYR-%05d", i),
			UnblockedSIDs: []string{fmt.Sprintf("SYR-U%04dA", i), fmt.Sprintf("SYR-U%04dB", i), fmt.Sprintf("SYR-U%04dC", i)},
			Followups:     3,
		})
	}
	p := BoardPulse{
		AutopilotState: "on",
		Time:           "14:32",
		FleetUp:        15,
		FleetTotal:     20,
		FleetNotable:   notable,
		Ready:          40,
		Dispatchable:   18,
		Running:        12,
		NeedsYou:       8,
		NeedsYouSIDs:   []string{"SYR-N0001", "SYR-N0002", "SYR-N0003", "SYR-N0004"},
		Review:         9,
		Triggers:       triggers,
	}
	got := p.Text()
	if tok := EstimateTokens(got); tok > PulseMaxTokens {
		t.Fatalf("huge pulse exceeded %d token cap: got %d tokens (%d bytes)\n%s", PulseMaxTokens, tok, len(got), got)
	}
	lines := strings.Split(got, "\n")
	if len(lines) < 3 || len(lines) > 4 {
		t.Fatalf("expected 3-4 lines, got %d:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "[SYRE PULSE |") {
		t.Fatalf("missing header: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "Fleet:") {
		t.Fatalf("missing Fleet line: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "Board:") {
		t.Fatalf("missing Board line: %q", lines[2])
	}
}
