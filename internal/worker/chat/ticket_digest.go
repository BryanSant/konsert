package chat

import (
	"context"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/gitutil"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/digest"
)

const maxTurnToolOutputBytes = 16 * 1024

// ProvisionedWorktrees is returned by ManagerOptions.ProvisionBindings after setting up
// per-repo worktrees under <root>/<ticket sid>/<repo>.
type ProvisionedWorktrees struct {
	Workdir   string
	Worktrees []protocol.TaskWorktree
	BaseSHAs  map[string]string
}

func captureWorktreeGit(worktrees []protocol.TaskWorktree, baseSHAs map[string]string) map[string]*protocol.GitSummary {
	if len(worktrees) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := make(map[string]*protocol.GitSummary, len(worktrees))
	for _, wt := range worktrees {
		if wt.WorktreePath == "" {
			continue
		}
		if gs, _ := gitutil.CaptureSummary(ctx, wt.WorktreePath, baseSHAs[wt.RepoName]); gs != nil {
			out[wt.RepoName] = gs
		}
	}
	return out
}

func mapChatErrorCategory(e *protocol.AgentChatError) string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case protocol.ErrKindAuth,
		protocol.ErrKindUsageLimit,
		protocol.ErrKindRateLimit,
		protocol.ErrKindTimeout,
		protocol.ErrKindOOM,
		protocol.ErrKindDiskFull,
		protocol.ErrKindMissingTool,
		protocol.ErrKindVerify,
		protocol.ErrKindCanceled,
		protocol.ErrKindModel,
		protocol.ErrKindAgentError:
		return e.Kind
	case protocol.ErrKindNotFound:
		return protocol.ErrKindMissingTool
	default:
		// Let digest.ClassifyTaskFailure inspect ExitCode + stderr/message (e.g. OOM, disk_full, agent_error).
		return ""
	}
}

func appendTurnToolOutput(b *strings.Builder, s string) {
	if s == "" || b.Len() >= maxTurnToolOutputBytes {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	rem := maxTurnToolOutputBytes - b.Len()
	if len(s) > rem {
		s = s[:rem]
	}
	b.WriteString(s)
}

// buildTurnDigestLocked builds a compact RunDigest for ticket-backed chat events.
// c.mu must be held.
func (m *Manager) buildTurnDigestLocked(c *chatState, ev protocol.AgentChatEvent, gitByRepo map[string]*protocol.GitSummary, vr *VerifyResult) *protocol.RunDigest {
	ticketSID := strings.TrimSpace(c.opts.TicketSID)
	if ticketSID == "" {
		return nil
	}

	finalMsg := strings.TrimSpace(c.turnLastMessage)
	if finalMsg == "" {
		finalMsg = strings.TrimSpace(c.turnMessageBuf.String())
	}
	var duration time.Duration
	if !c.turnStartedAt.IsZero() {
		duration = m.opts.Now().Sub(c.turnStartedAt)
	}
	model := c.opts.Model

	switch ev.Kind {
	case protocol.EventTurnEnd:
		taskStatus := protocol.TaskStatusCompleted
		canceled := false
		failureCat := ""
		switch ev.StopReason {
		case "cancelled":
			taskStatus = protocol.TaskStatusCanceled
			canceled = true
		case "error":
			taskStatus = protocol.TaskStatusFailed
		}
		outText := c.turnToolOutput.String()
		if outText == "" {
			outText = finalMsg
		}
		var verifyRes *protocol.DigestVerify
		if vr != nil && !vr.Skipped && vr.Verify != nil {
			verifyRes = vr.Verify
			if vr.Verify.Exit != 0 {
				taskStatus = protocol.TaskStatusFailed
				failureCat = protocol.ErrKindVerify
				outText = string(vr.Output)
			}
		}
		d := digest.Build(digest.Input{
			TicketSID:       ticketSID,
			Attempt:         c.turnAttempt,
			TaskStatus:      taskStatus,
			Machine:         m.opts.MachineName,
			Agent:           c.opts.Agent,
			Model:           model,
			Duration:        duration,
			Canceled:        canceled,
			FailureCategory: failureCat,
			Summary:         finalMsg,
			FinalMessage:    finalMsg,
			Output:          []byte(outText),
			Git:             gitByRepo,
			Verify:          verifyRes,
		})
		return &d

	case protocol.EventError:
		if ev.Error != nil && ev.Error.Kind == protocol.ErrKindTimeout && c.turnActive {
			// Watchdog warning keeps the turn running; not a terminal failure digest.
			return nil
		}
		var (
			errMsg   string
			stderr   string
			exitCode int
			cat      string
		)
		if ev.Error != nil {
			errMsg = ev.Error.Message
			stderr = ev.Error.Stderr
			if ev.Error.ExitCode != nil {
				exitCode = *ev.Error.ExitCode
			}
			cat = mapChatErrorCategory(ev.Error)
		}
		outText := stderr
		if outText == "" {
			outText = c.turnToolOutput.String()
		}
		if outText == "" {
			outText = errMsg
		}
		summary := errMsg
		if summary == "" {
			summary = finalMsg
		}
		d := digest.Build(digest.Input{
			TicketSID:       ticketSID,
			Attempt:         c.turnAttempt,
			TaskStatus:      protocol.TaskStatusFailed,
			Machine:         m.opts.MachineName,
			Agent:           c.opts.Agent,
			Model:           model,
			Duration:        duration,
			ExitCode:        exitCode,
			FailureCategory: cat,
			Summary:         summary,
			FinalMessage:    finalMsg,
			Output:          []byte(outText),
			Git:             gitByRepo,
		})
		return &d

	case protocol.EventElicitation:
		if ev.Elicitation == nil {
			return nil
		}
		q := strings.TrimSpace(ev.Elicitation.Title)
		if detail := strings.TrimSpace(ev.Elicitation.Detail); detail != "" && detail != q {
			if q != "" {
				q += ": " + detail
			} else {
				q = detail
			}
		}
		d := digest.Build(digest.Input{
			TicketSID:  ticketSID,
			Attempt:    c.turnAttempt,
			TaskStatus: protocol.TaskStatusWaitingInput,
			Machine:    m.opts.MachineName,
			Agent:      c.opts.Agent,
			Model:      model,
			Duration:   duration,
			Question:   q,
			Output:     []byte(finalMsg),
		})
		return &d
	}
	return nil
}
