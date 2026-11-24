package worker

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// HookInstallReport summarizes idempotent Konsert agent hook/skill installation on a host.
type HookInstallReport struct {
	ClaudeHooksInstalled bool     `json:"claude_hooks_installed"`
	AgySkillInstalled    bool     `json:"agy_skill_installed"`
	NotifyScriptPath     string   `json:"notify_script_path"`
	EventsFilePath       string   `json:"events_file_path"`
	Notes                []string `json:"notes,omitempty"`
}

// HookEvent is written atomically by `konsertd hook-event` (or notify.sh) to ~/.konsert/hook-events.jsonl.
type HookEvent struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Event     string `json:"event"` // "PreToolUse" | "PostToolUse" | "Notification" | "Stop" | "working" | "needs_you" | "done" | "fail"
	Cwd       string `json:"cwd,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	Model     string `json:"model,omitempty"`
	Message   string `json:"message,omitempty"`
	// Tool is the tool a PreToolUse/PostToolUse hook fired for (from the hook's stdin JSON).
	Tool string `json:"tool,omitempty"`
	// TaskID is set when the hook fired inside a konsertd-managed task (KONSERT_TASK_ID).
	TaskID    string    `json:"task_id,omitempty"`
	Timestamp time.Time `json:"ts"`
}

// ManagedTaskEnv is set in the environment of every konsertd-managed task process. Global
// agent hooks (installed by install-hooks) see it and do not report the managed task as an
// external session: the task already reports through its own PTY/telemetry pipeline.
const ManagedTaskEnv = "KONSERT_TASK_ID"

// HookEventsPath returns the local JSONL inbox path where agent hooks append real-time telemetry.
func HookEventsPath(homeDir string) string {
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return filepath.Join(homeDir, ".konsert", "hook-events.jsonl")
}

// RecordHookEvent appends a single HookEvent JSON line to ~/.konsert/hook-events.jsonl cleanly.
//
// Events from inside a konsertd-managed task (ev.TaskID or $KONSERT_TASK_ID set) are not
// recorded, so a managed agent never shows up a second time as an external session.
func RecordHookEvent(homeDir string, ev HookEvent) error {
	if ev.TaskID == "" {
		ev.TaskID = strings.TrimSpace(os.Getenv(ManagedTaskEnv))
	}
	if ev.TaskID != "" {
		return nil
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	if ev.Cwd == "" {
		ev.Cwd, _ = os.Getwd()
	}
	if ev.SessionID == "" {
		ev.SessionID = fmt.Sprintf("%s-%d", ev.Agent, os.Getppid())
	}
	path := HookEventsPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ev, protocol.JSONOptions)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// InstallAgentHooks idempotently installs lightweight Konsert telemetry hooks/skills for
// Claude Code, Antigravity/Gemini, and a portable notify.sh shim for Codex/Grok/custom agents.
func InstallAgentHooks(homeDir, konsertdBin string) (HookInstallReport, error) {
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return HookInstallReport{}, err
		}
	}
	if strings.TrimSpace(konsertdBin) == "" {
		konsertdBin = "konsertd"
	}

	report := HookInstallReport{
		EventsFilePath: HookEventsPath(homeDir),
	}

	// 1. Portable ~/.konsert/hooks/notify.sh shim (for Codex, Grok, custom shells/agents).
	hooksDir := filepath.Join(homeDir, ".konsert", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return report, err
	}
	notifyPath := filepath.Join(hooksDir, "notify.sh")
	notifyScript := fmt.Sprintf(`#!/usr/bin/env bash
# Konsert lightweight agent telemetry hook (managed by konsertd install-hooks)
exec %s hook-event "$@" >/dev/null 2>&1 || true
`, shellQuote(konsertdBin))
	if err := os.WriteFile(notifyPath, []byte(notifyScript), 0o755); err != nil {
		return report, err
	}
	report.NotifyScriptPath = notifyPath

	// 2. Claude Code (~/.claude/settings.json): our entries edited into the JSON text in
	// place, with a backup first (claude_settings.go).
	if err := installClaudeHooks(homeDir, konsertdBin); err != nil {
		report.Notes = append(report.Notes, "claude hooks skipped: "+err.Error())
	} else {
		report.ClaudeHooksInstalled = true
	}

	// 3. Antigravity / Gemini skill (~/.gemini/config/skills/konsert-telemetry/SKILL.md).
	if err := installAgyTelemetrySkill(homeDir, konsertdBin); err != nil {
		report.Notes = append(report.Notes, "agy skill skipped: "+err.Error())
	} else {
		report.AgySkillInstalled = true
	}

	return report, nil
}

// shellQuote quotes s for POSIX sh (Claude Code runs hook commands through a shell), so a
// konsertd path containing spaces or metacharacters stays one word.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%=,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoveAgentHooks takes out what InstallAgentHooks added: the Claude Code hook entries
// (the rest of settings.json stays as it is), the Antigravity skill and the notify shim.
func RemoveAgentHooks(homeDir string) (HookInstallReport, error) {
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return HookInstallReport{}, err
		}
	}
	report := HookInstallReport{EventsFilePath: HookEventsPath(homeDir)}
	changed, err := removeClaudeHooks(homeDir)
	if err != nil {
		return report, err
	}
	if changed {
		report.Notes = append(report.Notes, "removed the Konsert hooks from ~/.claude/settings.json")
	}
	for _, p := range []string{
		filepath.Join(homeDir, ".gemini", "config", "skills", "konsert-telemetry", "SKILL.md"),
		filepath.Join(homeDir, ".konsert", "hooks", "notify.sh"),
	} {
		if err := os.Remove(p); err == nil {
			report.Notes = append(report.Notes, "removed "+p)
		} else if !os.IsNotExist(err) {
			return report, err
		}
	}
	_ = os.Remove(filepath.Join(homeDir, ".gemini", "config", "skills", "konsert-telemetry"))
	return report, nil
}

func installAgyTelemetrySkill(homeDir, konsertdBin string) error {
	skillDir := filepath.Join(homeDir, ".gemini", "config", "skills", "konsert-telemetry")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`---
name: konsert-telemetry
description: Report lightweight session milestones to the local konsertd daemon when running outside the Konsert UI.
---

# Konsert Telemetry Hook

When running on a Konsert-managed machine outside the Konsert UI, you can emit concise status transitions (working, needs_you, done, fail) with zero overhead:

`+"```bash"+`
%s hook-event --agent agy --event working --cwd "$PWD"
%s hook-event --agent agy --event done --cwd "$PWD"
`+"```"+`
`, shellQuote(konsertdBin), shellQuote(konsertdBin))
	return os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644)
}
