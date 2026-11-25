package digest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// MaxSummaryChars caps RunDigest.Summary length.
const MaxSummaryChars = 200

// Input carries all post-run signals collected by syred for a single task.
type Input struct {
	TicketSID       string
	Attempt         int
	TaskStatus      string // protocol.TaskStatusCompleted | TaskStatusFailed | TaskStatusCanceled | TaskStatusWaitingInput
	Machine         string
	Agent           string
	Model           string
	Duration        time.Duration
	ExitCode        int
	TimedOut        bool
	Canceled        bool
	OOM             bool
	WaitErr         error
	FailureCategory string                          // optional explicit failure category (e.g. from AgentChatError.Kind)
	Summary         string                          // from drv.ExtractSummary or agent final message
	FinalMessage    string                          // optional agent final message for interface extraction
	Question        string                          // worker's question text when waiting_input / needs_you
	Output          []byte                          // raw PTY/stderr ring buffer snapshot
	Git             map[string]*protocol.GitSummary // repo name -> GitSummary for every binding or single workdir
	Verify          *protocol.DigestVerify          // nil until verification gate (A4a)
	Followups       []string                        // empty until ticket_followup wiring
}

// Build constructs a compact protocol.RunDigest from post-run task inputs.
func Build(in Input) protocol.RunDigest {
	d := protocol.RunDigest{
		TicketSID: strings.TrimSpace(in.TicketSID),
		Attempt:   in.Attempt,
		Status:    digestStatus(in),
		Machine:   strings.TrimSpace(in.Machine),
		Agent:     strings.TrimSpace(in.Agent),
		Model:     strings.TrimSpace(in.Model),
		DurationS: max(int64(in.Duration.Round(time.Second)/time.Second), 0),
		Verify:    in.Verify,
		Followups: append([]string(nil), in.Followups...),
	}

	if len(in.Git) > 0 {
		gitMap := make(map[string]string, len(in.Git))
		for repo, gs := range in.Git {
			if stat := FormatGitStatLine(gs); stat != "" {
				gitMap[repo] = stat
			}
		}
		if len(gitMap) > 0 {
			d.Git = gitMap
		}
	}

	msgSource := in.FinalMessage
	if msgSource == "" {
		msgSource = in.Summary
	}
	d.Interface = ExtractInterface(msgSource)
	d.Summary = truncateRunes(strings.TrimSpace(StripANSI(in.Summary)), MaxSummaryChars)

	switch d.Status {
	case protocol.DigestStatusFailed:
		tail := ExtractFailureTail(in.Output, DefaultFailedTailLines)
		classText := strings.Join(tail, "\n")
		if in.Summary != "" {
			if classText != "" {
				classText += "\n" + in.Summary
			} else {
				classText = in.Summary
			}
		}
		cat := strings.TrimSpace(in.FailureCategory)
		if cat == "" {
			cat = ClassifyTaskFailure(FailureClassInput{
				ExitCode: in.ExitCode,
				TimedOut: in.TimedOut,
				Canceled: in.Canceled || in.TaskStatus == protocol.TaskStatusCanceled,
				OOM:      in.OOM,
				Text:     classText,
				WaitErr:  in.WaitErr,
			})
		}
		var failingTests []string
		if parsed, ok := ParseTestOutput(string(in.Output)); ok && len(parsed.Names) > 0 {
			failingTests = parsed.Names
		}
		d.Failure = &protocol.DigestFailure{
			Category: cat,
			Tests:    failingTests,
			Tail:     tail,
		}
	case protocol.DigestStatusNeedsYou:
		qText, ctxLines := ExtractQuestionContext(in.Output, in.Question, DefaultQuestionContextLines)
		if qText != "" || len(ctxLines) > 0 {
			d.Question = &protocol.DigestQuestion{
				Text:    qText,
				Context: ctxLines,
			}
		}
	}

	return d
}

func digestStatus(in Input) string {
	switch in.TaskStatus {
	case protocol.TaskStatusWaitingInput:
		return protocol.DigestStatusNeedsYou
	case protocol.TaskStatusFailed, protocol.TaskStatusCanceled:
		return protocol.DigestStatusFailed
	case protocol.TaskStatusCompleted:
		if strings.TrimSpace(in.TicketSID) != "" {
			return protocol.DigestStatusReview
		}
		return protocol.DigestStatusCompleted
	default:
		if in.Canceled || in.TimedOut || in.WaitErr != nil || in.ExitCode != 0 {
			return protocol.DigestStatusFailed
		}
		if strings.TrimSpace(in.TicketSID) != "" {
			return protocol.DigestStatusReview
		}
		return protocol.DigestStatusCompleted
	}
}

var (
	diffFilesRe = regexp.MustCompile(`(\d+)\s+files?\s+changed`)
	diffInsRe   = regexp.MustCompile(`(\d+)\s+insertions?\(\+\)`)
	diffDelRe   = regexp.MustCompile(`(\d+)\s+deletions?\(-\)`)
)

// FormatGitStatLine converts a protocol.GitSummary into a compact stat line like
// "2 files, +74 -9" or "1 file (untracked)". Returns "" if there are no changes.
func FormatGitStatLine(gs *protocol.GitSummary) string {
	if gs == nil {
		return ""
	}
	if gs.DiffStat != "" {
		lines := strings.Split(strings.TrimSpace(gs.DiffStat), "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if fm := diffFilesRe.FindStringSubmatch(last); fm != nil {
			nFiles, _ := strconv.Atoi(fm[1])
			label := "files"
			if nFiles == 1 {
				label = "file"
			}
			var ins, del int
			if im := diffInsRe.FindStringSubmatch(last); im != nil {
				ins, _ = strconv.Atoi(im[1])
			}
			if dm := diffDelRe.FindStringSubmatch(last); dm != nil {
				del, _ = strconv.Atoi(dm[1])
			}
			switch {
			case ins > 0 && del > 0:
				return fmt.Sprintf("%d %s, +%d -%d", nFiles, label, ins, del)
			case ins > 0:
				return fmt.Sprintf("%d %s, +%d", nFiles, label, ins)
			case del > 0:
				return fmt.Sprintf("%d %s, -%d", nFiles, label, del)
			default:
				return fmt.Sprintf("%d %s", nFiles, label)
			}
		}
	}
	if len(gs.DirtyFiles) > 0 {
		n := len(gs.DirtyFiles)
		if n == 1 {
			return "1 file (dirty)"
		}
		return fmt.Sprintf("%d files (dirty)", n)
	}
	if len(gs.Commits) > 0 {
		n := len(gs.Commits)
		if n == 1 {
			return "1 commit"
		}
		return fmt.Sprintf("%d commits", n)
	}
	return ""
}

var interfaceLineRe = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?(?:\*\*)?(?:changed\s+interfaces?|interfaces?|contract)(?:\*\*)?\s*:\s*(.+)$`)

// ExtractInterface extracts an optional changed-interface note from the agent's final message.
func ExtractInterface(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	for _, line := range strings.Split(StripANSI(msg), "\n") {
		if m := interfaceLineRe.FindStringSubmatch(line); m != nil {
			val := strings.TrimSpace(m[1])
			val = strings.Trim(val, `"'*`)
			if val != "" && !strings.EqualFold(val, "none") && !strings.EqualFold(val, "n/a") {
				return truncateRunes(val, 140)
			}
		}
	}
	return ""
}
