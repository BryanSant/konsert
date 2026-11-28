package chat

// Per-agent adapters for the ACP engine: launch argv, approval-mode mapping, relogin
// commands, model fallbacks and the session files a probe leaves behind.
//
// Verified on macOS (2026-11-23) against grok 1.0.92, agy 1.5.14, copilot 1.0.114 and
// cursor-agent 2026.11.22 with initialize + session/new + set_mode/set_model/
// set_config_option/session/close (no prompts).

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// acpApproval maps one Syre approval mode to the agent's own controls.
type acpApproval struct {
	Mode protocol.AgentApprovalMode
	// ACPMode is applied with session/set_mode ("" = leave the session's mode alone).
	ACPMode string
	// Config values are applied with session/set_config_option (configId → value).
	Config map[string]string
	// Launch marks modes that need launch flags (see acpAdapter.argv); changing to or
	// from such a mode restarts the process and resumes the session.
	Launch bool
}

type acpLaunch struct {
	Approval string // Syre approval mode id
	Model    string // only for agents that take the model at launch (copilot)
	Effort   string // only for agents that take the effort at launch (copilot)
}

type acpAdapter struct {
	Agent           string
	Label           string
	DefaultBinary   string
	Approvals       []acpApproval
	DefaultApproval string
	// argv builds the arguments after the binary.
	argv func(l acpLaunch) []string
	// LaunchModel means model/effort are launch flags (no set_model over ACP).
	LaunchModel bool
	// VersionArgs reads the CLI version when initialize does not report it.
	VersionArgs []string
	// sessionMeta is merged into the _meta of session/new, session/load and session/resume
	// for the given approval mode (grok: yoloMode / autoMode).
	sessionMeta func(approval string) map[string]any
	// FallbackModels is used when session/new lists no models.
	FallbackModels []protocol.AgentModel
	// relogin is the fix for a logged-out account (bin = the account's binary name).
	relogin        func(bin string, init *acpInitResult) string
	ReloginNote    string
	ReloginURL     string
	ReloginEnvKeys []string
	UsageNote      string
	// sessionPaths lists the files a session with this id/cwd leaves in the CLI's home.
	sessionPaths func(home string, env map[string]string, sessionID, cwd string) []string
	// statusCheck is an optional cheap CLI auth check (state, email, detail).
	statusCheck func(ctx context.Context, acct Account) (state, email, detail string)
	// usage is an optional plan-usage fetch (SYR-W1GXW): one vendor HTTP call or a short
	// CLI run. It runs only for a signed-in account, on its own schedule (acpUsageTTL),
	// apart from the heavy ACP handshake.
	usage func(ctx context.Context, acct Account) acpUsage
	// ForkMethod is the agent's own session-fork method when it doesn't advertise the
	// standard session/fork (grok: _x.ai/session/fork {sourceSessionId, sourceCwd,
	// newCwd} -> {newSessionId}).
	ForkMethod string
}

func (a *acpAdapter) approval(id string) (acpApproval, bool) {
	if id == "" {
		id = a.DefaultApproval
	}
	for _, ap := range a.Approvals {
		if ap.Mode.ID == id {
			return ap, true
		}
	}
	return acpApproval{}, false
}

// approvalForACPMode maps an agent-reported mode back to a Syre approval id, keeping
// the current one when it already maps to that mode (several modes share an ACP mode).
func (a *acpAdapter) approvalForACPMode(mode string, current string) string {
	if cur, ok := a.approval(current); ok && cur.ACPMode == mode {
		return cur.Mode.ID
	}
	for _, ap := range a.Approvals {
		if ap.ACPMode == mode && !ap.Launch {
			return ap.Mode.ID
		}
	}
	return ""
}

func (a *acpAdapter) modes() []protocol.AgentApprovalMode {
	out := make([]protocol.AgentApprovalMode, 0, len(a.Approvals))
	for _, ap := range a.Approvals {
		out = append(out, ap.Mode)
	}
	return out
}

func mode(id, label, desc string, risk int, icon string) protocol.AgentApprovalMode {
	return protocol.AgentApprovalMode{ID: id, Label: label, Description: desc, Risk: risk, Icon: icon}
}

