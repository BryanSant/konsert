package protocol

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/tokenbudget"
)

// PacketMaxTokens is the hard token budget for DispatchPacket.Text() (docs/design/autopilot.md).
const PacketMaxTokens = tokenbudget.Packet

// DispatchPacketRepo describes one repo worktree bound to a ticket.
type DispatchPacketRepo struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// DispatchPacket is the compact work packet compiled by the hub for a worker session.
type DispatchPacket struct {
	TicketSID  string               `json:"ticket_sid"`
	Project    string               `json:"project,omitempty"`
	Repos      []DispatchPacketRepo `json:"repos,omitempty"`
	Stacked    []string             `json:"stacked,omitempty"`
	Objective  string               `json:"objective"`
	Acceptance []string             `json:"acceptance,omitempty"`
	Contracts  []string             `json:"contracts,omitempty"`
	Verify     string               `json:"verify,omitempty"`
	Tools      []string             `json:"tools,omitempty"`
}

// Text serializes the dispatch packet as compact YAML (docs/design/autopilot.md),
// enforcing the hard 300-token cap (chars/4 estimate) by trimming objective first,
// then contracts.
func (p DispatchPacket) Text() string {
	// Work on a shallow copy with cloned slices so Text() never mutates the receiver.
	c := p
	if len(p.Repos) > 0 {
		c.Repos = append([]DispatchPacketRepo(nil), p.Repos...)
	}
	if len(p.Stacked) > 0 {
		c.Stacked = append([]string(nil), p.Stacked...)
	}
	if len(p.Acceptance) > 0 {
		c.Acceptance = append([]string(nil), p.Acceptance...)
	}
	if len(p.Contracts) > 0 {
		c.Contracts = append([]string(nil), p.Contracts...)
	}
	if len(p.Tools) > 0 {
		c.Tools = append([]string(nil), p.Tools...)
	}

	out := c.renderText()
	if EstimateTokens(out) <= PacketMaxTokens {
		return out
	}

	origObjective := c.Objective

	// 1. Trim objective first if trimming objective alone can satisfy the budget.
	if c.Objective != "" {
		c.Objective = ""
		withoutObj := c.renderText()
		if EstimateTokens(withoutObj) <= PacketMaxTokens {
			c.Objective = trimToFitLimit(origObjective, PacketMaxTokens, func(s string) string {
				c.Objective = s
				return c.renderText()
			})
			return c.renderText()
		}
		// Even with an empty objective, contracts/other fields exceed the budget.
		// Restore original objective so step 2 trims contracts before we fit objective.
		c.Objective = origObjective
	}

	// 2. Trim contracts from the end until the packet (with empty objective if needed) fits.
	for len(c.Contracts) > 0 && EstimateTokens(out) > PacketMaxTokens {
		c.Contracts = c.Contracts[:len(c.Contracts)-1]
		out = c.renderText()
		if EstimateTokens(out) <= PacketMaxTokens {
			return out
		}
		// Check if trimming objective now suffices with the reduced contracts list.
		if origObjective != "" {
			c.Objective = ""
			if EstimateTokens(c.renderText()) <= PacketMaxTokens {
				c.Objective = trimToFitLimit(origObjective, PacketMaxTokens, func(s string) string {
					c.Objective = s
					return c.renderText()
				})
				return c.renderText()
			}
			c.Objective = origObjective
		}
	}

	// 3. Last resort: trim objective to empty, then acceptance items from the end,
	// then hard-cap chars so the 300-token invariant is never violated.
	if c.Objective != "" && EstimateTokens(out) > PacketMaxTokens {
		c.Objective = trimToFitLimit(c.Objective, PacketMaxTokens, func(s string) string {
			c.Objective = s
			return c.renderText()
		})
		out = c.renderText()
	}
	for len(c.Acceptance) > 0 && EstimateTokens(out) > PacketMaxTokens {
		c.Acceptance = c.Acceptance[:len(c.Acceptance)-1]
		out = c.renderText()
	}
	if EstimateTokens(out) > PacketMaxTokens {
		maxChars := PacketMaxTokens * 4
		if len(out) > maxChars {
			out = out[:maxChars]
		}
	}
	return out
}

func (p DispatchPacket) renderText() string {
	var b strings.Builder
	writeLine := func(s string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}

	if p.TicketSID != "" {
		writeLine("ticket: " + p.TicketSID)
	}
	if p.Project != "" {
		writeLine("project: " + p.Project)
	}
	if len(p.Repos) > 0 {
		writeLine("repos:")
		for _, r := range p.Repos {
			parts := make([]string, 0, 3)
			if r.Name != "" {
				parts = append(parts, "name: "+r.Name)
			}
			if r.Path != "" {
				parts = append(parts, "path: "+r.Path)
			}
			if r.Branch != "" {
				parts = append(parts, "branch: "+r.Branch)
			}
			writeLine("  - {" + strings.Join(parts, ", ") + "}")
		}
	}
	if len(p.Stacked) > 0 {
		writeLine("stacked:")
		for _, s := range p.Stacked {
			writeLine("  - " + s)
		}
	}
	if p.Objective != "" {
		writeLine("objective: " + p.Objective)
	}
	if len(p.Acceptance) > 0 {
		writeLine("acceptance:")
		for _, item := range p.Acceptance {
			writeLine("  - " + item)
		}
	}
	if len(p.Contracts) > 0 {
		writeLine("contracts:")
		for _, c := range p.Contracts {
			writeLine("  - " + strconv.Quote(c))
		}
	}
	if p.Verify != "" {
		writeLine("verify: " + p.Verify)
	}
	if len(p.Tools) > 0 {
		writeLine(fmt.Sprintf("tools: %s", strings.Join(p.Tools, ", ")))
	}
	return b.String()
}
