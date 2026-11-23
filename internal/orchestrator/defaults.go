package orchestrator

import (
	"errors"
	"strings"
)

// ErrNoModel is returned by the API-key providers when no model is configured.
// There are deliberately no built-in model defaults: the subscription-backed agent
// chat (the default orchestrator) takes its models from each agent CLI's reported
// capabilities, and the optional API-key backend needs an explicit llm_model.
var ErrNoModel = errors.New("no model configured for the API-key orchestrator backend: set llm_model in Settings (agent chats need no API key or model setting)")

// DefaultMaxTokens is the output-token ceiling sent to Anthropic (where
// max_tokens is required) when none is configured. Responses are streamed, so a
// generous ceiling does not risk HTTP timeouts, and adaptive thinking on current
// Claude models counts against it. OpenAI and Gemini only receive a limit when
// one is configured explicitly, because compatible servers disagree on the
// parameter name and on acceptable ranges.
const DefaultMaxTokens = 32000

// RolePrompt is the orchestrator role, shared by the agent-chat orchestrator (the
// default, running on a logged-in agent CLI) and the optional API-key backend.
const RolePrompt = `You are Konsert Orchestrator, coordinating an entire fleet of SSH-connected machines as one logical system.
Token Efficiency Rules:
- ALWAYS prefer deterministic fleet tools (fleet_status, inspect_machine_facts, list_tasks, get_task_result, get_taxonomy, recall_memory) over spawning coding agents for inspection, file discovery, git status, or cleanup scans.
- Delegate to coding agents (dispatch_task) ONLY for non-deterministic engineering work (writing/refactoring code, debugging, running complex multi-step plans).
- Use cached Machine Facts to place work on the right machine (OS, hardware/load, virtualization, installed tools like postgres 18 or gcloud auth readiness).
- Persist important cross-thread fleet context or task handoff notes with save_memory, and rely on compact structured summaries rather than raw terminal output.
Be concise, structured, and action-oriented.`

// apiToolsPrompt adds the daemon RPC tools only the API-key backend exposes.
const apiToolsPrompt = `
- The daemon RPC tools inspect_git_status, list_remote_dir, scan_temp_worktrees and cleanup_worktree answer inspection questions at zero agent cost.`

// AskUserPrompt tells an agent chat how to ask the user for a decision. Every
// agent chat gets it, because the hub serves ask_user to every agent chat.
const AskUserPrompt = `You are running inside Konsert, which shows this conversation in a chat window.
When you need a decision or information from the user before you can continue, call the "ask_user" tool of the "konsert" MCP server (question, optional options, allow_text) and wait for its answer instead of guessing or ending your turn with a question.`

// agentFleetPrompt explains how the agent chat reaches the fleet tools.
const agentFleetPrompt = `Your fleet tools are served by the "konsert" MCP server (fleet_status, inspect_machine_facts, dispatch_task, list_tasks, get_task_result, steer_task, cancel_task, get_taxonomy, manage_taxonomy, fan_out_fleet, get_settings, recall_memory, save_memory, list_audit). Use them to act on the fleet; your own shell and file tools only touch the machine you run on.`

// AgentSystemPrompt returns the system prompt appended to an agent chat's own
// prompt. fleet selects the orchestrator role (fleet tools available).
func AgentSystemPrompt(fleet bool) string {
	if !fleet {
		return AskUserPrompt
	}
	return strings.Join([]string{RolePrompt, agentFleetPrompt, AskUserPrompt}, "\n\n")
}

func normalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "openai"
	}
	return p
}
