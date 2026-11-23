package chat

// Failure classification for ACP agents (design doc "Failure classification").

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

var (
	authRe  = regexp.MustCompile(`(?i)(authentication required|not (signed|logged) in|unauthenticated|unauthori[sz]ed|please (run|use) .{0,30}login|login required|run '?\S*\s?login|sign in (again|to continue)|token (has )?expired|expired token|invalid (api key|token|credentials)|re-?authenticate|\b401\b)`)
	usageRe = regexp.MustCompile(`(?i)(usage limit|limit reached|quota (exceeded|reached|exhausted)|individual quota|out of (credits|requests|usage)|premium requests?|monthly limit|weekly limit|exceeded your|you('ve| have) (reached|hit|used up)|resource.?exhausted|insufficient (credits|quota))`)
	rateRe  = regexp.MustCompile(`(?i)(rate.?limit|too many requests|\b429\b|overloaded|\b529\b|\b503\b|temporarily unavailable|capacity|try again (later|shortly|in a (few|moment)))`)
	modelRe = regexp.MustCompile(`(?i)(invalid model|unknown model|model .{0,60}(not (found|available|supported)|does not exist|unavailable)|cannot use this model|unsupported model|no such model)`)

	resetsInRe = regexp.MustCompile(`(?i)(?:resets?|try again|available again|retry)\s+in\s+((?:\d+\s*(?:d|h|m|s|days?|hours?|hrs?|minutes?|mins?|seconds?|secs?)\s*,?\s*(?:and\s+)?)+)`)
	resetsAtRe = regexp.MustCompile(`(?i)(?:resets?|reset|available again|try again)\s+(?:at|on)\s+([0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9:.]+(?:Z|[+-][0-9:]+)?|[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{1,2}(?::[0-9]{2})?\s*(?:am|pm)?(?:\s*\(?[A-Z]{2,5}\)?)?)`)
	tzSuffixRe = regexp.MustCompile(`\s+\(?([A-Z]{2,5})\)?$`)
	durPartRe  = regexp.MustCompile(`(?i)(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
)

// classifyText decides an error kind from free text ("" = no match).
func classifyText(s string) string {
	switch {
	case s == "":
		return ""
	case usageRe.MatchString(s):
		return protocol.ErrKindUsageLimit
	case authRe.MatchString(s):
		return protocol.ErrKindAuth
	case modelRe.MatchString(s):
		return protocol.ErrKindModel
	case rateRe.MatchString(s):
		return protocol.ErrKindRateLimit
	}
	return ""
}

// parseUsageWindow extracts the reset time and window from usage-limit text.
func parseUsageWindow(s string, now time.Time) (*time.Time, string) {
	var resets *time.Time
	if m := resetsInRe.FindStringSubmatch(s); m != nil {
		var d time.Duration
		for _, p := range durPartRe.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(p[1])
			switch u := strings.ToLower(p[2]); {
			case strings.HasPrefix(u, "d"):
				d += time.Duration(n) * 24 * time.Hour
			case strings.HasPrefix(u, "h"):
				d += time.Duration(n) * time.Hour
			case strings.HasPrefix(u, "m"):
				d += time.Duration(n) * time.Minute
			default:
				d += time.Duration(n) * time.Second
			}
		}
		if d > 0 {
			t := now.Add(d).Truncate(time.Second)
			resets = &t
		}
	} else if m := resetsAtRe.FindStringSubmatch(s); m != nil {
		resets = parseResetAt(strings.TrimSpace(m[1]), now)
	}
	lower := strings.ToLower(s)
	window := ""
	switch {
	case strings.Contains(lower, "5-hour") || strings.Contains(lower, "five hour") || strings.Contains(lower, "5 hour") || strings.Contains(lower, "five_hour"):
		window = "five_hour"
	case strings.Contains(lower, "weekly") || strings.Contains(lower, "7-day") || strings.Contains(lower, "seven_day") || strings.Contains(lower, "this week"):
		window = "seven_day"
	case strings.Contains(lower, "daily") || strings.Contains(lower, "per day") || strings.Contains(lower, "today"):
		window = "daily"
	case strings.Contains(lower, "monthly") || strings.Contains(lower, "this month") || strings.Contains(lower, "billing period") || strings.Contains(lower, "billing cycle"):
		window = "monthly"
	case strings.Contains(lower, "session"):
		window = "session"
	}
	return resets, window
}

func parseResetAt(v string, now time.Time) *time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, now.Location()); err == nil {
			return &t
		}
	}
	// Clock time ("3:40 PM", "15:40", "3pm"): the next occurrence in local time.
	clock := strings.ToUpper(strings.TrimSpace(v))
	if m := tzSuffixRe.FindStringSubmatch(clock); m != nil && m[1] != "AM" && m[1] != "PM" {
		clock = clock[:len(clock)-len(m[0])] // drop a trailing zone name ("PST", "(UTC)")
	}
	clock = strings.Join(strings.Fields(clock), "")
	for _, layout := range []string{"3:04PM", "3PM", "15:04"} {
		if t, err := time.ParseInLocation(layout, clock, now.Location()); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			if !at.After(now) {
				at = at.Add(24 * time.Hour)
			}
			return &at
		}
	}
	return nil
}

// errContext carries what classification needs beyond the error itself.
type errContext struct {
	ad    *acpAdapter
	acct  Account
	init  *acpInitResult
	proc  *Proc
	phase string // initialize, session/new, prompt, ...
	now   func() time.Time
}

// classify turns any engine failure into an AgentChatError.
func (ec errContext) classify(err error) *protocol.AgentChatError {
	now := time.Now
	if ec.now != nil {
		now = ec.now
	}
	var secrets []string
	if ec.proc != nil {
		secrets = ec.proc.Secrets()
	} else {
		secrets = EnvSecrets(ec.acct.Env)
	}
	label := ec.ad.Label
	if ec.acct.Label != "" {
		label = ec.acct.Label
	}
	out := &protocol.AgentChatError{Kind: protocol.ErrKindOther}
	var msg string
	if err != nil {
		msg = err.Error()
	}

	var rpcErr *rpcError
	var protoErr *protocolError
	stderr := ""
	exited := false
	if ec.proc != nil && (errors.Is(err, errRPCClosed) || ec.proc.Exited()) {
		select {
		case <-ec.proc.Done():
			exited = true
		case <-time.After(2 * time.Second):
		}
	}
	if ec.proc != nil {
		stderr = ec.proc.StderrTail(2000)
	}

	switch {
	case IsNotInstalled(err):
		out.Kind = protocol.ErrKindNotFound
		out.Message = fmt.Sprintf("%s CLI is not installed on this host (%s).", label, ec.ad.DefaultBinary)
	case errors.As(err, &rpcErr):
		text := rpcErr.Message + " " + rpcErr.dataText()
		kind := classifyText(text)
		if rpcErr.Code == acpAuthRequired && strings.Contains(strings.ToLower(text), "auth") {
			kind = protocol.ErrKindAuth
		}
		if kind == "" && rpcErr.Code == rpcInvalidParams && strings.Contains(strings.ToLower(text), "model") {
			kind = protocol.ErrKindModel
		}
		if kind == "" {
			kind = protocol.ErrKindOther
		}
		out.Kind = kind
		out.Message = fmt.Sprintf("%s: %s", label, strings.TrimSpace(rpcErr.Error()))
	case errors.As(err, &protoErr):
		out.Kind = protocol.ErrKindProtocol
		out.Message = fmt.Sprintf("%s sent malformed protocol output: %s", label, protoErr.msg)
	case errors.Is(err, context.DeadlineExceeded):
		out.Kind = protocol.ErrKindTimeout
		out.Message = fmt.Sprintf("%s did not answer %s in time.", label, ec.phase)
		out.Retryable = true
	case exited || errors.Is(err, errRPCClosed):
		out.Kind = protocol.ErrKindExit
		code := -1
		if ec.proc != nil && exited {
			code = ec.proc.ExitCode()
		}
		out.ExitCode = &code
		if code >= 0 {
			out.Message = fmt.Sprintf("%s exited with code %d", label, code)
		} else {
			out.Message = fmt.Sprintf("%s exited unexpectedly", label)
		}
		if ec.phase != "" {
			out.Message += " during " + ec.phase
		}
		if line := lastLine(stderr); line != "" {
			out.Message += ": " + line
		}
		out.Retryable = true
	default:
		out.Message = fmt.Sprintf("%s: %s", label, msg)
	}

	// The stream or stderr may explain an exit or a generic error better.
	if out.Kind == protocol.ErrKindExit || out.Kind == protocol.ErrKindOther || out.Kind == protocol.ErrKindTimeout {
		if k := classifyText(msg + "\n" + stderr); k != "" {
			out.Kind = k
			out.Retryable = false
		}
	}
	switch out.Kind {
	case protocol.ErrKindAuth:
		out.Relogin = ec.ad.reloginFor(ec.acct, ec.init)
		out.Retryable = false
		if !strings.Contains(strings.ToLower(out.Message), "sign in") && !strings.Contains(strings.ToLower(out.Message), "log") {
			out.Message = label + " needs you to sign in again. " + out.Message
		}
	case protocol.ErrKindUsageLimit:
		out.ResetsAt, out.Window = parseUsageWindow(msg+"\n"+stderr, now())
		out.Retryable = false
	case protocol.ErrKindRateLimit:
		out.Retryable = true
	}
	if out.Kind == protocol.ErrKindExit || stderr != "" && out.Kind != protocol.ErrKindNotFound {
		out.Stderr = stderr
	}
	out.Message = RedactSecrets(truncate(out.Message, 1000), secrets...)
	return out
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return truncate(strings.TrimSpace(s), 300)
}