// acpAdapters is the registry of ACP agents.
var acpAdapters = map[string]*acpAdapter{
	"grok":    grokAdapter,
	"agy":     agyAdapter,
	"copilot": copilotAdapter,
	"cursor":  cursorAdapter,
}

// ACPAgents lists the agent names the ACP engine serves.
func ACPAgents() []string { return []string{"grok", "agy", "copilot", "cursor"} }

// grok: `grok agent stdio` (`grok agent --always-approve stdio` for always-approve).
//
// grok's ACP session/new advertises no modes; session/set_mode accepts "default", "plan",
// "agent" and "ask" (anything else is "unknown mode id"), but only "plan" changes
// anything visible. The approval policy is the documented session _meta: yoloMode
// (always-approve) and autoMode (auto-review) on session/new, session/load and
// session/resume. grok has no accept-edits mode ("auto" is its nearest). The top-level
// --permission-mode flag is not honoured by `grok agent`.
//
//	plan            risk 0  set_mode plan, _meta {yoloMode:false, autoMode:false}
//	default (Ask)   risk 1  _meta {yoloMode:false, autoMode:false}
//	auto            risk 3  _meta {yoloMode:false, autoMode:true}
//	always-approve  risk 4  _meta {yoloMode:true} + `agent --always-approve`
//
// Because _meta only applies when a session is created or attached, changing the mode
// restarts the process and resumes the session (session/resume, no history replay).
// Caveat seen on the test host: ~/.grok/config.toml had [ui] permission_mode =
// "always-approve", and grok then wrote files without asking even with yoloMode:false
// (and with GROK_DEFAULT_PERMISSION_MODE=ask), so on such hosts "Ask" may not ask.
//
// Model and effort switch live: session/set_model and set_config_option
// "reasoning_effort" (efforts per model come from _meta.reasoningEfforts).
var grokAdapter = &acpAdapter{
	Agent: "grok", Label: "Grok", DefaultBinary: "grok", ForkMethod: "_x.ai/session/fork",
	Approvals: []acpApproval{
		{Mode: mode("plan", "Plan", "Read-only: Grok plans and asks before changing anything.", 0, "eye"), ACPMode: "plan", Launch: true},
		{Mode: mode("default", "Ask", "Ask before edits and commands (unless grok's own config always approves).", 1, "hand"), ACPMode: "default", Launch: true},
		{Mode: mode("auto", "Auto-review", "A reviewer model approves routine calls; risky ones ask.", 3, "shield-check"), ACPMode: "default", Launch: true},
		{Mode: mode("always-approve", "Always approve", "Run every tool without asking (yolo).", 4, "shield-alert"), Launch: true},
	},
	DefaultApproval: "default",
	argv: func(l acpLaunch) []string {
		if l.Approval == "always-approve" {
			return []string{"agent", "--always-approve", "stdio"}
		}
		return []string{"agent", "stdio"}
	},
	sessionMeta: func(approval string) map[string]any {
		switch approval {
		case "always-approve":
			return map[string]any{"yoloMode": true}
		case "auto":
			return map[string]any{"yoloMode": false, "autoMode": true}
		default:
			return map[string]any{"yoloMode": false, "autoMode": false}
		}
	},
	relogin:        func(bin string, _ *acpInitResult) string { return bin + " login" },
	ReloginNote:    "On a host without a browser run `grok login --device-auth`.",
	ReloginEnvKeys: []string{"GROK_HOME"},
	UsageNote:      "Grok shows plan usage only in its TUI (/usage).",
	sessionPaths: func(home string, env map[string]string, id, cwd string) []string {
		root := env["GROK_HOME"]
		if root == "" {
			root = filepath.Join(home, ".grok")
		}
		var out []string
		for _, c := range cwdVariants(cwd) {
			out = append(out, filepath.Join(root, "sessions", url.PathEscape(c), id))
			if strings.Contains(filepath.Base(c), acpProbePrefix) {
				// Our own scratch cwd: drop its whole folder (prompt_history.jsonl).
				out = append(out, filepath.Join(root, "sessions", url.PathEscape(c)))
			}
		}
		return out
	},
}

