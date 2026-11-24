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
                                        └ per-chat MCP (fleet tools + ask_user, per-chat token)
                                          relayed by konsertd over its link: agent ──http──▶
                                          127.0.0.1:<port>/mcp/chat/<id> ──mcp.* frames──▶ hub
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
2. **Questions**: the hub exposes an `ask_user` MCP tool on a per-chat Streamable HTTP
   endpoint handed to the agent via `AgentChatOpen.MCPServers` (claude `--mcp-config`, ACP
   `session/new mcpServers`, codex `-c mcp_servers`). The endpoint is relayed (see "Per-chat
   MCP relay" below), so the agent talks to konsertd on loopback, never to the hub. The tool call blocks until the user
   answers; the agent's system prompt tells it to use `ask_user` and how to ask well. A turn
   that simply ends with a question is `done` (the user replies normally) — the UI highlights
   it as "replied with a question". Questions are hub objects in a fleet-wide queue; see
   "Questions and side conversations" below.
3. **Auth**: `EventElicitation{Kind: auth}` carries `AgentRelogin` (command + profile env +
   URL); the UI offers "Open terminal on <host> and run it" (a shell task with the account env).

## Questions and side conversations (PRD §5.8b)

* **Validation.** `ask_user` takes `question`, `context` (what it is about, with ticket/PR
  titles), `why_now` (what is blocked, what happens next), `options[]` of `{label,
  consequence, recommended}`, `recommendation` + `rationale`, `allow_text`, `category`,
  `blocks[]`. Missing context, why_now, a consequence, or the rationale for a recommendation
  returns a tool error listing exactly what to add (recorded in `question_rejects` for the
  quality metrics). The agents' own question and permission prompts are mirrored as
  questions too (no validation possible) and shown through the same card.
* **Persistence.** `questions` rows (`q-…` sids) stay open until answered; an identical re-ask
  from the same chat maps to the same card (blocked time keeps running, `ask_count` grows).
  When the agent's tool call ends without an answer (its MCP client timed out, the hub
  restarted, the engine was restarted) the question stays open and the answer is delivered
  as a `[Konsert]` note: as a new turn when the chat is idle, otherwise with its next prompt.
