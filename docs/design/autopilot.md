# Autopilot: token-efficient autonomous execution

Status: design, not built. Related: [work.md](work.md) (tickets, leases, tracker sync),
[delivery.md](delivery.md) (review and delivery), [prd.md](../prd.md) §5.8c.

## Principles

Tokens are the expensive resource. Go (`syre`, `syred`) is a deterministic router and state
machine between models.

1. **State lives in Syre**, never in the Commander's context: backlog, leases, attempts,
   policies, history.
2. **Pass by reference, return by digest.** Workers get a short packet and read files
   themselves. The Commander gets a short digest; diffs and logs stay at the edge and are
   pulled only on request.
3. **Deterministic first, reasoning on exception.** Rules, timers, git, process lifecycle,
   verification and retries run in Go at zero token cost. The Commander is woken for
   decomposition, judgment, ambiguity and translating natural language into policy.
4. **Push, never poll.** The Commander learns about state changes from a pulse; polling loops
   are what the pulse replaces.
5. **One control plane.** The PWA, REST and MCP call the same service layer.
6. **Hub and spoke only.** No agent-to-agent channels. Hub↔daemon stays JSON over WebSocket:
   it costs no LLM tokens, so a binary protocol buys nothing worth the churn.

## Who does what

| Responsibility | Layer |
|---|---|
| Ready queue, DAG, leases, placement, timed policies, cooldowns | `syre` (Go) |
| Graceful stop with grace period, then kill and process-group cleanup | `syre` + `syred` |
| Worktree provisioning and cleanup, verification runs, self-repair loop | `syred` |
| State detection (exit, idle, prompts), log/diff/test compression, digests | `syred` |
| Decomposition into tickets with acceptance and `blocked_by` | Commander |
| Natural-language policy ("go easy on the Mac until 6") into directives | Commander |
| Answering worker questions from project context; escalating the rest | Commander |
| Implementation, local exploration, running tests | Worker agents |

## Dispatch packet (hub → daemon → worker)

The hub compiles a packet from the ticket (target 150–300 tokens). The worker uses its own
file tools; nobody pastes files into prompts.

```yaml
ticket: SYR-7QK3M
project: billing
repos:
  - {name: api, path: /work/SYR-7QK3M/api, branch: feat/jwt-rotation}
  - {name: web, path: /work/SYR-7QK3M/web, branch: feat/jwt-rotation}
objective: Rotate refresh tokens every 15m and retry once on 401 in the web client.
acceptance:
  - Rotate on /auth/refresh; invalidate the token family on reuse.
  - Web fetch wrapper retries once on 401.
contracts:
  - "SYR-7QK1A (done): POST /v2/auth/refresh returns {access_token, refresh_token, expires_in}"
verify: go test ./pkg/auth/... && npm --prefix web test
tools: ticket_get, ticket_check, ticket_comment, ticket_followup, ask_user
```

`contracts` come from the digests (`interface_digest`) of done tickets this one is blocked by.

## Post-run digest (daemon → hub → Commander)

When a run leaves `running`, `syred`'s extractor writes a digest (target 100–250 tokens).
Full diffs, logs and transcripts stay on the machine as artifacts.

```yaml
ticket: SYR-7QK3M
attempt: 2
status: review            # review | needs_you | failed
machine: bocabox (claude, opus)
duration_s: 412
git: {api: "2 files, +74 -9", web: "1 file, +28 -4"}
verify: {cmd: "go test ./pkg/auth/... && npm --prefix web test", exit: 0, passed: 41, failed: 0}
interface: "RefreshHandler rotates tokens; api/pkg/auth/handler.go"
followups: ["/v1/token still issues static refresh tokens"]
summary: Rotation with reuse detection and a single 401 retry; tests pass.
```

- **failed**: failure category (shared with chat engines: auth, usage_limit, rate_limit,
  timeout, oom, disk_full, missing_tool, verify, agent_error, canceled), failing test names,
  and the last 30 ANSI-stripped stderr lines.
- **needs_you**: the worker's question and the 20 lines before it.
- Test output is parsed for common runners (go test, jest/vitest, pytest, cargo, gradle,
  maven, dotnet); otherwise exit code plus tail.

**Artifacts** (`diff_stat`, `diff`, `stderr`, `log`, `transcript`, `file`) are fetched through
`inspect` with a line or byte cap, from the daemon (edge principle). Every Commander-facing
payload is capped; `inspect_git_status`'s uncapped per-file diffs are replaced by `inspect`.

## Verification gate and self-repair

