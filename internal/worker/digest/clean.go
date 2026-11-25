package digest

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	// DefaultFailedTailLines is the maximum meaningful lines included for failed runs.
	DefaultFailedTailLines = 30
	// DefaultQuestionContextLines is the maximum preceding lines included for needs_you runs.
	DefaultQuestionContextLines = 20
	// maxCleanLineRunes prevents a single minified/noisy line from consuming the entire digest budget.
	maxCleanLineRunes = 160
)

var (
	// Matches CSI, OSC, DCS/APC/PM/SOS, and 2-byte ESC sequences.
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[PX^_][^\x1b]*\x1b\\|\x1b[@-Z\\-_]`)
	// Pure ASCII spinner frames (e.g. "|", "/", "-", "\").
	asciiSpinnerRe = regexp.MustCompile(`^[/\\|\-*•·.]+$`)
)

// StripANSI removes terminal escape sequences and non-printable control characters
// (preserving \n, \r, and \t).
func StripANSI(s string) string {
	clean := ansiRe.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(clean))
	for _, r := range clean {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// drop control characters (BEL, BS, etc.)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isSpinnerOnly reports whether a trimmed line consists solely of spinner / braille /
// box-drawing / progress-bar glyphs without meaningful text.
func isSpinnerOnly(line string) bool {
	if line == "" {
		return true
	}
	if asciiSpinnerRe.MatchString(line) {
		return true
	}
	hasMeaningful := false
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		// Braille patterns (U+2800..U+28FF) are used by almost all CLI spinners.
		if r >= 0x2800 && r <= 0x28FF {
			continue
		}
		// Block elements & box drawing (progress bars / TUI chrome).
		if (r >= 0x2500 && r <= 0x259F) || r == '━' || r == '─' || r == '═' {
			continue
		}
		hasMeaningful = true
		break
	}
	return !hasMeaningful
}

// CleanLines strips ANSI escapes, resolves carriage-return (\r) progress-bar rewrites,
// drops empty and spinner-only frames, and collapses consecutive identical lines with a count.
func CleanLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	s := StripANSI(string(raw))
	s = strings.ReplaceAll(s, "\r\n", "\n")

	rawLines := strings.Split(s, "\n")
	cleaned := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		// Resolve \r overwrites (progress bars, spinners): keep the last non-empty frame.
		if strings.ContainsRune(line, '\r') {
			parts := strings.Split(line, "\r")
			chosen := ""
			for i := len(parts) - 1; i >= 0; i-- {
				cand := strings.TrimSpace(parts[i])
				if cand != "" && !isSpinnerOnly(cand) {
					chosen = cand
					break
				}
			}
			line = chosen
		} else {
			line = strings.TrimSpace(line)
		}
		if line == "" || isSpinnerOnly(line) {
			continue
		}
		line = truncateRunes(line, maxCleanLineRunes)
		cleaned = append(cleaned, line)
	}

	if len(cleaned) == 0 {
		return nil
	}

	// Collapse consecutive repeated lines with a "(xN)" suffix.
	out := make([]string, 0, len(cleaned))
	i := 0
	for i < len(cleaned) {
		j := i + 1
		for j < len(cleaned) && cleaned[j] == cleaned[i] {
			j++
		}
		count := j - i
		if count > 1 {
			out = append(out, fmt.Sprintf("%s (x%d)", cleaned[i], count))
		} else {
			out = append(out, cleaned[i])
		}
		i = j
	}
	return out
}

// ExtractFailureTail returns the last maxLines meaningful lines from raw PTY/stderr output.
func ExtractFailureTail(raw []byte, maxLines int) []string {
	if maxLines <= 0 {
		maxLines = DefaultFailedTailLines
	}
	lines := dropPassingNoise(CleanLines(raw))
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// passingNoise matches test-runner lines that carry no failure information: test starts,
// passing tests and passing packages.
var passingNoise = regexp.MustCompile(`^(=== (RUN|PAUSE|CONT)\s|\s*--- PASS: |ok\s+\S+\s+[\d.]+s|PASS$|\s*✓ |\s*√ |test \S+ \.\.\. ok$|\[INFO\] Tests run: \d+, Failures: 0)`)

func dropPassingNoise(lines []string) []string {
	out := lines[:0:0]
	for _, l := range lines {
		if !passingNoise.MatchString(l) {
			out = append(out, l)
		}
	}
	return out
}

// ExtractQuestionContext returns the worker's question and up to maxPreceding cleaned lines
// immediately preceding it from raw PTY output.
func ExtractQuestionContext(raw []byte, question string, maxPreceding int) (string, []string) {
	if maxPreceding <= 0 {
		maxPreceding = DefaultQuestionContextLines
	}
	lines := CleanLines(raw)
	q := strings.TrimSpace(StripANSI(question))
	if len(lines) == 0 {
		return q, nil
	}

	matchIdx := -1
	if q != "" {
		qLower := strings.ToLower(q)
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(strings.ToLower(lines[i]), qLower) {
				matchIdx = i
				break
			}
		}
	} else {
		matchIdx = len(lines) - 1
		q = lines[matchIdx]
	}

	var ctxLines []string
	if matchIdx >= 0 {
		start := matchIdx - maxPreceding
		if start < 0 {
			start = 0
		}
		ctxLines = append([]string(nil), lines[start:matchIdx]...)
	} else {
		start := len(lines) - maxPreceding
		if start < 0 {
			start = 0
		}
		ctxLines = append([]string(nil), lines[start:]...)
	}
	return q, ctxLines
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return strings.TrimSpace(string(runes[:max-3])) + "..."
}
