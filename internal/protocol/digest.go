package protocol

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Digest statuses for RunDigest.Status.
const (
	DigestStatusReview    = "review"
	DigestStatusNeedsYou  = "needs_you"
	DigestStatusFailed    = "failed"
	DigestStatusCompleted = "completed"
)

// DigestMaxTokens is the hard token budget for RunDigest.Text() (docs/design/autopilot.md).
const DigestMaxTokens = 250

// DigestVerify holds verification command results (populated in a later ticket).
type DigestVerify struct {
	Cmd    string `json:"cmd,omitempty"`
	Exit   int    `json:"exit,omitzero"`
	Passed int    `json:"passed,omitzero"`
	Failed int    `json:"failed,omitzero"`
}

// DigestFailure captures structured failure details for failed runs.
type DigestFailure struct {
	Category string   `json:"category,omitempty"`
	Tests    []string `json:"tests,omitempty"`
	Tail     []string `json:"tail,omitempty"`
}

// DigestQuestion captures the worker's question and preceding output for needs_you runs.
type DigestQuestion struct {
	Text    string   `json:"text,omitempty"`
	Context []string `json:"context,omitempty"`
}

// RunDigest is the compact post-run digest produced by syred's extractor.
type RunDigest struct {
	TicketSID string            `json:"ticket_sid,omitempty"`
	Attempt   int               `json:"attempt,omitzero"`
	Status    string            `json:"status"`
	Machine   string            `json:"machine,omitempty"`
	Agent     string            `json:"agent,omitempty"`
	Model     string            `json:"model,omitempty"`
	DurationS int64             `json:"duration_s,omitzero"`
	Git       map[string]string `json:"git,omitempty"`
	Verify    *DigestVerify     `json:"verify,omitzero"`
	Interface string            `json:"interface,omitempty"`
	Followups []string          `json:"followups,omitempty"`
	Summary   string            `json:"summary,omitempty"`
	Failure   *DigestFailure    `json:"failure,omitzero"`
	Question  *DigestQuestion   `json:"question,omitzero"`
}

// EstimateTokens returns a simple ceiling chars/4 token approximation.
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}

// Text serializes the digest as compact YAML-ish text, enforcing the hard 250-token cap
// by trimming tail/context lines first, then failing test names, then summary.
func (d RunDigest) Text() string {
	// Work on a shallow copy with cloned slices so Text() never mutates the receiver.
	c := d
	if d.Failure != nil {
		f := *d.Failure
		f.Tests = append([]string(nil), d.Failure.Tests...)
		f.Tail = append([]string(nil), d.Failure.Tail...)
		c.Failure = &f
	}
	if d.Question != nil {
		q := *d.Question
		q.Context = append([]string(nil), d.Question.Context...)
		c.Question = &q
	}
	if len(d.Followups) > 0 {
		c.Followups = append([]string(nil), d.Followups...)
	}

	out := c.renderText()
	if EstimateTokens(out) <= DigestMaxTokens {
		return out
	}

	// 1. Trim failure tail lines from the front (oldest first, preserving most recent).
	if c.Failure != nil {
		for len(c.Failure.Tail) > 0 && EstimateTokens(out) > DigestMaxTokens {
			c.Failure.Tail = c.Failure.Tail[1:]
			out = c.renderText()
		}
	}
	// 1b. Trim question context lines from the front (oldest first).
	if c.Question != nil {
		for len(c.Question.Context) > 0 && EstimateTokens(out) > DigestMaxTokens {
			c.Question.Context = c.Question.Context[1:]
			out = c.renderText()
		}
	}
	if EstimateTokens(out) <= DigestMaxTokens {
		return out
	}

	// 2. Trim failing test names from the end.
	if c.Failure != nil {
		for len(c.Failure.Tests) > 0 && EstimateTokens(out) > DigestMaxTokens {
			c.Failure.Tests = c.Failure.Tests[:len(c.Failure.Tests)-1]
			out = c.renderText()
		}
	}
	if EstimateTokens(out) <= DigestMaxTokens {
		return out
	}

	// 3. Trim summary.
	if c.Summary != "" {
		c.Summary = trimToFit(c.Summary, func(s string) string {
			c.Summary = s
			return c.renderText()
		})
		out = c.renderText()
	}
	if EstimateTokens(out) <= DigestMaxTokens {
		return out
	}

	// 4. Last resort: trim followups, interface, question text so the hard cap is never violated.
	for len(c.Followups) > 0 && EstimateTokens(out) > DigestMaxTokens {
		c.Followups = c.Followups[:len(c.Followups)-1]
		out = c.renderText()
	}
	if c.Interface != "" && EstimateTokens(out) > DigestMaxTokens {
		c.Interface = trimToFit(c.Interface, func(s string) string {
			c.Interface = s
			return c.renderText()
		})
		out = c.renderText()
	}
	if c.Question != nil && c.Question.Text != "" && EstimateTokens(out) > DigestMaxTokens {
		c.Question.Text = trimToFit(c.Question.Text, func(s string) string {
			c.Question.Text = s
			return c.renderText()
		})
		out = c.renderText()
	}
	if EstimateTokens(out) > DigestMaxTokens {
		maxChars := DigestMaxTokens * 4
		if len(out) > maxChars {
			out = out[:maxChars]
		}
	}
	return out
}

