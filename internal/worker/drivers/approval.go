package drivers

import (
	"regexp"
	"strings"
)

// Approval modes and effort for PTY tasks.
//
// LaunchOptions.ApprovalMode is one of the agent's own approval mode IDs (as reported in
// AgentAccount.Approvals). When it is set it replaces the approval flags that
// PermissionMode would add (--dangerously-skip-permissions and friends); PermissionMode
// still decides sandboxing. Unknown modes are ignored (PermissionMode behaviour applies),
// and full-access modes are refused by sandboxed workers (see ApprovalModeFullAccess).

var flagValueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// flagValue returns v when it is a safe single flag value, else "".
func flagValue(v string) string {
	v = strings.TrimSpace(v)
	if !flagValueRe.MatchString(v) {
		return ""
	}
	return v
}

// fullAccessModes are approval modes that never ask and/or disable the sandbox.
var fullAccessModes = map[string][]string{
	"claude":  {"bypassPermissions"},
	"grok":    {"bypassPermissions"},
	"codex":   {"full-access", "danger-full-access", "yolo"},
	"agy":     {"full-access", "bypass", "yolo", "dangerously-skip-permissions"},
	"cursor":  {"full-access", "force", "yolo"},
	"copilot": {"full-access", "allow-all", "yolo"},
}

// ApprovalModeFullAccess reports whether mode gives the agent full access (never asks),
// which sandboxed workers refuse.
func ApprovalModeFullAccess(agent, mode string) bool {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return false
	}
	for _, m := range fullAccessModes[agent] {
		if strings.EqualFold(m, mode) {
			return true
		}
	}
	return strings.EqualFold(mode, "full-access")
}

// claudeStyleModes are the --permission-mode values of claude and grok.
var claudeStyleModes = map[string]bool{
	"default": true, "acceptEdits": true, "auto": true, "dontAsk": true, "bypassPermissions": true, "plan": true, "manual": true,
}

// permissionModeFlag returns the claude/grok --permission-mode value for mode ("" = none).
func permissionModeFlag(mode string) string {
	mode = flagValue(mode)
	if claudeStyleModes[mode] {
		return mode
	}
	return ""
}

// codexApproval maps a codex approval mode to its approval policy and sandbox; bypass
// means --dangerously-bypass-approvals-and-sandbox. ok is false for unknown modes.
func codexApproval(mode string) (policy, sandbox string, bypass, ok bool) {
	switch flagValue(mode) {
	case "read-only":
		return "on-request", "read-only", false, true
	case "auto", "workspace-write", "on-request", "default":
		return "on-request", "workspace-write", false, true
	case "untrusted":
		return "untrusted", "workspace-write", false, true
	case "never":
		return "never", "workspace-write", false, true
	case "full-access", "danger-full-access", "yolo":
		return "", "", true, true
	}
	return "", "", false, false
}
