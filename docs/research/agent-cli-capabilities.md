> Research snapshot (2026-11-23) of what each agent CLI exposes for login status, models,
> usage/quota, multiple accounts, headless mode, approval modes and effort levels.
> Gathered read-only on one macOS host; personal account details removed. Versions drift:
> re-verify flags against `--help` before relying on them.

# Syre: subscription-first agent CLI research (macOS, 2026-11-22/23)

Machine: the user's Mac (darwin arm64), run locally (not over SSH). Read-only with respect to accounts: no logins, logouts or config changes.
Emails are reduced to their domain. No token values were printed. Account names, org IDs and team IDs are left out.

Probing method: "logged-out" behaviour was observed by pointing each CLI at an empty scratch config dir
(`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GROK_HOME`, `CURSOR_CONFIG_DIR` + `AGENT_CLI_CREDENTIAL_STORE=memory`, `COPILOT_HOME`)
under `/var/tmp/syre-research/`. That dir was deleted at the end.

Prompts sent: **one** in total, to `copilot` ("reply OK"). It was meant as a logged-out probe, but copilot found the
user's GitHub login anyway (see the Copilot section). It cost 1 premium request and produced the usage-record schema below.
The claude/codex/grok/cursor logged-out `-p` probes failed authentication before reaching any model.

---

## Summary matrix

| CLI | Auth-status method | Models method | Usage / quota method | Multi-account mechanism | Headless mode | MCP injection |
|---|---|---|---|---|---|---|
| **claude** 2.1.330 | `claude auth status` (JSON default, `--text`). Exit 0 = logged in, **1 = not logged in**. Fields: `loggedIn`, `authMethod`, `loginExpiresAt`, `subscriptionType`, `planTier` | SDK `initialize` control request over `-p --input-format stream-json` returns `models[]` with displayName, description and effort levels (**no prompt needed**). Also `--model` aliases `default/opus/sonnet/haiku/fable` plus full IDs | `GET https://api.anthropic.com/api/oauth/usage` with the OAuth bearer token (what Claude Code itself calls). Shape: `five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`, … each `{utilization, resets_at}`. In-stream: `rate_limit_event`. Statusline JSON: `rate_limits.five_hour/seven_day.{used_percentage,resets_at}` | `CLAUDE_CONFIG_DIR` (+ `CLAUDE_SECURESTORAGE_CONFIG_DIR`), or `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`. `claude2` uses both | `-p --output-format stream-json --verbose [--include-partial-messages] [--input-format stream-json]` | `--mcp-config <file/json…>` + `--strict-mcp-config` |
| **claude2** (wrapper) | same (`claude2 auth status`). Shows `authMethod:"oauth_token"`, `loginExpiresAt:null` | same | same, but the setup-token is probably inference-scoped, so `/api/oauth/usage` may refuse it (unverified) | config dir `~/.claude2`, token file `~/.claude2-oauth-token` | same | same |
| **codex** 0.171.0 | `codex login status`. Exit 0 = logged in, **1 = "Not logged in"** . `codex app-server` → `account/read` | `codex debug models` (raw JSON catalog, offline) or app-server `model/list` (displayName, description, isDefault, per-model efforts) | app-server `account/rateLimits/read` → `primary`/`secondary` `{usedPercent, windowDurationMins, resetsAt}`, plus credits and planType. Pushed as `account/rateLimits/updated`. Backend: `chatgpt.com/backend-api/wham/usage` | `CODEX_HOME` (auth.json inside; `cli_auth_credentials_store = file/keyring/auto`) | `codex exec --json` (JSONL) or `codex app-server` (JSON-RPC v2, the richer option) | `-c mcp_servers.<n>.command=…` overrides or config.toml / `codex mcp add` |
| **grok** 1.0.92 | No status command. `grok models` prints "You are logged in with grok.com." vs "You are not authenticated." (**exit 0 in both cases**; parse the text). File: `~/.grok/auth.json` `expires_at` | `grok models` (text). `~/.grok/models_cache.json` (rich: name, description, context_window, reasoning_efforts) | **Not headless in this version**: `grok usage` → "can't show your plan's usage here yet. Run /usage inside grok instead." (exit 1). `grok usage <session_id>` gives per-session tokens and cost | `GROK_HOME` | `grok -p "…" --output-format streaming-json` (or `json`, `streaming-messages-json`). `grok agent stdio` = ACP | No flag. `[mcp_servers.*]` in `$GROK_HOME/config.toml`; project `.mcp.json` (Claude-compatible) |
| **agy** 1.5.14 | No status command. Credential in Keychain service `gemini`, account `antigravity`. Log line `keyringAuth: loaded token, expiry=… expired=false` in `~/.gemini/antigravity-cli/log/cli-*.log` | `agy models` (id + display name, effort baked into the ID). Calls `v1internal:fetchAvailableModels` | No CLI command. The CLI calls `daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary` (per-model buckets, quota_remaining / resets). 429 text: "Individual quota reached … Resets in 2h27m4s" | None documented for the Google login (global Keychain item). Alternatives: `AGY_ADC_AUTH` (ADC) or `GEMINI_API_KEY`. `AGY_ACCOUNT` is for plugin accounts only | `agy -p … --output-format stream-json [--input-format stream-json]`. `agy acp` | `~/.gemini/config/mcp_config.json` `{mcpServers}`, `agy mcp add`. Maybe `--settings` JSON |
| **cursor-agent** 2026.11.19 (cask; `about` reports 2026.11.22) | `cursor-agent status --format json` → `{status:"authenticated"/"unauthenticated", isAuthenticated, hasAccessToken, hasRefreshToken, userInfo}`. **Exit 0 in both cases** | `cursor-agent models` / `--list-models` (text `id - Display Name`, 168 entries, current default marked) | No CLI command. CLI's RPC: `aiserver.v1.DashboardService/GetCurrentPeriodUsage` on `api2.cursor.sh` → `plan_usage{total_spend, included_spend, remaining, limit, auto/api/total_percent_used}`, billing cycle start/end | `CURSOR_CONFIG_DIR` (config only). On macOS tokens live in global Keychain items `cursor-access-token`/`cursor-refresh-token`. For isolation: `AGENT_CLI_CREDENTIAL_STORE=file` (file is `$HOME/.cursor/auth.json`, so HOME override too) or `CURSOR_API_KEY` | `-p --output-format stream-json [--stream-partial-output]` | No flag. `.cursor/mcp.json` (workspace) or `$CURSOR_CONFIG_DIR/mcp.json` + `--approve-mcps` |
| **copilot** 1.0.114 | No status command. Cache `~/Library/Caches/copilot/copilot-user-cache.json` (plan + quota snapshot). Log: "Signed in to https://github.com as …" | No headless list. `/model` is interactive. ACP (`--acp`) is likely. Auto-mode event lists `availableModels` | Cache file `quota_snapshots.{chat,completions,premium_interactions}.{entitlement,remaining,percent_remaining,unlimited,overage_*}` + `quota_reset_date`. Per run: `--usage-output-file`, JSONL `session.usage_record`. Session/weekly limits: `/usage` (interactive) | `COPILOT_HOME`. Per-process `COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN`. `/user` manages several accounts in one home | `-p "…" --output-format json` (JSONL) `[-s] [--stream on]`. `--acp` | `--additional-mcp-config <json or @file>` (repeatable); `--disable-builtin-mcps` |