func trimToFit(val string, render func(string) string) string {
	return trimToFitLimit(val, DigestMaxTokens, render)
}

func trimToFitLimit(val string, maxTokens int, render func(string) string) string {
	// If even clearing the field doesn't fit, return empty.
	if EstimateTokens(render("")) > maxTokens {
		return ""
	}
	runes := []rune(val)
	lo, hi := 0, len(runes)
	best := ""
	for lo <= hi {
		mid := (lo + hi) / 2
		cand := string(runes[:mid])
		if mid < len(runes) && mid > 3 {
			cand = strings.TrimSpace(string(runes[:mid-3])) + "..."
		}
		if EstimateTokens(render(cand)) <= maxTokens {
			best = cand
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

func (d RunDigest) renderText() string {
	var b strings.Builder
	writeLine := func(s string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}

	if d.TicketSID != "" {
		writeLine("ticket: " + d.TicketSID)
	}
	if d.Attempt > 0 {
		writeLine("attempt: " + strconv.Itoa(d.Attempt))
	}
	if d.Status != "" {
		writeLine("status: " + d.Status)
	}
	if m := formatMachineLine(d.Machine, d.Agent, d.Model); m != "" {
		writeLine(m)
	}
	if d.DurationS > 0 {
		writeLine("duration_s: " + strconv.FormatInt(d.DurationS, 10))
	}
	if len(d.Git) > 0 {
		keys := make([]string, 0, len(d.Git))
		for k := range d.Git {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %q", k, d.Git[k]))
		}
		writeLine("git: {" + strings.Join(parts, ", ") + "}")
	}
	if d.Verify != nil && (d.Verify.Cmd != "" || d.Verify.Exit != 0 || d.Verify.Passed > 0 || d.Verify.Failed > 0) {
		writeLine(fmt.Sprintf("verify: {cmd: %q, exit: %d, passed: %d, failed: %d}",
			d.Verify.Cmd, d.Verify.Exit, d.Verify.Passed, d.Verify.Failed))
	}
	if d.Interface != "" {
		writeLine(fmt.Sprintf("interface: %q", d.Interface))
	}
	if len(d.Followups) > 0 {
		quoted := make([]string, len(d.Followups))
		for i, f := range d.Followups {
			quoted[i] = strconv.Quote(f)
		}
		writeLine("followups: [" + strings.Join(quoted, ", ") + "]")
	}
	if d.Summary != "" {
		writeLine("summary: " + d.Summary)
	}
	if d.Failure != nil && (d.Failure.Category != "" || len(d.Failure.Tests) > 0 || len(d.Failure.Tail) > 0) {
		writeLine("failure:")
		if d.Failure.Category != "" {
			writeLine("  category: " + d.Failure.Category)
		}
		if len(d.Failure.Tests) > 0 {
			writeLine("  tests: [" + strings.Join(d.Failure.Tests, ", ") + "]")
		}
		if len(d.Failure.Tail) > 0 {
			writeLine("  tail:")
			for _, line := range d.Failure.Tail {
				writeLine("    - " + line)
			}
		}
	}
	if d.Question != nil && (d.Question.Text != "" || len(d.Question.Context) > 0) {
		writeLine("question:")
		if d.Question.Text != "" {
			writeLine("  text: " + strconv.Quote(d.Question.Text))
		}
		if len(d.Question.Context) > 0 {
			writeLine("  context:")
			for _, line := range d.Question.Context {
				writeLine("    - " + line)
			}
		}
	}
	return b.String()
}

func formatMachineLine(machine, agent, model string) string {
	var meta []string
	if agent != "" {
		meta = append(meta, agent)
	}
	if model != "" {
		meta = append(meta, model)
	}
	if machine != "" {
		if len(meta) > 0 {
			return fmt.Sprintf("machine: %s (%s)", machine, strings.Join(meta, ", "))
		}
		return "machine: " + machine
	}
	if len(meta) > 0 {
		return "agent: " + strings.Join(meta, ", ")
	}
	return ""
}