// agy: `agy acp [--dangerously-skip-permissions]`.
//
// agy's ACP modes (session/set_mode): default, accept-edits, auto, plan.
//
//	plan              risk 0  set_mode plan
//	default           risk 1  set_mode default (ask before edits and commands)
//	accept-edits      risk 2  set_mode accept-edits
//	auto              risk 3  set_mode auto (edits + safe commands; asks before irreversible ones)
//	skip-permissions  risk 4  launch --dangerously-skip-permissions
//
// Models carry the effort in the id (gemini-4-neon-high); set_model on the variant.
// opts.SystemPrompt goes to session/new _meta.antigravity.systemPrompt.append.
var agyAdapter = &acpAdapter{
	Agent: "agy", Label: "Antigravity", DefaultBinary: "agy",
	Approvals: []acpApproval{
		{Mode: mode("plan", "Plan", "Plan carefully before executing; no changes without approval.", 0, "eye"), ACPMode: "plan"},
		{Mode: mode("default", "Ask", "Ask before edits and commands.", 1, "hand"), ACPMode: "default"},
		{Mode: mode("accept-edits", "Accept edits", "Approve file edits in the workspace without asking.", 2, "edit"), ACPMode: "accept-edits"},
		{Mode: mode("auto", "Auto", "Approve edits and safe commands; ask before irreversible actions.", 3, "shield-check"), ACPMode: "auto"},
		{Mode: mode("skip-permissions", "Full access", "Auto-approve every tool request (--dangerously-skip-permissions).", 4, "shield-alert"), ACPMode: "default", Launch: true},
	},
	DefaultApproval: "default",
	argv: func(l acpLaunch) []string {
		if l.Approval == "skip-permissions" {
			return []string{"acp", "--dangerously-skip-permissions"}
		}
		return []string{"acp"}
	},
	relogin:     func(bin string, _ *acpInitResult) string { return bin + " acp --login" },
	ReloginNote: "Or run `agy` and use /login. The Google login is a per-user Keychain item.",
	usage:       agyUsage,
	sessionPaths: func(home string, _ map[string]string, id, _ string) []string {
		base := filepath.Join(home, ".gemini", "antigravity-cli")
		db := filepath.Join(base, "conversations", id+".db")
		return []string{db, db + "-wal", db + "-shm", filepath.Join(base, "brain", id), filepath.Join(base, "annotations", id+".pbtxt")}
	},
}

const copilotModeURL = "https://agentclientprotocol.com/protocol/session-modes#"