### Matrix part 2 (addendum): permissions, effort, model metadata

| CLI | Approval / permission modes | Mid-session change | Effort / reasoning | Model metadata available |
|---|---|---|---|---|
| claude | `--permission-mode acceptEdits/auto/bypassPermissions/manual/dontAsk/plan`; `--dangerously-skip-permissions`; `--allow-dangerously-skip-permissions` (makes bypass selectable but not on); `--allowedTools`/`--disallowedTools` rules; `--permission-prompts host/none` (with -p: `none` auto-denies anything that would prompt); `--permission-prompt-tool` (an MCP tool answers prompts) | Yes. stream-json control requests `set_permission_mode`, `set_model`, `set_max_thinking_tokens`, `interrupt`, `mcp_set_servers`, `apply_flag_settings`. Shift+Tab in the TUI | `--effort low/medium/high/xhigh/max` (env `CLAUDE_EFFORT` seen in env). Levels depend on the model (`supportedEffortLevels`; e.g. Opus/Sonnet 4.6 lack `xhigh`, Haiku 4.5 has none) | **Best of all**: `initialize` → `value, resolvedModel, displayName, description, supportsEffort, supportedEffortLevels, supportsAdaptiveThinking, supportsFastMode, supportsAutoMode, supportsThinkingOff`. `value:"default"` marks the default |
| codex | `-a/--ask-for-approval untrusted/on-request/never` (schema also has a `granular` object); `-s/--sandbox read-only/workspace-write/danger-full-access`; `--approve-for-me` (auto-review in workspace-write); `--dangerously-bypass-approvals-and-sandbox` (yolo). Config keys `approval_policy`, `approvals_reviewer` | Yes, through app-server: `turn/start` takes `approvalPolicy`, `sandboxPolicy`, `model`, `effort` per turn | `-c model_reasoning_effort=<level>` (also `plan_mode_reasoning_effort`, `model_reasoning_summary`). Levels per model: e.g. gpt-6.x `low/medium/high/xhigh/max/ultra`, gpt-6-luna without ultra, gpt-5.5 `low…xhigh` | `model/list`: `id, displayName, description, isDefault, defaultReasoningEffort, supportedReasoningEfforts[], hidden, inputModalities, serviceTiers`. `debug models` adds `context_window` |
| grok | `--permission-mode default/acceptEdits/auto/dontAsk/bypassPermissions/plan`; `--always-approve`; `--allow`/`--deny` rules (`Bash(…)`, `Edit(…)`, `MCPTool(…)`, …); `--sandbox off/workspace/read-only/strict` or a custom profile from `sandbox.toml` (env `GROK_SANDBOX`) | TUI: yes (modes). Headless: per invocation. ACP probably exposes modes (not verified) | `--reasoning-effort`/`--effort`. Per-model list in models_cache (`grok-4.7`: xhigh/high/medium/low, default high; `grok-4.5`: high/medium/low) | models_cache.json: `name` ("Grok 4.7"), `description`, `context_window`, `reasoning_efforts[{id,label,description,default}]`, `hidden`. `grok models` marks the default |
| agy | `--mode accept-edits/auto/plan` (TUI cycles default → accept-edits → plan with Shift+Tab); `--dangerously-skip-permissions`; `--sandbox` (terminal restrictions) | TUI yes. Headless per invocation | `--effort low/medium/high/xhigh/max`. In practice effort is also **encoded in the model ID** (`gemini-4-neon-high`, `claude-opus-5-5-low`) | `agy models`: id + display name only ("Gemini 4 Neon (High)"). No description or context window |
| cursor-agent | `-f/--force` = `--yolo` (run everything not explicitly denied); `--auto-review` (server classifier auto-runs safe calls); `--sandbox enabled/disabled`; `--mode plan/ask` (read-only), `--plan`; `--approve-mcps`; `--trust` (workspace trust, needed headless). Allow/deny lists in `cli-config.json` `permissions` | TUI only | Through model variants (`claude-opus-5-5-high`, `gpt-5.6-sol-xhigh`, `-fast` suffix) or bracket params `--model 'claude-opus-4-8[context=1m,effort=high,fast=false]'` | `models`: id + display name ("Claude Opus 5.5 1M High"), current/default marked. No description. Context is in the name ("1M") |
| copilot | `--mode interactive/plan/autopilot`; `--allow-all`/`--yolo` (= tools+paths+urls); `--allow-all-tools`, `--allow-tool 'shell(git:*)'`, `--deny-tool` (deny always wins), `--available-tools`/`--excluded-tools`, `--allow-url`/`--deny-url`, `--assisted-approval` (LLM judge). In `-p`/autopilot anything that would prompt is **denied** | Yes in TUI (`/permissions`, `/allow-all`, `/model`, effort applies from the next request) | `--reasoning-effort none/minimal/low/medium/high/xhigh/max` ("supported levels depend on the model") | Interactive `/model` only. Usage records carry `modelDisplayName` and `maxPromptTokens`. No headless catalog found |

