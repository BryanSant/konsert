package protocol

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PulseMaxTokens is the hard token budget for BoardPulse.Text() (docs/design/autopilot.md).
const PulseMaxTokens = 120

// PulseTriggerKind constants identify actionable board pulse triggers.
const (
	PulseTriggerReview             = "review"
	PulseTriggerFailed             = "failed"
	PulseTriggerNeedsYou           = "needs_you"
	PulseTriggerFollowups          = "followups"
	PulseTriggerNeedsDecomposition = "needs_decomposition"
	PulseTriggerDirectiveExpiring  = "directive_expiring"
	PulseTriggerBoardIdle          = "board_idle"
)

// PulseTrigger describes one actionable change summarized on the Trigger line.
type PulseTrigger struct {
	Kind          string   `json:"kind"`
	TicketSID     string   `json:"ticket_sid,omitempty"`
	TicketSIDs    []string `json:"ticket_sids,omitempty"`
	UnblockedSIDs []string `json:"unblocked_sids,omitempty"`
	Followups     int      `json:"followups,omitzero"`
	Category      string   `json:"category,omitempty"`
	Project       string   `json:"project,omitempty"`
	Summary       string   `json:"summary,omitempty"`
}

// BoardPulse is the compact hub -> Commander status push (<= 120 tokens).
//
// Format (docs/design/autopilot.md):
//
//	[SYRE PULSE | autopilot on | 14:32]
//	Fleet: 4/5 up (mini-mac throttled max=1 until 18:00; win-desk paused until 18:00)
//	Board: 12 ready (5 dispatchable) · 4 running · 1 needs you (SYR-7QK9P) · 3 review
//	Trigger: SYR-7QK3M → review (unblocked SYR-7QK4B, SYR-7QK4C); 1 followup
type BoardPulse struct {
	AutopilotState string         `json:"autopilot_state"` // "on" | "off" | "paused"
	Time           string         `json:"time"`            // "HH:MM"
	FleetUp        int            `json:"fleet_up"`
	FleetTotal     int            `json:"fleet_total"`
	FleetNotable   []string       `json:"fleet_notable,omitempty"`
	Ready          int            `json:"ready"`
	Dispatchable   int            `json:"dispatchable"`
	Running        int            `json:"running"`
	NeedsYou       int            `json:"needs_you"`
	NeedsYouSIDs   []string       `json:"needs_you_sids,omitempty"` // up to 3 SIDs rendered
	Review         int            `json:"review"`
	Triggers       []PulseTrigger `json:"triggers,omitempty"`
}

// Text renders the board pulse in 3-4 compact lines, enforcing PulseMaxTokens (120).
func (p BoardPulse) Text() string {
	c := p.clone()
	if len(c.NeedsYouSIDs) > 3 {
		c.NeedsYouSIDs = c.NeedsYouSIDs[:3]
	}

	omittedTriggers := 0
	omittedNotable := 0

	out := c.renderText(omittedNotable, omittedTriggers)
	if EstimateTokens(out) <= PulseMaxTokens {
		return out
	}

	// 1. Trim unblocked SID lists on triggers first.
	for i := len(c.Triggers) - 1; i >= 0 && EstimateTokens(out) > PulseMaxTokens; i-- {
		for len(c.Triggers[i].UnblockedSIDs) > 0 && EstimateTokens(out) > PulseMaxTokens {
			c.Triggers[i].UnblockedSIDs = c.Triggers[i].UnblockedSIDs[:len(c.Triggers[i].UnblockedSIDs)-1]
			out = c.renderText(omittedNotable, omittedTriggers)
		}
	}
	if EstimateTokens(out) <= PulseMaxTokens {
		return out
	}

	// 2. Trim extra triggers from the end (keeping at least 1).
	for len(c.Triggers) > 1 && EstimateTokens(out) > PulseMaxTokens {
		c.Triggers = c.Triggers[:len(c.Triggers)-1]
		omittedTriggers++
		out = c.renderText(omittedNotable, omittedTriggers)
	}
	if EstimateTokens(out) <= PulseMaxTokens {
		return out
	}

	// 3. Trim notable machine states from the end.
	for len(c.FleetNotable) > 0 && EstimateTokens(out) > PulseMaxTokens {
		c.FleetNotable = c.FleetNotable[:len(c.FleetNotable)-1]
		omittedNotable++
		out = c.renderText(omittedNotable, omittedTriggers)
	}
	if EstimateTokens(out) <= PulseMaxTokens {
		return out
	}

	// 4. Trim NeedsYouSIDs down to 0.
	for len(c.NeedsYouSIDs) > 0 && EstimateTokens(out) > PulseMaxTokens {
		c.NeedsYouSIDs = c.NeedsYouSIDs[:len(c.NeedsYouSIDs)-1]
		out = c.renderText(omittedNotable, omittedTriggers)
	}
	if EstimateTokens(out) <= PulseMaxTokens {
		return out
	}

	// 5. Drop omitted counters if still over budget.
	if omittedNotable > 0 {
		omittedNotable = 0
		out = c.renderText(omittedNotable, omittedTriggers)
	}
	if EstimateTokens(out) <= PulseMaxTokens {
		return out
	}

	// 6. Hard byte cap (preserving valid UTF-8) with ellipsis.
	maxBytes := PulseMaxTokens * 4
	if len(out) > maxBytes {
		const ellipsis = "…"
		limit := maxBytes - len(ellipsis)
		if limit < 0 {
			limit = 0
		}
		out = truncateUTF8Bytes(out, limit) + ellipsis
	}
	return out
}