* **MCP tool-call timeouts** (the hub passes `AgentMCPServer.ToolTimeoutSeconds` = ask_user
  wait + 10 min; the hub's own wait is 30 min by default):

  | Agent | Default tool-call limit | How Konsert raises it |
  |---|---|---|
  | claude 2.1.x | 60 s for HTTP servers: the fetch first-byte budget is `max(server timeout or MCP_TOOL_TIMEOUT, 60 s)` — this is what ended ask_user after ~60 s | `MCP_TOOL_TIMEOUT` (ms) in the child env and per-server `"timeout"` (ms) in `--mcp-config`. Verified live: a question stayed open 3 min 40 s and was answered in-turn. `-p` sessions don't auto-background MCP calls. |
  | codex | `mcp_servers.<name>.tool_timeout_sec`, 60 s default | `tool_timeout_sec` in the `config` of `thread/start`/`thread/resume`/`thread/fork` |
  | grok | per-server `tool_timeout_sec` in `~/.grok/config.toml` (no session-level knob confirmed) | none; the note fallback covers it |
  | copilot | per-server `timeout` (ms) in its own MCP config; no deadline on tool calls by default (inferred) | none needed |
  | agy | 180 s (`timeoutSeconds` in its `mcp_config.json` only) | the hub returns "still open" after 170 s |
  | cursor-agent | 60 s fixed (MCP SDK default; no override for HTTP servers) | the hub returns "still open" after 55 s |

  While a Konsert tool (`ask_user`, `request_user_action`) runs, the engines' no-progress
  watchdog stays quiet.
* **Side conversation ("Ask about this").** A hidden agent chat (`chat_sessions.question_id`)
  answers the user's clarifying questions; it runs through the normal chat machinery and is
  never listed. When the asking account reports `AgentAccount.Fork`, it is a read-only fork
  of the asking session on the same machine and account (`AgentChatOpen.ForkFrom`, read-only
  approval mode, no Konsert tools), so the original stays blocked and undisturbed:

  | Agent | Fork |
  |---|---|
  | claude | `--resume <orig> --fork-session --session-id <new>` with stream-json input, `--permission-mode plan` (verified on 2.1.330, including while the original waits in ask_user). The fork's transcript is deleted on discard. |
  | codex | app-server `thread/fork {threadId, ephemeral: true}` (response `thread.id`), read-only sandbox |
  | grok | `_x.ai/session/fork {sourceSessionId, sourceCwd, newCwd}` → `newSessionId`, then `session/resume` (verified live) |
  | ACP agents advertising `sessionCapabilities.fork` | `session/fork` |
  | agy, copilot, cursor | none over ACP → fallback |
  | mock | in-process fork that inherits the original's prompts |

  Otherwise (or when the fork fails before answering) the orchestrator's agent answers with a
  context pack: the question, the asking chat's goal, recent transcript excerpt (from the
  hub's own chat events), files it touched and the resolved references. The answerer may end
  with `DRAFT ANSWER: …` ("Use this answer"). On answering, it writes the one-sentence summary
  delivered with the answer ("The user asked …; after learning …, chose … because …"); the
  hidden chat is then discarded.
* **Delegation.** "Let the orchestrator decide" hands the question to the orchestrator chat,
  which must call `answer_question` (choice + rationale). The decision is held for a grace
  period (default 30 s) so the user can override it; a later override reaches the agent as a
  correction note. Per-category auto-delegation rules are off by default.
* **References.** ka ids (`ka show <id> --json --offline` on the hub host), PR numbers (`gh pr
  view` when the chat's repo has a GitHub remote), file paths (what the session did with them)
  and Konsert sids become cards, resolved after the question is shown and cached 2 min.

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

## Per-chat MCP relay (KON-DTN92)

Agents reach their chat's Konsert tools through konsertd, over the link konsertd already
has with the hub (outbound WebSocket, SSH or the local socket), so no machine needs a
network path to the hub's port and the hub can stay on 127.0.0.1.

* **Open.** The hub sends the server as `{name: konsert, url: /mcp/chat/<chat id>, relay:
  true, headers: {Authorization: Bearer <chat token>}}`. konsertd starts its relay (once per
  daemon: `127.0.0.1`, an ephemeral port, kept for the daemon's lifetime so engines that
  outlive a link keep a working URL) and rewrites the URL to
  `http://127.0.0.1:<port>/mcp/chat/<chat id>` before the engine sees it. The relay answers
  nothing but `GET`/`POST`/`DELETE` on `/mcp/chat/<id>`.
* **Frames.** Each request becomes `mcp.request {id, chat_id, method, header, body}` on the
  link (`id`: 128 random bits chosen by konsertd; only the MCP headers cross: Authorization,
  Accept, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID, User-Agent;
  body up to 4 MiB). The hub runs it through the chat's MCP handler and answers
  `mcp.head {id, status, header}`, `mcp.body {id, data}` chunks (sent whenever the handler
  flushes, at most 32 KiB each, so SSE streams flow as they are written) and
  `mcp.end {id, error}`. konsertd sends `mcp.cancel {id}` when the agent goes away, which
  cancels the handler (e.g. a pending `ask_user`). Requests are multiplexed by id; hub frames
  wait for room in the link's send queue rather than being dropped.
* **Auth.** Unchanged in strength, narrower in reach: the chat's bearer token is checked
  exactly as before (deleting the chat revokes it), and the hub also requires that the chat
  runs on the machine whose link carried the request. The hub has no per-chat MCP endpoint
  on its HTTP listener any more.
* **Link drops.** A request in flight when its link ends fails at once: `503` with
  `Retry-After` if the response had not started, a cut connection if it had (so the agent
  sees an error, not a short answer). A request made while konsertd has no link waits up to
  20 s for the next one, then gets `503`. MCP clients retry; the hub's MCP sessions are
  in-memory, so after a hub restart the client re-initializes as it would on any `404`.
* **Every transport.** Outbound machines use the relay too: one path for all machines, and
  `public_url` is no longer needed for agents' tools.

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