### Key caveats

1. **Exit codes are unreliable** for `cursor-agent status` and `grok models`: both exit 0 when logged out, so parse the output. `claude auth status` and `codex login status` do exit 1 when logged out.
2. **Logged-out `codex exec` does not fail fast.** It retries the websocket 5× with `401 Unauthorized` `error` events. Check `codex login status` first.
3. **Keychain-global credentials** (cursor `cursor-access-token`, agy `gemini/antigravity`, copilot falling back to the `gh` login) mean a config-dir override alone does **not** isolate accounts on macOS. Claude (per-config-dir storage plus token env), Codex (`CODEX_HOME`) and Grok (`GROK_HOME`, file-based) isolate cleanly.
4. **Copilot picked up a login even with an empty `COPILOT_HOME` and `GH_CONFIG_DIR`** and with token env vars unset. It ran a model call on the GitHub account. The most likely source is the `gh:github.com` Keychain item. For a per-account Copilot backend, pass `COPILOT_GITHUB_TOKEN` explicitly.
5. **Plan usage headless**: available for Claude (OAuth usage endpoint, undocumented/internal), Codex (app-server `account/rateLimits/read`, a supported protocol) and Copilot (cached `copilot_internal/user`-style response on disk; the endpoint name is inferred, not confirmed in the binary). Grok shows plan usage only in the TUI. Cursor and agy have internal RPCs only (Connect/gRPC-ish, undocumented).
7. claude2's setup-token: `loginExpiresAt:null`. `claude auth status` cannot show expiry for it; a revoked token only surfaces at call time.

---

## 1. claude (primary)

**Binary/version.** `~/.local/bin/claude` → `~/.local/share/claude/versions/2.1.330` (Mach-O arm64). `claude --version` → `2.1.330 (Claude Code)`.

**Auth status (no side effects).**
```
$ claude auth status          # --json is the default; --text available
{ "loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty",
  "configDirectory": "~/.claude", "loginExpiresAt": "2026-12-02T16:36:22.418Z",
  "storedClaudeAiLogin": true, "planTier": "max_20x", ... }      exit 0
$ claude auth status --text
Login method: Claude Ultra account ... Sign-in ends: between 2026-11-29 and 2026-12-02
```
Logged out (empty `CLAUDE_CONFIG_DIR`): `{"loggedIn": false, "authMethod": "none", ...}` **exit 1**. Text form: `Not logged in. Run claude auth login to authenticate.`
`claude -p` logged out: result `{"is_error": true, "result": "Not logged in · Please run /login", "terminal_reason": "api_error"}`, exit 1.

**Credential storage.**
- macOS Keychain generic password, service **`Claude Code-credentials`** (login keychain; created 2026-09-24, modified 2026-11-03). Only metadata was read (no `-w`/`-g`).
- Also `~/.claude/.credentials.json` exists (mode 600). Structure: `claudeAiOauth{accessToken, refreshToken, expiresAt(ms epoch), refreshTokenExpiresAt(ms), scopes[7], subscriptionType, subscriptionTypeSource, rateLimitTier}` and `mcpOAuth{<server>|<hash>: {serverName, serverUrl, accessToken, discoveryState, clientId, redirectUri}}`.
- `~/.claude.json`: `oauthAccount{accountUuid, emailAddress, organizationUuid, billingType, seatTier, planDisplayName, organizationRateLimitTier, userRateLimitTier, hasExtraUsageEnabled, …}`, `additionalModelOptionsCache`, `additionalModelCostsCache` (keys = model IDs).

Re-login: `claude auth login [--claudeai|--console|--sso]` (browser OAuth). Long-lived subscription token: `claude setup-token`.

**Models.** `--model` takes aliases `default`, `opus`, `sonnet`, `haiku`, `fable` or full IDs. The full list with metadata comes **without a prompt** from the SDK `initialize` control request:
```
(echo '{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}'; sleep 8) \
 | claude -p --input-format stream-json --output-format stream-json --verbose
```
The response has `models[]`, `account{email, organization, subscriptionType, apiProvider}`, `current_permission_mode`, `commands`, `agents`, and more. Models seen:

| value | resolvedModel | displayName | description | effort levels |
|---|---|---|---|---|
| default | claude-opus-5-5 | Default (recommended) | Opus 5.5 · Best for everyday, complex tasks | low…max |
| opus | claude-opus-5-5 | Opus 5.5 | For complex work and everyday tasks | low,medium,high,xhigh,max |
| fable | claude-fable-5-2 | Fable 5.2 | For your toughest challenges | low…max |
| sonnet | claude-sonnet-5-5 | Sonnet 5.5 | Most efficient for simpler tasks | low…max |
| haiku | claude-haiku-5-5 | Haiku 5.5 | Fastest for quick answers | low…max |
| claude-fable-5-1, claude-fable-5, claude-opus-5, claude-sonnet-5, claude-opus-4-8, claude-opus-4-7 | | | | low…max |
| claude-opus-4-6, claude-sonnet-4-6 | | | | low,medium,high,max |
| claude-haiku-4-5-20251001 | | Haiku 4.5 | | (no effort) |

Per-model flags: `supportsAdaptiveThinking`, `supportsFastMode`, `supportsAutoMode`, `supportsThinkingOff`.