func (p BoardPulse) clone() BoardPulse {
	c := p
	if len(p.FleetNotable) > 0 {
		c.FleetNotable = append([]string(nil), p.FleetNotable...)
	}
	if len(p.NeedsYouSIDs) > 0 {
		c.NeedsYouSIDs = append([]string(nil), p.NeedsYouSIDs...)
	}
	if len(p.Triggers) > 0 {
		c.Triggers = make([]PulseTrigger, len(p.Triggers))
		for i, tr := range p.Triggers {
			tr.TicketSIDs = append([]string(nil), tr.TicketSIDs...)
			tr.UnblockedSIDs = append([]string(nil), tr.UnblockedSIDs...)
			c.Triggers[i] = tr
		}
	}
	return c
}

func (p BoardPulse) renderText(omittedNotable, omittedTriggers int) string {
	ap := strings.TrimSpace(p.AutopilotState)
	if ap == "" {
		ap = "off"
	}
	ts := strings.TrimSpace(p.Time)

	var b strings.Builder
	// Line 1: Header
	if ts != "" {
		fmt.Fprintf(&b, "[SYRE PULSE | autopilot %s | %s]\n", ap, ts)
	} else {
		fmt.Fprintf(&b, "[SYRE PULSE | autopilot %s]\n", ap)
	}

	// Line 2: Fleet
	fmt.Fprintf(&b, "Fleet: %d/%d up", p.FleetUp, p.FleetTotal)
	notable := make([]string, 0, len(p.FleetNotable)+1)
	for _, n := range p.FleetNotable {
		if s := strings.TrimSpace(n); s != "" {
			notable = append(notable, s)
		}
	}
	if omittedNotable > 0 {
		notable = append(notable, fmt.Sprintf("+%d more", omittedNotable))
	}
	if len(notable) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(notable, "; "))
		b.WriteByte(')')
	}
	b.WriteByte('\n')

	// Line 3: Board
	readyPart := "0 ready"
	if p.Ready > 0 || p.Dispatchable > 0 {
		readyPart = fmt.Sprintf("%d ready (%d dispatchable)", p.Ready, p.Dispatchable)
	}
	needsPart := fmt.Sprintf("%d needs you", p.NeedsYou)
	if p.NeedsYou > 0 && len(p.NeedsYouSIDs) > 0 {
		sids := p.NeedsYouSIDs
		if len(sids) > 3 {
			sids = sids[:3]
		}
		needsPart = fmt.Sprintf("%d needs you (%s)", p.NeedsYou, strings.Join(sids, ", "))
	}
	fmt.Fprintf(&b, "Board: %s · %d running · %s · %d review", readyPart, p.Running, needsPart, p.Review)

	// Line 4: Trigger(s)
	var parts []string
	for _, tr := range p.Triggers {
		if s := formatPulseTrigger(tr); s != "" {
			parts = append(parts, s)
		}
	}
	if omittedTriggers > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", omittedTriggers))
	}
	if len(parts) > 0 {
		b.WriteString("\nTrigger: ")
		b.WriteString(strings.Join(parts, "; "))
	}

	return b.String()
}

func formatPulseTrigger(tr PulseTrigger) string {
	switch tr.Kind {
	case PulseTriggerReview:
		s := tr.TicketSID + " → review"
		if len(tr.UnblockedSIDs) > 0 {
			s += " (unblocked " + strings.Join(tr.UnblockedSIDs, ", ") + ")"
		}
		if tr.Followups > 0 {
			s += fmt.Sprintf("; %d %s", tr.Followups, followupNoun(tr.Followups))
		}
		return s
	case PulseTriggerFailed:
		s := tr.TicketSID + " → failed"
		if cat := strings.TrimSpace(tr.Category); cat != "" {
			s += " (" + cat + ")"
		}
		if tr.Followups > 0 {
			s += fmt.Sprintf("; %d %s", tr.Followups, followupNoun(tr.Followups))
		}
		return s
	case PulseTriggerNeedsYou:
		s := tr.TicketSID + " → needs you"
		if cat := strings.TrimSpace(tr.Category); cat != "" {
			s += " (" + cat + ")"
		}
		if tr.Followups > 0 {
			s += fmt.Sprintf("; %d %s", tr.Followups, followupNoun(tr.Followups))
		}
		return s
	case PulseTriggerFollowups:
		if tr.Followups <= 0 {
			return ""
		}
		if tr.TicketSID != "" {
			return fmt.Sprintf("%s: %d %s", tr.TicketSID, tr.Followups, followupNoun(tr.Followups))
		}
		return fmt.Sprintf("%d %s", tr.Followups, followupNoun(tr.Followups))
	case PulseTriggerNeedsDecomposition:
		sids := tr.TicketSIDs
		if len(sids) == 0 && tr.TicketSID != "" {
			sids = []string{tr.TicketSID}
		}
		if len(sids) == 0 {
			return "needs decomposition"
		}
		if len(sids) > 3 {
			return fmt.Sprintf("needs decomposition: %s (+%d)", strings.Join(sids[:3], ", "), len(sids)-3)
		}
		return "needs decomposition: " + strings.Join(sids, ", ")
	case PulseTriggerDirectiveExpiring:
		if s := strings.TrimSpace(tr.Summary); s != "" {
			return s
		}
		return "directive expiring"
	case PulseTriggerBoardIdle:
		if p := strings.TrimSpace(tr.Project); p != "" {
			return p + " board idle"
		}
		return "board idle"
	default:
		return strings.TrimSpace(tr.Summary)
	}
}

func followupNoun(n int) string {
	if n == 1 {
		return "followup"
	}
	return "followups"
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return strings.TrimRight(s[:i], " ;,")
}
