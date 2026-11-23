# Agent chat on subscriptions (design)

Status: active (pre-alpha, initiative KON-TY1XP). Evidence: `docs/research/agent-cli-capabilities.md`.

## Goal

Chat with the user's subscription-backed agent CLIs (claude, claude2, grok, agy, cursor,
copilot, codex) from the Konsert UI, on any worker, with no API key. The chat must look and
behave like a real chat: streaming replies, tool-call cards, "working / needs you / done /
failed" status, the agent asking for decisions, and failures explained (exit code, re-login
needed, usage window exhausted until a time, ...).

**Failure condition for the whole effort:** chatting with an installed, logged-in agent on
the hub's own machine returns an error or asks for an API key.

## Core idea: drive structured protocols, not TTYs

Every supported CLI has a structured headless protocol that gives us real signals. konsertd
runs **one long-lived engine process per chat** and normalizes its output into
`protocol.AgentChatEvent` (`internal/protocol/agentchat.go`).

| Agent | Engine | Launch | Signals we get |
|---|---|---|---|
| claude (any account) | `claudeEngine` | `claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages [--resume ID] --model M --effort E --permission-mode X --permission-prompt-tool stdio --mcp-config <json>` | `initialize` control request → models + efforts + account; `stream_event` deltas; `assistant` / `user` (tool_use / tool_result); `result` (is_error, terminal_reason, usage, cost); `rate_limit_event` (status, resetsAt, rateLimitType, utilization); `control_request can_use_tool` (permission elicitation); control requests `set_permission_mode`, `set_model`, `interrupt` |
| grok, agy, copilot, cursor | `acpEngine` | `grok agent stdio`, `agy acp`, `copilot --acp`, `cursor-agent acp` | `initialize` (authMethods), `session/new` (sessionId, models with efforts, modes/configOptions), `session/prompt` → `session/update` (agent_message_chunk, agent_thought_chunk, tool_call, tool_call_update, plan), `session/request_permission` (elicitation), stopReason; `session/load` / resume; `session/set_mode`, `session/set_model`; JSON-RPC errors (auth required) |
| codex | `codexEngine` | `codex app-server` | `account/read`, `model/list`, `account/rateLimits/read` + `account/rateLimits/updated`, thread/turn events, approval requests (exec/patch) |
| mock | `mockEngine` | in-process | deterministic events for tests (accounts, models, elicitations, errors) |