**Usage/quota.**
- Endpoint in the binary: `/api/oauth/usage` on `https://api.anthropic.com`, with variants `?skip_spend=1` and `?at_wall=1…`. It is called with `Authorization: Bearer <oauth access token>`. The OAuth beta header string in the binary is `oauth-2025-04-20`.
- Response buckets (binary strings): `five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`, `seven_day_oauth_apps`, `seven_day_cowork`, `seven_day_overage_included`, plus `model_scoped[{display_name,…}]`. Each has `utilization` (percent) and `resets_at`.
- Claude Code maps this into the **statusline stdin JSON** as `rate_limits.five_hour.{used_percentage,resets_at}` and `rate_limits.seven_day.{…}` (`resets_at` is epoch seconds), plus `spend_limit` for gateway users.
- **stream-json** emits `{"type":"rate_limit_event","rate_limit_info":{status, resetsAt, rateLimitType, utilization, overageStatus, isUsingOverage, unifiedRateLimitFallbackAvailable}, uuid, session_id}` whenever the info changes. This is headless and free if you are already running a session.
- `/usage` in the TUI is interactive. A third-party tool can call the endpoint with the stored OAuth access token (refreshing it as needed). The endpoint is undocumented and internal.

**Multi-account.** `CLAUDE_CONFIG_DIR=<dir>` (separate `.claude.json`, projects and credentials; `claude auth status` reports `configDirectory`). Alternatively `CLAUDE_CODE_OAUTH_TOKEN=<setup-token>`.

**Headless.** `claude -p "<prompt>" --output-format stream-json --verbose [--include-partial-messages] [--input-format stream-json --replay-user-messages] [--session-id <uuid>] [-r <id>] [--model X] [--effort Y] [--permission-mode Z] [--mcp-config a.json …] [--strict-mcp-config] [--json-schema] [--max-budget-usd] [--no-session-persistence] [--bare]`.
- Message types: `system` (subtypes `init`, `hook_started`, `hook_response`, `commands_changed`), `assistant`, `user`, `stream_event` (partials), `rate_limit_event`, `result` (`is_error`, `result`, `usage`, `total_cost_usd`, `terminal_reason`), `control_request`/`control_response`.
- Note: `--bare` ignores OAuth and Keychain (API key only), so **do not use `--bare`** in subscription mode.

**Session store (2.1.33x, used by observed sessions).**
- `<config dir>/projects/<cwd with every non-alphanumeric as '-'>/<session id>.jsonl` is the transcript, one record per line. `user` records hold prompts (a string or text blocks) and tool results. `assistant` records hold one content block each (`thinking`, `text`, `tool_use`), and every block of a message repeats its `message.id`, `model`, `usage` and `stop_reason` (`tool_use`, `end_turn`, …). `system` has `turn_duration`, `stop_hook_summary`, `compact_boundary` and `local_command`. `ai-title` carries the session title. Noise types include `attachment`, `api-request*`, `data-flow-records` and `file-history-snapshot`. Slash commands are `user` records starting `<command-name>` or `<local-command-…>`, with an `isMeta` caveat before them. An interruption is a user text `[Request interrupted by user…`. Sub-agents write under `<session id>/subagents/`.
- `<config dir>/sessions/<pid>.json` is written by every running claude, interactive or `-p`: `pid`, `sessionId`, `cwd`, `startedAt` (ms), `kind`, `entrypoint` (`cli`, `sdk-cli`), `status` (`busy`, `shell`, `idle`, `waiting`) and `waitingFor` (`input needed`, `dialog open`, `approve <tool>`, …). A permission prompt writes no transcript record, so this file is the only place that state shows. A crashed process can leave its file behind: check that the PID is alive and started before `startedAt`.
- Present on macOS, Linux and Windows. The Windows `claude2.cmd` wrapper sets `CLAUDE_CONFIG_DIR=%USERPROFILE%\.claude2` like the bash one.

## 2. claude2 (second Claude subscription)

`~/.local/bin/claude2` is a bash script (3300 bytes):
- Config dir `${CLAUDE_CONFIG_DIR_OVERRIDE:-~/.claude2}`. It symlinks shared items from `~/.claude`: `settings.json`, `settings.local.json`, `CLAUDE.md`, `keybindings.json`, `orca-agent-view.json`, `hooks`, `skills`, `plugins`. It creates private runtime dirs (projects, sessions, …).
- Token source order: `CLAUDE2_CODE_OAUTH_TOKEN` env, then file `${CLAUDE2_OAUTH_TOKEN_FILE:-~/.claude2-oauth-token}` (mode 600, 109 bytes; it enforces 600), then `CLAUDE2_ANTHROPIC_API_KEY`. With none of these it prints `claude2: no token — set CLAUDE2_CODE_OAUTH_TOKEN or put one in …` and exits 1.
- It execs `env -u CLAUDE_CODE_OAUTH_TOKEN -u ANTHROPIC_API_KEY CLAUDE_CONFIG_DIR=~/.claude2 CLAUDE_SECURESTORAGE_CONFIG_DIR=~/.claude2 CLAUDE_CODE_TMPDIR=/var/tmp/claude2-$UID TMPDIR=… CLAUDE_CODE_OAUTH_TOKEN=<token> ~/.local/bin/claude "$@"`.
- Status: `claude2 auth status` → `{"loggedIn": true, "authMethod": "oauth_token", "configDirectory": "~/.claude2", "loginExpiresAt": null}`. There is no email, plan or tier, because a setup-token carries no profile.
- `~/.claude2/` has its own `.claude.json`, no `.credentials.json`, and no separate Keychain item. Only `Claude Code-credentials` exists, because auth comes from the env token.
- Syre pattern: one "Claude account" = `{CLAUDE_CONFIG_DIR, CLAUDE_CODE_OAUTH_TOKEN?}`. Usage polling for setup-token accounts may not work (scope); `rate_limit_event` from sessions still works.

## 3. codex

**Binary/version.** `~/.local/bin/codex` → `~/.codex/packages/standalone/current/bin/codex` (release `0.171.0-aarch64-apple-darwin`, with `codex-exec-server` and `codex-code-mode-host`). `codex-cli 0.171.0`.

