package digest

import (
	"regexp"
	"strings"

	"github.com/BryanSant/syre/internal/protocol"
)

var (
	// Shared text classifiers used by both chat engines (internal/worker/chat) and task digests.
	authRe  = regexp.MustCompile(`(?i)(authentication required|authentication_error|invalid authentication|not (signed|logged) in|unauthenticated|unauthori[sz]ed|please (run|use) .{0,30}login|please log in|login required|login again|run '?\S*\s?(auth )?login|sign in (again|to continue)|token (has )?expired|token has been revoked|expired token|missing bearer|refresh token|invalid (api key|token|credentials|bearer token)|oauth token is invalid|re-?authenticate|(?:http|status|error|code)[: ]+401\b)`)
	usageRe = regexp.MustCompile(`(?i)(usage[ _]limit|limit reached|hit your limit|quota (exceeded|reached|exhausted)|individual quota|out of (credits|requests|usage|extra usage)|premium requests?|monthly limit|weekly limit|5-hour limit|session limit|exceeded your|you('ve| have) (reached|hit|used up)|resource.?exhausted|insufficient (credits|quota))`)
	rateRe  = regexp.MustCompile(`(?i)(rate[ ._]?limit|too many requests|\b429\b|overloaded|\b529\b|\b503\b|temporarily unavailable|capacity|try again (later|shortly|in a (few|moment)))`)
	modelRe = regexp.MustCompile(`(?i)(invalid model|unknown model|model_not_found|model not found|model .{0,60}(not (found|available|supported)|does not exist|unavailable)|cannot use this model|unsupported model|no such model|issue with the selected model|not available for your account)`)

	// Task/OS-level failure classifiers.
	oomRe         = regexp.MustCompile(`(?i)(out of memory|cannot allocate memory|oom[- ]?killer|killed.*oom|memory limit exceeded|fatal error: runtime: out of memory|javascript heap out of memory|allocation failed|job object.*memory)`)
	diskFullRe    = regexp.MustCompile(`(?i)(no space left on device|\benospc\b|disk full|not enough space on the disk|insufficient disk space)`)
	missingToolRe = regexp.MustCompile(`(?i)(command not found|is not recognized as an internal or external command|is not recognized as the name of a cmdlet|executable file not found)`)
)

// Windows NTSTATUS / Win32 exit codes associated with memory / job-object memory limits.
const (
	winStatusNoMemory        = 0xC0000017 // STATUS_NO_MEMORY (3221225495 / -1073741801)
	winStatusCommitmentLimit = 0xC000012D // STATUS_COMMITMENT_LIMIT (3221225773 / -1073741523)
)

// ClassifyText decides an error kind from free text ("" = no match).
// Shared by chat engines (claude, codex, ACP) and task failure classification.
func ClassifyText(s string) string {
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

// FailureClassInput supplies all signals available when classifying a failed task.
type FailureClassInput struct {
	ExitCode int
	TimedOut bool
	Canceled bool
	OOM      bool
	Text     string // cleaned output/stderr/summary text
	WaitErr  error
}

// ClassifyTaskFailure maps a failed/canceled task's signals to a canonical failure category.
func ClassifyTaskFailure(in FailureClassInput) string {
	if in.Canceled {
		return protocol.ErrKindCanceled
	}
	if in.TimedOut {
		return protocol.ErrKindTimeout
	}

	text := in.Text
	if in.WaitErr != nil {
		if text != "" {
			text += "\n" + in.WaitErr.Error()
		} else {
			text = in.WaitErr.Error()
		}
	}

	if in.OOM || isOOMExitCode(in.ExitCode) || oomRe.MatchString(text) {
		return protocol.ErrKindOOM
	}
	if diskFullRe.MatchString(text) {
		return protocol.ErrKindDiskFull
	}
	if in.ExitCode == 127 || missingToolRe.MatchString(text) {
		return protocol.ErrKindMissingTool
	}
	if kind := ClassifyText(text); kind != "" {
		switch kind {
		case protocol.ErrKindAuth, protocol.ErrKindUsageLimit, protocol.ErrKindRateLimit:
			return kind
		}
	}
	if strings.Contains(strings.ToLower(text), "signal: killed") {
		return protocol.ErrKindOOM
	}
	return protocol.ErrKindAgentError
}

func isOOMExitCode(code int) bool {
	if code == 137 || code == -9 {
		return true
	}
	u := uint32(code)
	return u == winStatusNoMemory || u == winStatusCommitmentLimit
}