// copilot: `copilot --acp [--model M] [--reasoning-effort E]`.
//
// copilot's ACP modes (session/set_mode, also config option "mode") are URLs ending in
// #agent, #plan, #autopilot; config option "allow_all" (on/off) auto-approves tools.
//
//	plan       risk 0  set_mode #plan, allow_all off
//	agent      risk 1  set_mode #agent, allow_all off (asks for tools, paths, URLs)
//	allow-all  risk 4  set_mode #agent, allow_all on (never asks)
//	autopilot  risk 4  set_mode #autopilot (enables allow-all, runs to completion)
//
// session/new lists no models, so the model and effort are launch flags (verified not to
// persist to ~/.copilot/config.json); changing them restarts with session/load.
var copilotAdapter = &acpAdapter{
	Agent: "copilot", Label: "Copilot", DefaultBinary: "copilot",
	usage: copilotUsage,
	Approvals: []acpApproval{
		{Mode: mode("plan", "Plan", "Plan multi-step work before executing.", 0, "eye"), ACPMode: copilotModeURL + "plan", Config: map[string]string{"allow_all": "off"}},
		{Mode: mode("agent", "Agent", "Ask before using tools, paths or URLs.", 1, "hand"), ACPMode: copilotModeURL + "agent", Config: map[string]string{"allow_all": "off"}},
		{Mode: mode("allow-all", "Allow all", "Approve every tool, path and URL request without asking.", 4, "shield-alert"), ACPMode: copilotModeURL + "agent", Config: map[string]string{"allow_all": "on"}},
		{Mode: mode("autopilot", "Autopilot", "Autonomous: allow-all and run until the task is complete (experimental).", 4, "shield-alert"), ACPMode: copilotModeURL + "autopilot"},
	},
	DefaultApproval: "agent",
	LaunchModel:     true,
	argv: func(l acpLaunch) []string {
		out := []string{"--acp"}
		if l.Model != "" && l.Model != "auto" {
			out = append(out, "--model", l.Model)
		}
		if l.Effort != "" {
			out = append(out, "--reasoning-effort", l.Effort)
		}
		return out
	},
	FallbackModels: []protocol.AgentModel{{
		ID: "auto", DisplayName: "Auto", IsDefault: true,
		Description: "Copilot picks the model (the CLI lists no models headlessly).",
	}},
	relogin: func(bin string, init *acpInitResult) string {
		if init != nil {
			for _, m := range init.AuthMethods {
				if cmd, ok := m.terminalAuth(); ok {
					return cmd
				}
			}
		}
		return bin + " login"
	},
	ReloginNote:    "Copilot also uses the GitHub CLI login (gh auth login) or COPILOT_GITHUB_TOKEN.",
	ReloginEnvKeys: []string{"COPILOT_HOME", "GH_CONFIG_DIR", "GH_HOST", "COPILOT_GH_HOST"},
	sessionPaths: func(home string, env map[string]string, id, _ string) []string {
		root := env["COPILOT_HOME"]
		if root == "" {
			root = filepath.Join(home, ".copilot")
		}
		return []string{filepath.Join(root, "session-state", id)}
	},
}

// cursor: `cursor-agent [--force] acp`.
//
// cursor's ACP modes (session/set_mode): agent, plan, ask.
//
//	ask          risk 0  set_mode ask (Q&A, no edits or commands)
//	plan         risk 0  set_mode plan (read-only planning)
//	agent        risk 1  set_mode agent (asks per the cli-config allowlist)
//	full-access  risk 4  launch --force (= --yolo, run everything not denied), set_mode agent
//
// Models: one bracket id per model; only listed ids are accepted (a bracket id with a
// different effort is "Invalid model value"), so effort is fixed per entry. Note:
// session/set_model and --model persist to ~/.cursor/cli-config.json (Cursor's own
// behaviour), so a model picked in a chat becomes the CLI's default.
var cursorAdapter = &acpAdapter{
	Agent: "cursor", Label: "Cursor", DefaultBinary: "cursor-agent",
	Approvals: []acpApproval{
		{Mode: mode("ask", "Ask", "Q&A only: no edits or command execution.", 0, "eye"), ACPMode: "ask"},
		{Mode: mode("plan", "Plan", "Read-only planning and design before implementation.", 0, "eye"), ACPMode: "plan"},
		{Mode: mode("agent", "Agent", "Full agent; asks before commands outside the allowlist.", 1, "hand"), ACPMode: "agent"},
		{Mode: mode("full-access", "Full access", "Run everything not explicitly denied (--force).", 4, "shield-alert"), ACPMode: "agent", Launch: true},
	},
	DefaultApproval: "agent",
	VersionArgs:     []string{"--version"},
	argv: func(l acpLaunch) []string {
		if l.Approval == "full-access" {
			return []string{"--force", "acp"}
		}
		return []string{"acp"}
	},
	relogin: func(bin string, _ *acpInitResult) string {
		return bin + " login"
	},
	ReloginNote:    "Set NO_OPEN_BROWSER=1 to print the login URL instead of opening a browser.",
	ReloginEnvKeys: []string{"CURSOR_CONFIG_DIR", "AGENT_CLI_CREDENTIAL_STORE", "CURSOR_DATA_DIR"},
	usage:          cursorUsage,
	sessionPaths: func(home string, env map[string]string, id, cwd string) []string {
		root := env["CURSOR_CONFIG_DIR"]
		if root == "" {
			root = filepath.Join(home, ".cursor")
		}
		out := []string{filepath.Join(root, "acp-sessions", id)}
		for _, c := range cwdVariants(cwd) {
			if strings.Contains(filepath.Base(c), acpProbePrefix) {
				out = append(out, filepath.Join(root, "projects", strings.Trim(strings.ReplaceAll(c, "/", "-"), "-")))
			}
		}
		return out
	},
	statusCheck: cursorStatus,
}