**Auth status.**
```
$ codex login status       → "Not logged in"   exit 1   
```
- There is no `~/.codex/auth.json` and no Keychain item `Codex Auth` (lookup returned "could not be found").
- Storage: `$CODEX_HOME/auth.json` (keys `OPENAI_API_KEY`, `tokens{id_token, access_token, refresh_token, account_id}`, `last_refresh`, from binary strings). Or the OS keyring, depending on `cli_auth_credentials_store` (`file`/`keyring`/`auto`).
- app-server `account/read` while logged out → `{"account": null, "requiresOpenaiAuth": true}`. When logged in: `{type:"chatgpt", email, planType, workspaceName}`. `planType` enum: free, go, plus, pro, prolite, promax, team, business, enterprise, edu, ….
- Logged-out `codex exec --json`: `thread.started`, `turn.started`, then `{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: wss://api.openai.com/v1/responses …)"}` ×5, then failure.
- Re-login: `codex login` (browser), `codex login --device-auth`, `--with-api-key`, `--with-access-token` (stdin). `codex logout`.

**Models.**
- `codex debug models` prints the raw catalog JSON offline (`{"models":[{slug, display_name, description, default_reasoning_level, supported_reasoning_levels[{effort,description}], visibility, context_window, max_context_window, …}]}`). The cache file when logged in is `$CODEX_HOME/models_cache.json` (string in binary).
- app-server `model/list` (works logged out) → `data[]{id, model, displayName, description, isDefault, defaultReasoningEffort, supportedReasoningEfforts[], hidden, inputModalities, serviceTiers, …}`.
- Listed models: gpt-6.2-sol (default; "Fast, go-to model for coding and everyday work."), gpt-6.2-astra ("Our most capable model…"), gpt-6-astra, gpt-6.1-sol, gpt-6.1-terra, gpt-6-sol, gpt-6-luna, gpt-5.6-sol, gpt-5.6-terra, gpt-5.6-luna, gpt-5.5. Hidden: gpt-daybreak-blue/red-latest, codex-auto-review.

**Usage/quota.**
- app-server request `account/rateLimits/read` → `GetAccountRateLimitsResponse` with `RateLimitSnapshot{limitId, limitName, planType, primary, secondary, credits{hasCredits, unlimited, balance}, rateLimitReachedType, individualLimit}`. Each window is `RateLimitWindow{usedPercent:int, windowDurationMins, resetsAt:epoch}` (primary is about 5 h, secondary weekly). Push notification: `account/rateLimits/updated`. Also `account/tokenUsage`.
- Backend endpoints in the binary: `chatgpt.com/backend-api/wham/usage` (plus `wham/rate-limit-reset-credits`).
- Older rollouts JSONL carried `token_count` events with `rate_limits.primary/secondary{used_percent, window_minutes, resets_at}`. Sessions now live in `~/.codex/state_5.sqlite` (`threads` table, 0 rows here).
- Headless and supported: run `codex app-server` (stdio JSON-RPC) per account. The schema is self-describing: `codex app-server generate-json-schema --out DIR` / `generate-ts`.

**Multi-account.** `CODEX_HOME=<dir>` (config.toml, auth.json, sqlite state). The Keychain store may be shared, so use `cli_auth_credentials_store="file"` per home for isolation.

**Headless.**
- `codex exec --json [--model M] [-c model_reasoning_effort=high] [-s workspace-write] [--skip-git-repo-check] [--ephemeral] [-o last.txt] [--output-schema f] "<prompt>"` emits JSONL `thread.started`, `turn.started`, `item.started/updated/completed`, `turn.completed` (usage), `turn.failed`, `error`. `codex exec resume`.
- Richer: `codex app-server` with `thread/start` and `turn/start` (per-turn model, effort, approvalPolicy, sandboxPolicy), approval requests as server→client JSON-RPC (`CommandExecutionRequestApproval`, `FileChangeRequestApproval`), and `DynamicToolCall` (client-provided tools).
- MCP: `-c 'mcp_servers.syre.command="…"' -c 'mcp_servers.syre.args=[…]'` or `codex mcp add`.

## 4. grok

**Binary/version.** `~/.grok/bin/grok` → `../downloads/grok-macos-aarch64`. `grok 1.0.92 (b06756fd6256) [stable]`. Docs: `~/.grok/README.md` (113 KB).

**Auth status.**
```
$ grok models
You are logged in with grok.com.
Default model: grok-4.5
Available models: grok-4.7, grok-4.7-build-fast, grok-4.6, * grok-4.5 (default)     exit 0
GROK_HOME=<empty> grok models → "You are not authenticated." + fallback list (grok-4.6, grok-4.5)   exit 0 (!)
GROK_HOME=<empty> grok -p "…" --output-format json →
 {"type":"error","message":"Not signed in. To authenticate without a browser, run:\n  grok login --device-code\n\nAlternatively, set the XAI_API_KEY environment variable or run `grok login` on a machine with a browser."}   exit 1
```
- File `~/.grok/auth.json` (600): `{"<issuer>::<uuid>": {key, auth_mode:"oidc", create_time, user_id, email, first_name, profile_image_asset_id, principal_type:"User", principal_id, team_id, team_zero_data_retention, coding_data_retention_opt_out, coding_data_team, refresh_token, expires_at:"2026-11-23T11:55:18Z", oidc_issuer:"https://auth.x.ai", oidc_client_id}}`. `expires_at` is short-lived (hours); a refresh token is present.
- `grok models` also refreshes `models_cache.json` over the network (a minor side effect).
- Re-login: `grok login` (browser) or `grok login --device-auth`/`--device-code`; `grok logout`; `grok agent --reauth`. For enterprise: `GROK_AUTH_PROVIDER_COMMAND`, OIDC env.