A "chat session" therefore is a persistent agent session, not a series of disconnected
command lines; when the engine process has to restart (idle timeout, crash, konsertd restart,
model change that can't be applied live) it resumes the CLI's own session id
(`AgentChatOpen.AgentSessionID`).

## Flow

```
PWA ──ws chat.send/respond/control──▶ hub ──agent.chat.prompt/respond/control──▶ konsertd
PWA ◀──ws agent.chat.event (fan-out)── hub ◀────────── agent.chat.event ────────── konsertd
                                        │ persists events + transcript
                                        └ /mcp (fleet tools + ask_user, per-chat token)
```

* The hub owns chat persistence (sessions, transcript, events) and routing to the worker that
  hosts the chat. On `chat.send` it ensures the engine is open (`agent.chat.open`, idempotent)
  then sends `agent.chat.prompt`.
* konsertd's `chat.Manager` maps ChatID → Session, assigns per-chat `Seq`, enforces one turn
  at a time, closes idle engines after 15 min (resume on next prompt), and closes everything
  on shutdown. Events are queued as control messages (never dropped like PTY frames).
* Browser subscriptions use the existing `chat.subscribe` scoping.

## Status model (the chat pill)

| Status | Set when |
|---|---|
| `starting` | engine launching / initialize in progress |
| `working` | a turn was accepted, until turn_end/error |
| `needs_you` | an elicitation is pending (permission, question, auth) |
| `done` | turn_end with end_turn (and `idle` once the UI has shown it) |
| `failed` | EventError ended the turn |
| `cancelled` | the user interrupted |

Watchdog: a turn with no events for 10 min emits `ErrKindTimeout` (the engine stays alive;
the user can cancel or wait). "Working" also shows the latest tool-call title as a subtitle.

## Elicitation: how the agent asks the user

1. **Permission requests** (approve a command / edit): claude `can_use_tool` control requests
   (via `--permission-prompt-tool stdio`), ACP `session/request_permission`, codex approval
   requests. Normalized to `EventElicitation{Kind: permission, Options: allow_once /
   allow_always / reject_once ...}`; the user's `agent.chat.respond` is translated back to the
   agent's reply format. Which tool calls ask depends on the approval mode.
2. **Questions**: the hub exposes an `ask_user(question, options[], allow_text)` MCP tool on a
   per-chat Streamable HTTP endpoint handed to the agent via `AgentChatOpen.MCPServers`
   (claude `--mcp-config`, ACP `session/new mcpServers`, codex `-c mcp_servers`). The tool call
   blocks until the user answers in the chat; the agent's system prompt tells it to use
   `ask_user` when it needs a decision. A turn that simply ends with a question is `done`
   (the user replies normally) — the UI highlights it as "replied with a question".
3. **Auth**: `EventElicitation{Kind: auth}` carries `AgentRelogin` (command + profile env +
   URL); the UI offers "Open terminal on <host> and run it" (a shell task with the account env).

## Failure classification

Engines classify every failure into `AgentChatError.Kind` with actionable fields:

| Kind | Detected from | UI |
|---|---|---|
| `auth` | `claude auth status` loggedIn=false; stream-json result/assistant text "Please run /login", "OAuth token has expired", 401; ACP JSON-RPC auth-required error or `authMethods` + failure; codex `account/read` null / 401 | "Claude 2 needs you to sign in again" + relogin command/terminal |
| `usage_limit` | claude `rate_limit_event` status `rejected` (rateLimitType five_hour / seven_day / seven_day_opus, resetsAt); codex rateLimits `rateLimitReachedType`; text patterns "usage limit", "limit reached", "quota exceeded", "resets at ..." | "Out of 5-hour usage until 3:40 PM" (window + local time); suggest another account |
| `rate_limit` | transient 429 / overloaded | "Busy, retrying" (`Retryable`) |
| `exit` | process exited unexpectedly: exit code + stderr tail (redacted) | "claude exited with code 1: ..." |
| `not_installed` | binary missing | install hint |
| `model` | unknown model id | pick another model |
| `timeout` | watchdog | cancel / keep waiting |
| `protocol` | malformed protocol output | bug report detail |

Zero exit with an error inside the stream (e.g. claude `result.is_error=true`) is still a
failure: classification looks at the stream, not just the exit code.

## Accounts (multiple subscriptions)

`AgentAccount` = one agent CLI + an environment profile. konsertd discovers each installed
CLI's default account and loads extras from its YAML config:

```yaml
accounts:
  - id: claude2
    agent: claude
    label: Claude 2
    env:
      CLAUDE_CONFIG_DIR: ~/.claude2
      CLAUDE_SECURESTORAGE_CONFIG_DIR: ~/.claude2
    token_file: ~/.claude2-oauth-token     # → CLAUDE_CODE_OAUTH_TOKEN (0600 enforced)
```

An importer recognizes wrapper scripts like `claude2` (exec of claude with
`CLAUDE_CONFIG_DIR` + token file) and offers the equivalent account. Secrets stay on the
worker. Keychain-bound CLIs (cursor, agy, copilot via gh) can't separate accounts by directory
on macOS; document and surface that.

Capabilities (`agent.capabilities`) are probed at startup, every 10 min, on demand
(`agent.capabilities.refresh`), and after a chat fails with `auth`/`usage_limit`. Usage sources:
claude `GET /api/oauth/usage` (opt-out; falls back to stream `rate_limit_event`), codex
`account/rateLimits/read`, copilot cache file; others report `usage_note`.

## Orchestrator = an agent chat with fleet tools

The orchestrator is just an agent chat whose `MCPServers` include Konsert's fleet tools
(dispatch, status, steer, ...) and whose `SystemPrompt` sets the orchestrator role. The
API-key provider backend remains as an optional engine for users who want it, but it is never
the default and never required.

## Persistence (hub)

Chat sessions gain: `backend` (agent|api), `worker_id`, `agent`, `account_id`, `model`,
`effort`, `approval_mode`, `cwd`, `agent_session_id`, `status`, `fleet_tools`. Events are
stored as an append-only `chat_events` table (seq, kind, json) from which the transcript is
rebuilt; deltas are coalesced into messages on turn end to keep storage small.

## Definition of done

Hands-on, with no API key configured: on bocamacmini, chat with claude, claude2, grok, agy,
cursor and copilot (tiny prompts); see models/effort/approval modes in the composer; trigger a
permission elicitation and answer it; see codex reported as logged out with re-login help;
see footer usage for claude. Automated: mock-engine e2e covering the same flow.