1. The worker finishes (one-shot exit 0, or a chat turn ends with the ticket's acceptance
   items checked or the agent saying it's done).
2. `syred` runs `verify_cmd` (ticket, else repo default) in the ticket workdir, under the
   machine's resource envelope and a timeout.
3. **Fail** and `attempt < retry_limit` (default 2): `syred` sends the compact failure tail
   back into the same agent session as the next prompt. No Commander tokens.
4. **Fail**, retries exhausted: ticket → `failed` (or `needs_you` when `escalate_on_blocker`),
   digest pushed.
5. **Pass**: ticket → `review`. With `cross_review`, the hub dispatches a review run on a
   different agent or model that checks the diff against the acceptance items and returns
   approve or changes-requested; changes-requested goes back to the author session like a
   verify failure. Then delivery proceeds per [delivery.md](delivery.md).

Each loop iteration is an *attempt* row: machine, account, agent, model, timings, exit,
failure category, verify result, tokens and cost where known. Attempts feed work profiles and
placement (PRD §5.4–5.6).

## Board pulse (hub → Commander)

A pulse is pushed only on actionable transitions: a batch of completions, `needs_you`,
retries exhausted, followups awaiting triage, a backlog item needing decomposition, a
directive expiring, the board going idle. Routine progress is not a trigger.

```
[SYRE PULSE | autopilot on | 14:32]
Fleet: 4/5 up (mini-mac throttled max=1 until 18:00; win-desk paused until 18:00)
Board: 12 ready (5 dispatchable) · 4 running · 1 needs you (SYR-7QK9P) · 3 review
Trigger: SYR-7QK3M → review (unblocked SYR-7QK4B, SYR-7QK4C); 1 followup
```

- Transitions within a debounce window (default 20s) are batched into one pulse.
- Delivery reuses the fleet-event path: an idle Commander chat gets a new turn; a busy one gets
  the pulse prepended to its next prompt. External MCP Commanders get an MCP notification and
  `pulse` as a resource.
- The pulse names tickets; details come from `backlog` or `inspect`.

## Autopilot loop

**Play** (PWA toggle or "keep going until it's done") is per project or global. It's distinct
from **Pause everything**, which is the emergency control that also suspends running work.

1. **Ready queue** (Go, every scheduler pass): dispatchable tickets in priority order, then
   age. Tickets marked `needs_decomposition` or with no repo binding are batched into one
   pulse asking the Commander to decompose them via `manage_work`.
2. **Dispatch** (Go): placement (eligibility, policy, directives, headroom, locality,
   subscriptions; PRD §5.5), lease, packet, worktrees, agent launch.
3. **Verify and self-repair** (`syred`), as above.
4. **Blockers**: worker questions go to the Commander first when the project allows it
   (existing "let the Commander decide" with receipts and the override window); destructive
   or genuinely ambiguous ones stay in Needs you for the human.
5. **Scope**: followups are triaged by the Commander. With `expand_scope` on, in-scope ones
   become tickets under the same epic; otherwise they're filed to backlog for the human.
6. **Done** when nothing is dispatchable, running or waiting in the project: final pulse,
   Play turns off.

## Policy directives

Natural-language requests become *directives*: typed, timed, attributed policy entries
enforced by the scheduler and the daemon.

| Directive | Effect |
|---|---|
| `machine.pause(until)` | No new work; running work finishes or is suspended (choice) |
| `machine.throttle(max_concurrency, cpu_ceiling_pct, heavy_ok, until)` | Caps; envelopes enforce CPU where the OS can |
| `machine.drain` | Finish running work, accept none |
| `provider.disable(agent or account, until, fallback)` | Skip it in placement; reroute preferring `fallback` |
| `autopilot.pause(project, until)` | Stop dispatching for the project |

- **Tighten vs loosen.** Directives that only restrict apply immediately when the Commander
  sets them, attributed "user via Commander", with a receipt and one-click undo. Anything that
  loosens operator policy (more slots, lifting caps or quiet hours) goes through
  `request_override` and user approval.
- **Setting** `commander_policy_writes`: `tighten` (default), `confirm` (every change is an
  approval card), `none` (read-only).
- Expiry is a hub timer; nothing keeps a model awake to watch a clock.
- Usage-limit windows reported by daemons create automatic `provider.disable` directives until
  the reset time (provenance `system`).

## Commander MCP surface

Target: about 10 tools with flat schemas (from 28 today). Old tools fold in as actions.

| Tool | Replaces / covers |
|---|---|
| `fleet_status(detail, machine?)` | `fleet_status`, `inspect_machine_facts`; summary groups by capability by default |
| `backlog(project?, parent?, status?, limit?)` | `list_tasks`, `get_taxonomy`; compact table |
| `manage_work(action, items[])` | create/update/cancel/requeue tickets and DAG edges, `dispatch_task` (ad-hoc), `manage_taxonomy` |
| `control(scope, target?, action, grace_s?)` | pause/resume/stop/drain/suspend at ticket, session, machine, project, fleet; `cancel_task`, `veto_task`, `suspend_session`, `resume_session`, `drain_machine`, `pause_everything`, `resume_everything` |
| `set_policy(target, directive, until?, constraints?)` | new; tighten vs loosen as above |
| `reply(target, message, resume?)` | `steer_task`, answering a worker's question |
| `inspect(target, kind, repo?, path?, max_lines?)` | `get_task_result`, artifacts, `recent_events`, `list_audit` |
| `ask_user`, `answer_question`, `request_override` | unchanged |
| `memory(action, ...)` | `save_memory`, `recall_memory` |

Settings, fan-out, narration and git inspection move to the UI or into these actions. External
`/mcp` gets the same surface. Schema size is budgeted (below).

## Token budgets as tests

Like ka's scenario budgets, CI asserts token counts (a fixed tokenizer approximation) for:
packet ≤ 300, digest ≤ 250, pulse ≤ 120, the full Commander tool schema ≤ 2,500, and
`fleet_status` summary for a 20-machine fleet ≤ 400. Regressions fail the build.

## Deferred

- Batch leases (a daemon checks out an epic or chain and runs it offline).
- Optimistic chaining on by default.
- Preemption.