**Models.** `grok models` (text). `~/.grok/models_cache.json`: `{fetched_at, grok_version, auth_method:"session", origin:"https://cli-chat-proxy.grok.com/v1/models", identity, etag, models{<id>: {info{id, model, model_family, base_url, name, description, context_window, context_windows[], reasoning_effort, supports_reasoning_effort, reasoning_efforts[{id,value,label,description,default}], hidden, supported_in_api, agent_type, …}, api_key, env_key, api_base_url}}}`.
- grok-4.7 "Grok 4.7", ctx 256000, efforts xhigh/high*/medium/low.
- grok-4.7-build-fast "Grok 4.7 Fast", same efforts.
- grok-4.6, same efforts.
- grok-4.5, high*/medium/low.

**Usage.** `grok usage` / `grok usage --json` → `Error: grok usage can't show your plan's usage here yet. Run /usage inside grok instead.` exit 1 (help says it "needs Grok's background service"; no leader was running). `grok usage <SESSION_ID> [TURN]` reads the persisted per-session token and cost usage. Plan usage is TUI `/usage` only. The binary has a `billing` extension (`billing/usage`), but no documented headless route. → **No headless plan quota for grok today.**

**Multi-account.** `GROK_HOME` (documented: "Override config directory (default: ~/.grok)"). The `auth.json` key format `issuer::uuid` suggests it can hold several identities.

**Headless.** `grok -p "<prompt>" --output-format streaming-json [--include-partial-messages] [-m model] [--effort high] [--permission-mode …|--always-approve] [-s <session-uuid>] [-r id] [--max-turns N] [--json-schema]`.
- streaming-json events: `{"type":"text","data":…}`, `{"type":"thought","data":…}`, `{"type":"end","stopReason":"EndTurn","sessionId":…,"requestId":…}`, `{"type":"error","message":…}`. `json` gives a single `{text, stopReason, sessionId, requestId}`. There is also `streaming-messages-json`.
- ACP: `grok agent stdio` (JSON-RPC, ACP; extension methods `x.ai/auth/info`, `x.ai/auth/check_subscription`, …). Also `agent serve`/`headless` (WebSocket).
- MCP: there is no per-run flag. Use `[mcp_servers.<name>]` in `$GROK_HOME/config.toml`, `grok mcp add`, or a project `.mcp.json` (Claude-compatible). It also reads `~/.claude.json` MCP servers. Per run: a temporary `GROK_HOME`, or `.mcp.json` in the cwd.

## 5. agy (Google Antigravity CLI)

**Binary/version.** `/opt/homebrew/bin/agy` (Mach-O, about 229 MB). `agy --version` → `1.5.14`. Data in `~/.gemini/antigravity-cli/` (settings.json, history.jsonl, conversations/, `log/cli-*.log`). Shared config in `~/.gemini/config/` (hooks.json, mcp_config.json, skills).

**Auth status.** No status subcommand (subcommands: acp, agent(s), changelog, install, mcp, mic-serve, models, plugin, remote-control, update).
- Evidence of login: Keychain generic password service **`gemini`**, account **`antigravity`** (modified 2026-11-23T06:17Z).
- Before the keyring loads, the log shows transient "You are not logged into Antigravity." lines. That is the logged-out message.
- Legacy `~/.gemini/oauth_creds.json` (Gemini CLI era) has `access_token, refresh_token, scope, token_type, id_token, expiry_date` (expired March 2026). It is not used by agy.
- Re-login: interactive first-run flow or the `/login` slash command (`/logout`). With `AGY_ADC_AUTH` set it uses Application Default Credentials ("To log out, please run `unset AGY_ADC_AUTH`"). `GEMINI_API_KEY` is supported, but some features say "not available with a Gemini API key".
- A cheap headless check: run `agy models` and look for a non-empty list. Its log calls `v1internal:fetchAvailableModels` only when authenticated.

**Models.** `agy models` (network; "Fetching available models..." then `id<TAB>Display Name`): gemini-4-neon-{low,medium,high,xhigh}, gemini-4-argon-{low,medium,high}, gemini-4-hydrogen-{low,medium,high,xhigh}, gemini-pro-commander-neon ("Commander (Neon)"), gemini-pro-commander-g1-pro ("Commander (Argon)"), gemini-3.8-flash-{low,medium,high}, claude-opus-5-5-{low,medium,high}, claude-sonnet-5-5-{low,medium,high}, gpt-oss-120b-medium. (The exit code was not captured reliably.)

**Usage.**
- The CLI polls `https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary` (854 calls in logs), with protos `QuotaSummary{Group,Bucket,ModelAvailability}`, `quota_buckets`.
- The built-in swarm skill describes `quota_remaining` and `quota_resets` per model, and "AI credits" (`useG1Credits` setting).
- Exhaustion in logs: `RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 2h27m4s.`
- `/quota` and `/usage` strings exist in the binary (interactive). There is no headless command. A third party could call the internal Code Assist endpoint with the user's Google OAuth token: undocumented and fragile.

**Multi-account.** Nothing documented for the main Google login: the Keychain item is per macOS user. `AGY_ACCOUNT=<name>` exists but selects among **plugin-connected provider accounts** (e.g. gh) for sandboxed commands, not the Antigravity login. Options are `AGY_ADC_AUTH` (point at different ADC/service-account creds) or `GEMINI_API_KEY`. HOME-override isolation was not tested.

**Headless.** `agy -p "<prompt>" --output-format stream-json [--input-format stream-json] [--model id] [--effort level] [--mode accept-edits|auto|plan] [--dangerously-skip-permissions] [--print-timeout 0] [--conversation id|-c] [--json-schema] [--settings '{…}']`. stream-json uses NDJSON with `type`, `session_id`, `result`, `usage`, `is_error`, `stop_reason`, `delta` fields (from Go struct tags; exact event list not captured). `agy acp` = ACP stdio server. MCP: `~/.gemini/config/mcp_config.json` `{"mcpServers":{…}}`, `agy mcp add/remove/list`.

## 6. cursor-agent