// cursorStatus runs `cursor-agent status --format json`. It exits 0 whether or not the
// user is logged in, so only the JSON decides.
func cursorStatus(ctx context.Context, acct Account) (state, email, detail string) {
	out, _, err := RunQuick(ctx, acct.Binary, append(append([]string{}, acct.ArgsPrefix...), "status", "--format", "json"), acct.Env, 20*time.Second)
	if err != nil {
		return protocol.AuthUnknown, "", ""
	}
	return parseCursorStatus(out)
}

func parseCursorStatus(out string) (state, email, detail string) {
	i := strings.IndexByte(out, '{')
	if i < 0 {
		return protocol.AuthUnknown, "", ""
	}
	var st struct {
		Status          string `json:"status"`
		IsAuthenticated *bool  `json:"isAuthenticated"`
		Message         string `json:"message"`
		UserInfo        *struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	}
	if unmarshal([]byte(out[i:]), &st) != nil {
		return protocol.AuthUnknown, "", ""
	}
	if st.UserInfo != nil {
		email = st.UserInfo.Email
	}
	switch {
	case st.IsAuthenticated != nil && *st.IsAuthenticated, st.Status == "authenticated":
		return protocol.AuthOK, email, ""
	case st.IsAuthenticated != nil, st.Status == "unauthenticated":
		return protocol.AuthLoggedOut, "", st.Message
	}
	return protocol.AuthUnknown, "", ""
}

const acpProbePrefix = "syre-acp-probe-"

// cwdVariants returns cwd and its symlink-resolved form (/var/tmp vs /private/var/tmp).
func cwdVariants(cwd string) []string {
	out := []string{cwd}
	if r, err := filepath.EvalSymlinks(cwd); err == nil && r != cwd {
		out = append(out, r)
	}
	return out
}

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{3,127}$`)

// removeSessionFiles deletes what a (probe) session left in the CLI's home. Only paths
// derived from the exact session id (and the probe's own scratch cwd) are touched.
func (a *acpAdapter) removeSessionFiles(acct Account, sessionID, cwd string) []string {
	if a.sessionPaths == nil || !safeSessionID.MatchString(sessionID) || strings.Contains(sessionID, "..") {
		return nil
	}
	home := acct.Env["HOME"]
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = h
	}
	var removed []string
	for _, p := range a.sessionPaths(home, acct.Env, sessionID, cwd) {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if os.RemoveAll(p) == nil {
			removed = append(removed, p)
			// Drop a per-cwd parent the session created (grok sessions/<cwd>/).
			parent := filepath.Dir(p)
			if strings.Contains(parent, url.PathEscape(acpProbePrefix)) || strings.Contains(parent, acpProbePrefix) {
				_ = os.Remove(parent)
			}
		}
	}
	return removed
}

// reloginFor builds the AgentRelogin for an account.
func (a *acpAdapter) reloginFor(acct Account, init *acpInitResult) *protocol.AgentRelogin {
	bin := a.DefaultBinary
	if acct.Binary != "" {
		bin = filepath.Base(acct.Binary)
		bin = strings.TrimSuffix(bin, filepath.Ext(bin))
	}
	r := &protocol.AgentRelogin{Command: a.relogin(bin, init), Note: a.ReloginNote, URL: a.ReloginURL}
	for _, k := range a.ReloginEnvKeys {
		if v, ok := acct.Env[k]; ok && v != "" {
			if r.Env == nil {
				r.Env = map[string]string{}
			}
			r.Env[k] = v
		}
	}
	if v, ok := acct.Env["HOME"]; ok && v != "" {
		if r.Env == nil {
			r.Env = map[string]string{}
		}
		r.Env["HOME"] = v
	}
	return r
}

func (a *acpAdapter) String() string { return fmt.Sprintf("acp(%s)", a.Agent) }
