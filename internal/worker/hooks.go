package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent"`
	Event     string    `json:"event"` // "PreToolUse" | "PostToolUse" | "Notification" | "Stop" | "working" | "needs_you" | "done" | "fail"
	Cwd       string    `json:"cwd,omitempty"`
	FilePath  string    `json:"file_path,omitempty"`
	Model     string    `json:"model,omitempty"`
	Message   string    `json:"message,omitempty"`
	Timestamp time.Time `json:"ts"`
}

// HookEventsPath returns the local JSONL inbox path where agent hooks append real-time telemetry.
func HookEventsPath(homeDir string) string {
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	return filepath.Join(homeDir, ".konsert", "hook-events.jsonl")
}

// RecordHookEvent appends a single HookEvent JSON line to ~/.konsert/hook-events.jsonl cleanly.
func RecordHookEvent(homeDir string, ev HookEvent) error {
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
	b, err := json.Marshal(ev)
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
exec %q hook-event "$@" >/dev/null 2>&1 || true
`, konsertdBin)
	if err := os.WriteFile(notifyPath, []byte(notifyScript), 0o755); err != nil {
		return report, err
	}
	report.NotifyScriptPath = notifyPath

	// 2. Claude Code (~/.claude/settings.json) — merge Konsert hooks without clobbering existing settings!
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

const konsertHookMarker = "konsertd hook-event"

func installClaudeHooks(homeDir, konsertdBin string) error {
	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return err
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	root := map[string]any{}
	if raw, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	}

	hooksMap, ok := root["hooks"].(map[string]any)
	if !ok || hooksMap == nil {
		hooksMap = map[string]any{}
	}

	events := []string{"PreToolUse", "PostToolUse", "Notification", "Stop"}
	for _, evName := range events {
		cmdStr := fmt.Sprintf("%s hook-event --agent claude --event %s", konsertdBin, evName)
		existing, _ := hooksMap[evName].([]any)
		if hasKonsertHookEntry(existing) {
			continue
		}
		entry := map[string]any{
			"matcher": "*",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": cmdStr,
				},
			},
		}
		hooksMap[evName] = append(existing, entry)
	}
	root["hooks"] = hooksMap

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0o600)
}

func hasKonsertHookEntry(entries []any) bool {
	for _, item := range entries {
		b, err := json.Marshal(item)
		if err == nil && strings.Contains(string(b), konsertHookMarker) {
			return true
		}
	}
	return false
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
`, konsertdBin, konsertdBin)
	return os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644)
}