**Binary/version.** `/opt/homebrew/bin/cursor-agent` → `/opt/homebrew/Caskroom/cursor-cli/2026.11.19-f005e9d/dist-package/cursor-agent` (bash launcher plus bundled node and JS). `--version` → `2026.11.19-f005e9d`. `cursor-agent about` reports CLI Version `2026.11.22-e9042b2`, Subscription Tier <tier>, Model Auto.

**Auth status.**
```
$ cursor-agent status --format json
{ "status":"authenticated","isAuthenticated":true,"hasAccessToken":true,"hasRefreshToken":true,
  "userInfo":{"email":…,"userId":…,"firstName":…,"lastName":…,"teamId":…,"teamName":…,"createdAt":…} }
logged out: {"status":"unauthenticated","isAuthenticated":false,"hasAccessToken":false,"hasRefreshToken":false,"message":"Not logged in"}   exit 0 (!)
logged-out -p: "Error: Authentication required. Please run 'agent login' first, or set CURSOR_API_KEY environment variable."  exit 1
```
- Storage: macOS Keychain services **`cursor-access-token`** and **`cursor-refresh-token`**. `AGENT_CLI_CREDENTIAL_STORE=file` switches to `$HOME/.cursor/auth.json`; `memory` is also allowed.
- Config: `$CURSOR_CONFIG_DIR` → `$XDG_CONFIG_HOME/cursor` → `~/.cursor` (`cli-config.json`: permissions{allow,deny}, model{modelId, displayName, …}, selectedModel, privacyCache, …).
- Re-login: `cursor-agent login` (`NO_OPEN_BROWSER=1` to suppress the browser), `logout`. `--api-key`/`CURSOR_API_KEY` is a Cursor user/service API key, billed against the Cursor plan.

**Models.** `cursor-agent models` or `--list-models` gives 168 lines like `claude-opus-5-5-high - Claude Opus 5.5 1M High`, `auto - Auto (current, default)`. They cover Claude (opus-5-5, opus-5, opus-4-8, sonnet-5, fable-5-2 with low…max, `-fast`, `-thinking` variants), GPT (5.3-codex, 5.5, 5.6-sol/luna), Gemini 4 (neon/carbon/hydrogen/argon), cursor-grok-4.5/4.6, and composer-2.5. There is no JSON format and no descriptions.

**Usage.** No CLI command found. The bundled client defines `aiserver.v1.DashboardService/GetCurrentPeriodUsage` (request: `team_id?`, `include_pooled_usage?`, `personal_scope?`):
- Response: `billing_cycle_start`, `billing_cycle_end`, `plan_usage{total_spend, included_spend, bonus_spend, remaining, limit, auto_spend, api_spend, auto_limit, api_limit, auto_percent_used, api_percent_used, total_percent_used}`, `spend_limit_usage{individual_used/remaining/limit, pooled_*}`, `display_message`.
- Also `GetPlanInfo` and `GetAggregatedUsageEvents`.
- Endpoint base is `https://api2.cursor.sh` (Connect RPC) with the access token. This is internal and undocumented.

**Multi-account.** `CURSOR_CONFIG_DIR` alone is not enough on macOS (global Keychain items). Use `AGENT_CLI_CREDENTIAL_STORE=file` + `HOME=<per-account>` (auth file path is `$HOME/.cursor/auth.json`), or per-account `CURSOR_API_KEY`. `CURSOR_DATA_DIR` moves projects/chats data.

**Headless.** `cursor-agent -p "<prompt>" --output-format stream-json [--stream-partial-output] [--model id] [--force|--yolo] [--trust] [--approve-mcps] [--workspace dir] [--resume chatId]`. `create-chat` returns a chat ID. stream-json message `type`s: `system` (subtype `init`), `user`, `assistant`, `thinking`, `tool_call`, `result` (subtype `success`). MCP: `.cursor/mcp.json` in the workspace or `$CURSOR_CONFIG_DIR/mcp.json`; `cursor-agent mcp list/list-tools/enable/disable/login`. There is no per-run `--mcp-config` (a worker-only env `CURSOR_WORKER_MCP_CONFIG_JSON` exists).

## 7. copilot (GitHub Copilot CLI)

**Binary/version.** `~/.local/bin/copilot` (Mach-O). `GitHub Copilot CLI 1.0.114.` Home `~/.copilot`; cache `~/Library/Caches/copilot`.

**Auth status.** No status subcommand (only `login`; `/login`, `/logout`, `/user` in the TUI).
- Process log `~/.copilot/logs/process-*.log`: "gh CLI availability check completed", then "Signed in to https://github.com as <user>".
- `~/.copilot/config.json` (JSONC with `//` header) holds signed-in accounts when they are added with `copilot login`. Here it has only `firstLaunchAt`, so auth comes from the **GitHub CLI login** (Keychain `gh:github.com`). The CLI has no Keychain item of its own.
- Token precedence: `COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN` > stored login (Keychain, or plaintext in config.json). Classic `ghp_` PATs are not supported; fine-grained PATs with "Copilot Requests" or gh/Copilot OAuth tokens are.
- `~/Library/Caches/copilot/copilot-user-cache.json` (JSONC): `copilotUserCache{"v1:<hash>": {schemaVersion, generation, retrievedAt, response{login, access_type_sku:"free_limited_copilot", copilot_plan:"individual", chat_enabled, endpoints{api, proxy, telemetry, …}, quota_reset_date:"2026-12-01", quota_reset_date_utc, quota_snapshots{chat|completions|premium_interactions: {entitlement, remaining, quota_remaining, percent_remaining, unlimited, overage_count, overage_permitted, has_quota, quota_reset_at, token_based_billing, timestamp_utc}}, token_based_billing, can_upgrade_plan, …}}}`. This is a cache of the Copilot user/entitlement response (shape matches GitHub's `copilot_internal/user`; the endpoint name is inferred).
- Logged-out probe: with `COPILOT_HOME` and `GH_CONFIG_DIR` empty and token env unset, it **still authenticated** and answered (my one prompt; 1 premium request, model gpt-6-luna via Auto). So true logged-out output was not observed. Docs: model calls fail with the service's message; re-login is `copilot login` (`--device-code`/`--web-flow`/`--with-token`).

**Models.** No headless list command. `/model` is interactive. The `--model` help gives no enum (`auto` allowed). In the probe the `session.auto_mode_resolved` event reported `availableModels:["gpt-6-luna"]` (Free tier). `COPILOT_MODEL` env. `--acp` (ACP) may expose models (not verified).

**Usage.**
- `copilot help billing` / `help limits`: `/usage` (TUI) shows Plan % used, used/limit AIC, reset, plus Session and Weekly rate-limit bars.
- Headless sources: the cache file above. `--usage-output-file f.json` (`totalNanoAiu`, `totalPremiumRequestCost`, `modelMetrics`, `agentMetrics`). JSONL `session.usage_record` events (`model, modelDisplayName, inputTokens, outputTokens, cacheRead/WriteTokens, reasoningTokens, cost, maxPromptTokens, copilotUsage{totalNanoAiu, tokenDetails[]}, reasoningEffort, isAuto, …`). `session.shutdown{totalPremiumRequests, modelMetrics}`.

**Multi-account.** `COPILOT_HOME` (config/state); several accounts per home via `/user`; per process `COPILOT_GITHUB_TOKEN`. `COPILOT_GH_HOST`/`GH_HOST` for GHE.

**Headless.** `copilot -p "<prompt>" --output-format json [-s] [--stream on|off] [--model m] [--reasoning-effort l] [--allow-all-tools|--allow-tool …] [--additional-mcp-config @file.json] [--session-id uuid|--resume id] [--usage-output-file f] [--no-custom-instructions] [--disable-builtin-mcps]`. JSONL events have the shape `{type, data, id, timestamp, parentId, ephemeral?}`: `session.start`, `session.mcp_server_status_changed`, `session.model_change`, `session.auto_mode_resolved`, `user.message`, `assistant.turn_start`, `assistant.message`, `assistant.turn_end`, `session.usage_record`, `session.usage_checkpoint`, `session.shutdown`. `--acp` = ACP server.

## 8. gemini

Not installed (`which gemini` → not found). `~/.gemini` is now used by agy; the legacy `oauth_creds.json` there is stale (expired 2026-03).

---

## Windows shell tool / Git Bash configuration

Research snapshot (2026-11-25) for SYR-GM3S7: how each supported agent CLI can be configured on Windows to run its shell tool under Git Bash instead of PowerShell/cmd. Inspected via locally installed CLIs (`--help`, subcommand help, on-disk docs, JS bundles, and binary strings) on macOS; runtime execution on Windows was not exercised here.

| Agent | Version inspected | Mechanism | Verified how |
|---|---|---|---|
| **claude** | 2.1.332 | `CLAUDE_CODE_GIT_BASH_PATH=<path-to-bash.exe>` | Binary strings: reads `process.env.CLAUDE_CODE_GIT_BASH_PATH`, validates the basename is `bash.exe`/`sh.exe`/`bash`/`sh`, and prints *"Or set CLAUDE_CODE_GIT_BASH_PATH to your bash.exe location."* when Git Bash is missing on Windows. (`CLAUDE_CODE_USE_POWERSHELL_TOOL=1` opts into PowerShell; unset/default uses Bash.) |
| **grok** | 1.0.92 | `GROK_SHELL=bash` | Binary strings (`crates/codegen/xai-grok-config/src/shell.rs`) and embedded Windows docs: *"In settings shared with other platforms or with Claude Code, set `GROK_SHELL=bash` instead, so the command runs under Git Bash."* Verified live on Windows 11 (2026-11-25): `uname -s` returned `MINGW64_NT`. |
| **copilot** | 1.0.114 | **Not supported** | Not supported: `COPILOT_BASH_PATH` only sets which bash runs when bash is invoked; on Windows the shell tool stays PowerShell (verified live on Windows 11, 2026-11-25: `'uname'` not recognized). |
| **codex** | 0.171.0 | **Not supported** | Checked `codex --help`, `codex features list`, `codex app-server generate-json-schema`, and binary strings. Only `shell_environment_policy` (env filtering/injection) and internal `default_user_shell` / PowerShell helpers exist; no CLI flag, `config.toml` key, feature flag, or `CODEX_*` env var selects Git Bash. |
| **cursor** (`cursor-agent`) | 2026.11.22 | **Not supported** | Checked `cursor-agent --help` and bundled JS (`~/.local/share/cursor-agent/versions/2026.11.22-e9042b2/*.js`). No CLI flag, config key, or `CURSOR_*`/`AGENT_CLI_*` env var selects Git Bash for the shell tool (only `CURSOR_LOGIN_SHELL` and `CURSOR_FORCED_SHELL_EGRESS` exist; `COMSPEC` is read only for shell-name detection). |
| **agy** | 1.5.14 | **Not supported** | Checked `agy --help`, `agy install --help`, bundled docs, and binary strings. No CLI flag, `AGY_*` env var, or `--settings`/`settings.json` key configures the agent's shell executable (`shell_path`/`shellPath` exists only in internal protobuf/language-server messages). |

---

## Cleanup

- Scratch dir `/var/tmp/syre-research/` (empty config dirs, generated Codex schema, strings dumps, one Copilot probe session) was deleted.
- Side effects outside scratch:
  - agy appended its normal log files under `~/.gemini/antigravity-cli/log/`.
  - `grok models` refreshed `~/.grok/models_cache.json`.
  - cursor-agent may have refreshed `~/.cursor/cli-config.json` / statsig cache.
  - Two claude `initialize`-only stream-json sessions ran against `~/.claude`. No prompt was sent, so no transcript content.
  - The single Copilot prompt counts against the GitHub account's Copilot usage.
