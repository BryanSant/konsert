# Live work data: session activity, machine heat, project narration (design)

Status: active (pre-alpha). Tickets: KON-NAYVC (activity), KON-RJ8P7 (heat), KON-H5BXK
(narration), KON-ZAY87 (shells and sub-agents), under the UI rebuild KON-88NN9. This is the data layer only; the Work, Command
and Fleet modes render it.

## Session activity

What each session is doing right now, as one of eight states:

| State | Meaning | Extra fields |
|---|---|---|
| `thinking` | the model reasons, or a turn waits on the model (prompt sent, a tool result returned) | `label` for engine notes ("codex is retrying") |
| `working` | a tool call is running | `tool` (name), `label` (the call's title, e.g. "Run: go test ./...") |
| `speaking` | the model streams its reply | `tokens_per_sec` (estimated: streamed characters / 4 over the last 2 s) |
| `asking` | waiting for the user: a permission prompt, a question, Konsert's `ask_user` / `request_user_action` | `label` (the question) |
| `blocked` | can't continue on its own: suspended, paused by Pause everything | `label` |
| `queued` | accepted but not running: no machine yet, starting on one, the engine is launching | `label` |
| `done` | the turn or task ended normally (also: cancelled, the engine came up idle) | `label` ("Cancelled", "Ready") |
| `error` | the turn or task failed | `label` (the error) |

Every activity also carries `since` (when the state began), `at`, `source` and `coarse`,
and what runs beside the main loop (see [Shells and sub-agents](#shells-and-sub-agents)):

| Field | Meaning |
|---|---|
| `shells` | shell processes under the session's agent, from its process tree (background ones included; dangling ones after the session ended too) |
| `shells_busy` | how many of them run a command |
| `shell_list` | up to 12 of them: `pid`, `command` (cleaned), `summary` (hub, nowline), `started_at`, `busy`, `since` (busy last changed), `dangling` |
| `subagents` | running sub-agents: `id` (the parent's tool call id), `title`, `state` (thinking / working / speaking / asking), `label`, `since` |
| `waiting` | the parent only waits on its sub-agents (every running tool call starts one) |

`state` stays the parent loop's state, with one exception: a sub-agent waiting for the user
makes the whole session `asking`, its label naming the sub-agent ("Sub-agent “Docs for cache
flags” asks: Allow Write: docs/flags.md?").

### Readable now lines (`summary`)

`label` is the raw tool line ("Bash: cd /private/var/tmp/konsert-live/… && python -m pytest
-q"); when tool calls run at once (three parallel Reads) the worker joins their labels with
` · `. For a `working` activity the hub adds `summary`, a short line made by
`internal/nowline` (the only summariser; the web app shows `summary` and keeps `label` as the
line's hover title, and the session view keeps the full call):

* labels of every engine: claude (`Run:`/`Read:`/`Edit:`/`Search:`/`Fetch:`/`Agent:`, PTY
  `Bash:`), codex (`Run:`, `Edit: a, b`, PTY `exec …`/`Edited …`/`M …`), ACP titles of grok,
  copilot, cursor-agent and agy/gemini (`ReadFile x`, `Shell …`, a command in backticks, the
  ACP kind with a free-form title), session-log tool names (`view`, `search_replace: …`,
  `StrReplace: …`) and hook reports;
* shell commands: `cd X &&`, env assignments and wrappers (`sudo`, `timeout`, `npx`, `uv run`,
  `bash -lc '…'`, `python -m`) are dropped; the most telling step wins (tests > build >
  installs > git commit/push > lint > running a program > search > read); test, build,
  install, lint and git commands are recognised for Go, JS/TS, Python, Rust, C/C++, JVM,
  .NET, Swift, Elixir, Ruby, PHP, Dart and system package managers;
* paths are shown relative to the session's checkout (task effective dir / workdir, chat
  cwd), else with `~` for the home directory.

Examples: `running tests`, `running tests in internal/store`, `editing wc.py`, `reading 3
files`, `searching for handleReview`, `installing deps`, `building`, `git commit`,
`fetching api.github.com`.

### Where it is derived

```
engine stream ─▶ chat.Manager.emitLocked ─▶ activity.Chat ─┐
PTY telemetry ─▶ Runner callbacks ─▶ activity.Tracker ─────┼▶ activity.Publisher ─▶ session.activity ─▶ hub
                                                           │   (debounce + rate limit)               │
hub: task status (queued/suspended/final), observed        │                                         ▼
     sessions (hooks, process scan), hub-side chat errors, ┴──────────────────────────▶ fleet.update kind "activity"
     pending ask_user questions                                                        + GET /api/fleet `activity`
```

* **Chats** (`internal/worker/activity/chat.go`): `activity.Chat` folds every normalized
  `AgentChatEvent` the manager emits, so every engine is handled the same way: thought deltas
  → thinking; tool call pending/running → working (the latest of the running calls);
  completed/failed with nothing else running → thinking; message deltas → speaking; a
  complete message → thinking; elicitation → asking until resolved; status `starting` →
  queued, `suspended` → blocked, `working` after the turn starts → thinking; `turn_end` →
  done (stop reason `error` → error); `EventError` → error.
* **PTY tasks** (`internal/worker/activity/task.go`): from the driver telemetry konsertd already
  collects (session log tail every ~600 ms, PTY lines) and the task status. Always `coarse`.
* **Hub** (`internal/controller/activity.go`): queued/assigned → queued, suspended → blocked
  ("Paused (Pause everything)" when Pause everything did it), final statuses → done/error,
  running with no worker report → thinking (coarse); observed sessions from hooks and process
  scans (coarse); errors and turn ends the hub generates for chats (lost connection); a pending
  `ask_user` question → asking with the question as label. The hub only accepts a worker's
  report for sessions that worker hosts, ignores reports older than what it has, and never
  revives a finished task.

### Shells and sub-agents

**Shells come from processes, not events** (`internal/worker/shells`). konsertd knows each
hosted session's agent process: chat engines (`chat.Manager.ProcessIDs`, every real engine's
session implements `ProcessID`) and PTY agent tasks (`Runner.SessionPIDs`; scratch terminals
are not agent sessions). Every second (`DefaultShellInterval`) it takes **one process-table
snapshot shared by every session**, none at all while nothing is hosted or dangling:

| OS | Process table | Command line | CPU (for busy) |
|---|---|---|---|
| Linux | `/proc/*/stat` (pid, ppid, pgrp, state, utime+stime, starttime + `btime`) | `/proc/<pid>/cmdline` | ✓ utime+stime |
| macOS | `sysctl kern.proc.all` (`x/sys/unix`), no process spawned | `sysctl kern.procargs2` | ✗ (needs cgo `proc_pid_rusage`): busy = has a child |
| Windows | Toolhelp32 snapshot (pid, parent, image) | `NtQueryInformationProcess(ProcessCommandLineInformation)` | ✓ `GetProcessTimes`; start time too, read only for processes of a session's tree |
| other | not supported (no shells reported) | | |

Counting: walking down from the agent, the **first shell on each path** counts (`sh`, `bash`,
`zsh`, `fish`, `dash`, `ksh`, `mksh`, `ash`, `csh`, `tcsh`, `nu`, `pwsh`, `powershell`,
`cmd`); shells nested in it belong to it. Members of the agent's process group that lost
their parent count too, shell or not (`sleep 300 &` in a tool call: the shell exited, the
job runs on). A shell is **busy** when a non-shell process runs under it, or (Linux,
Windows) its tree used ≥ 5 % of the tick on the CPU. A child that started before its
"parent" is a stale parent link (Windows pid reuse) and is skipped. Each shell is
identified by pid + start time, so a reused pid is never mistaken for it.

**Dangling shells**: a shell that is no longer under the agent (the session ended, the agent
let go of it) but still runs keeps being tracked and reported for its session (`dangling:
true`) until it exits; then one last report with zero, and the session is forgotten. The
publisher doesn't prune a finished session while it has shells, and the hub keeps konsertd's
shells when it derives a state itself (a task's final status).

Commands are cleaned for people: the script of `sh -c …` / `bash -lc …` / `pwsh -Command`
/ `cmd /c`, without claude's wrapper (`source …/shell-snapshots/…`, `setopt`/`shopt`,
`eval '…' \< /dev/null`, `pwd -P >| …`); capped at 300 characters. The hub adds each
shell's `summary` with `internal/nowline`.

**The detail list is inline** (`shell_list`, at most 12; `shells` stays exact): a frame
with 12 shells is about 2 KB and changes at most a few times a second per session (below),
so a separate message wasn't worth a second store and ordering problem.

**Sub-agents come from the stream.** claude lines with `parent_tool_use_id` belong to the
sub-agent started by that Task/Agent tool call. The engine marks their events with
`AgentChatEvent.parent_tool_call_id` (stream deltas used to be dropped); the chat manager
feeds a sub-agent's text and thoughts only to the activity deriver (never the transcript),
while its tool calls stay in the transcript as before. `activity.Chat` keeps one state per
sub-agent, from its Task call (title = the Task's `description`, thinking) until the Task's
tool result: thought → thinking, text → speaking, tool call → working (label = the call),
permission prompt → asking. A sub-agent's permission prompt is named ("Sub-agent “…”
asks: …") in the transcript, the Needs-you item and the session's label. Codex and ACP
agents expose no sub-agent signal: they report none.

**Cleanup** (Fleet): `POST /api/tasks/{id}/shells/kill`, `POST /api/chats/{id}/shells/kill`
with `{"shells": [{"pid", "started_at"}]}` (no list: all of them). The hub checks each shell
is in the session's current `shell_list` and sends `session.shell.kill` to the hosting
konsertd, which kills only processes it still attributes to that session with the same
start time, each with its whole tree (SIGTERM, SIGKILL after 2 s for survivors; Windows
TerminateProcess). `POST …/shells/ask` asks the agent instead: typed into a running PTY
task (steer), sent as a follow-up message to an idle chat, queued for a chat mid-turn and
sent when its turn ends; a finished task or a suspended session can't be asked (409). Both
are audited (`session.shell_kill`, `session.shell_ask`) and recorded in the fleet timeline.

### Rate limiting

`activity.Publisher` (one per konsertd) publishes per session:

* **debounce 300 ms** for transient states (thinking, working, speaking): a tool that ends and
  the next one that starts 50 ms later never shows the "thinking" in between;
* states that matter to a human (asking, blocked, queued, done, error) go out as soon as the
  minimum interval allows;
* **at least 300 ms between publishes** of one session (≤ ~3/s), **max wait 1 s** (a stream of
  changes still shows about once a second), **output-rate-only updates at most once a second**;
* the latest activity always wins (a held-back change is replaced, never queued); a task's final
  state is flushed at once; the last state of each session is re-sent after a reconnect (the hub
  keeps activity in memory only);
* **shell and sub-agent changes** are debounced like a transient state (out 300 ms after the
  change, within 1 s under a stream of changes): the poller hands each session's shells to
  `Publisher.SetShells`, which republishes the session's latest activity with them, so a count
  change reaches the browser within ~1.3 s of the process starting or exiting.

Token deltas never reach the hub as activity: a 2,000-update burst over 2 s publishes ≤ 10 times
(`TestPublisherBoundsAFlood`); 2,000 shell-count and sub-agent changes over 2 s also publish
≤ 10 times, ≥ 300 ms apart, the latest of both landing (`TestPublisherBoundsAShellAndSubagentFlood`).

### Per-agent fidelity

Verified live: claude (haiku) chat sessions. The rest follows from each engine's protocol
mapping (unit-tested with fake engines) and should be confirmed live when those agents are used.

| Agent (path) | thinking | working + label | speaking + tok/s | asking | blocked | queued | done / error | shells | sub-agents | Notes |
|---|---|---|---|---|---|---|---|---|---|---|
| claude chat (`-p` stream-json) | ✓ `thinking_delta`, and waiting on the model | ✓ `tool_use` (title from the input: "Run: …", "Edit: …") | ✓ `text_delta`, estimated rate | ✓ `can_use_tool` permission, `ask_user` (hub), `request_user_action` | ✓ suspend | ✓ engine start | ✓ `result` | ✓ process tree (incl. `run_in_background`) | ✓ `parent_tool_use_id` lines: thinking / working / speaking / asking + label, until the Task result | A tool shows once its input is complete (assistant line); while a long tool input streams (a big Write) the previous state stays. |
| codex chat (app-server) | ✓ reasoning summary deltas when codex sends them; otherwise waiting on the model | ✓ commandExecution / fileChange / mcpToolCall items | ✓ `item/agentMessage/delta` | ✓ exec/patch/permission approvals, `requestUserInput`, MCP elicitations, `ask_user` | ✓ | ✓ | ✓ `turn/completed`, errors | ✓ process tree (incl. background terminals) | ✗ no signal on the wire | |
| grok, agy, copilot, cursor chat (ACP) | ✓ `agent_thought_chunk` when the agent streams thoughts (agent-dependent); otherwise waiting on the model | ✓ `tool_call` / `tool_call_update` with status; label = the agent's title | ✓ `agent_message_chunk` | ✓ `session/request_permission`, `ask_user` (name or title match) | ✓ | ✓ | ✓ stop reason, errors | ✓ process tree | ✗ no signal in ACP | Tool names come from `_meta`; ask_user is also matched by title. |
| mock chat | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ (in-process) | ✓ prompt `subagents` | Prompt `activity` walks thinking → working → speaking → done with 1.2 s per state; `subagents` runs two sub-agents, one asks a permission; `ask`/`permission` → asking; suspend → blocked; `crash`/`fail auth` → error. |
| PTY tasks (all drivers) | coarse: after start, after a tool, on usage records | coarse: tool_call / file_edit telemetry (claude, grok, copilot, cursor session logs; codex/agy PTY lines) | ✗ (session logs hold whole messages) | coarse: trust/permission prompts in the PTY | ✓ suspend, Pause everything | ✓ (hub) | ✓ exit status | ✓ process tree (mock: `BG_SHELL=<secs>`) | ✗ | `coarse: true`, `source: session_log`. |
| Observed claude (hooks installed) | coarse: PostToolUse, prompt submit | coarse: PreToolUse with the tool and its command/file | ✗ | coarse: Notification | ✗ | ✗ | coarse: Stop → done | ✗ | ✗ | `konsertd hook-event` reads the hook JSON on stdin (session, cwd, tool). |
| Observed sessions without hooks | ✗ | coarse: process seen busy (no tool) | ✗ | ✗ | ✗ | ✗ | coarse: process gone → done | ✗ | ✗ | Process scan every 12 s; `source: passive`. |

### Web store

`ActivityStore` (`web/src/app/core/state/activity.store.ts`): `byKey`, `byChat`, `byTask`,
`forChat(id)`, `forTask(id)`; newest report wins (a slow snapshot never reverts a live update).
`WorkStore.taskActivity(id)` / `chatActivity(id)` fall back to a coarse state from the task or
chat status, so every session has one.

## Machine heat

One smoothed 0..1 score per machine with its dominant cause, computed by the hub from the
heartbeat metrics (`internal/controller/heat.go`):

| Component | Value (0..1) |
|---|---|
| `cpu` | max(CPU% / 100, load average / cores) |
| `mem` | memory pressure: used/total mapped from 30 % → 0 to 95 % → 1; swap in use above 80 % used adds half the swap share |
| `gpu` | GPU utilization / 100, where known (macOS `ioreg` IOAccelerator statistics, `nvidia-smi` on Linux) |
| `slots` | active tasks / max_concurrent (the scheduler's limit; ≤ 0 counts as 1, as in the scheduler) |

Raw score = the maximum component; `cause` = that component. The score is an EWMA with a 30 s
time constant (α = 1 − e^(−Δt/30 s) per heartbeat), so a 10 s spike stays cool and a minute at
full load is hot. **A machine the scheduler considers full (active ≥ max_concurrent) is shown hot
immediately** (`full: true`, cause `slots`). Levels: **cool < 0.4, warm 0.4–0.75, hot > 0.75**.

Exposed as `Worker.heat` (`score`, `raw`, `level`, `cause`, `components`, `full`, `updated_at`)
in worker updates and `GET /api/fleet`, and as `FleetStore.heatByWorker` (online workers only).

### Heat history

For the Fleet heat strips (12h / 24h / 3d / 1w) the hub keeps a downsampled history per machine
(`internal/controller/heat_history.go`):

* **Storage**: one row per machine per **1-minute bucket** in SQLite (`worker_heat_samples`:
  `bucket_start` unix seconds, `score_max`, `score_avg`, `samples`, `cause` at the max). The
  current minute accumulates in memory on each heartbeat; when a heartbeat lands in the next
  minute the finished one is written with one upsert on a background goroutine, and hub
  shutdown flushes every in-progress minute. The upsert merges into an existing row (max of
  maxes, sample-weighted mean), so a minute split by a restart or reconnect stays correct.
  Minutes are the finest step any strip needs (1w at 336 points is 30 min, 12h at 336 is
  ~2 min), cost ~10k rows per machine per week, and are cheap to aggregate on read; raw
  heartbeats (~6/min) would be 6× the rows with no visible gain. **Retention: 8 days** (the
  longest range plus a day), pruned at most hourly on a flush. Removing a machine deletes its
  rows (`DeleteWorker`, plus `ON DELETE CASCADE`). History survives hub restarts.
* **Endpoint**: `GET /api/fleet/heat?range=12h|24h|3d|1w&points=N` (browser auth; default
  `12h`, 48 points; `points` clamped to 12..336; an unknown range or non-integer points is 400).

```json
{"range":"12h","start":"2026-01-01T22:15:00Z","end":"2026-01-02T10:15:00Z","step_seconds":900,
 "workers":[{"worker_id":"…","samples":[{"score":0.41,"max":0.62,"cause":"cpu"},null,…]}]}
```

`step_seconds` is range / points rounded up to whole seconds; `end` is the end of the
step-aligned (unix epoch) bucket holding the current minute, so points stay put between polls,
and `start` = `end` − points × step. `samples` has exactly `points` entries, oldest first; each
aggregates the minutes starting in it (`score` = mean of the minute means, `max` = max of the
minute maxes, `cause` = cause at that max, 3 decimals) or is `null` with no data (offline, or
before history began). The in-progress minute is included, so a live machine's last point is
never empty. Every known machine is listed (in `ListWorkers` order), even with all nulls.

## Project narration

* **Orchestrator**: the `set_narration` MCP tool (orchestrator chats, `konsert mcp`, and the
  API-key backend) sets a project's `headline` (its current goal, ≤ 140 chars) and/or a task's
  one-line `now` (≤ 120 chars). `dispatch_task` takes a short `title`. The standing prompt
  (`orchestrator.NarrationPrompt`) says to update headlines when a plan starts or changes, not on
  every step, and to add a task's `now` only when its purpose isn't clear from its title.
* **Facts**: tasks record a `ticket_id` detected in their title or prompt; the hub resolves its
  title with `ka show` in the background (`ticket_title`).
* **Fallbacks** (`web/src/app/core/state/activity-logic.ts`), so nothing is blank:
  * task title: `title` → ticket id + title → the prompt's first line → agent + sid;
  * task now: orchestrator `narration` → the live line (tool label while working, the question
    while asking, the error) → the task title;
  * chat now: the live line → chat title → "<agent> chat";
  * project headline: orchestrator `headline` → "Needs you: …" / "Working on …" (the most
    pressing item, ticket title preferred) → counts ("2 queued · 3 done") → project name.
* **Grouping** (`WorkStore.projectGroups`): tasks by `project_id`; tasks in the default project,
  observed sessions and chats are placed by their directory when it falls under a project's repo
  on the same machine (longest match); everything else is under **Other work**. Scratch
  terminals, shells and the orchestrator chat itself are not work items. Each group has counts
  `running` / `queued` / `needs_you` / `blocked` / `done` (+ `failed`): the bucket comes from the
  activity (asking → needs you, blocked, queued, done/error → done, else running), and a final
  task status always counts as done. Groups needing the user come first; Other work is last.

## How the views show it

The signed-off design (KON-K2GAD) renders all three with one **spinner**
(`web/src/app/shared/activity-spinner.component.ts`, logic in `spinner-logic.ts`): the avatar
fill is the machine (its initials on a slowly drifting gradient by heat, cool blue/teal →
amber → red, hot drifts faster), the ring around it is the activity.

| Ring state | Comes from | Ring |
|---|---|---|
| thinking | `thinking` | violet comet, breathing |
| working | `working` | white segments spinning |
| speaking | `speaking` | mint ripple, faster with `tokens_per_sec` (1.5 s → 0.5 s at 80 tok/s) |
| asking | `asking` | solid amber, "?" badge |
| held | `blocked`, or the control label HELD / SUSPENDED | dashed blue, pause badge |
| you | control label YOU (Take control) | solid blue, "›" badge |
| queued | `queued` | hollow avatar, dotted ring |
| done | `done` | grey ring, green check, neutral avatar |
| error | `error` | red, "!" badge |
| waiting | `working` with `waiting` (the parent only waits on sub-agents) | plain grey ring; the sub-agent dots carry the motion |

Shells and sub-agents ride on the same spinner (KON-ZAY87): a **shell badge** `❯N` at the
avatar's bottom-left (its outline pulses while any shell is busy, still when idle), and up to
**four sub-agent dots orbiting the ring** (one turn per 9 s) in their own state colours
(violet breathing = thinking, white blinking = working, green = speaking, amber = asking),
"+N" top-right beyond four. Any sub-agent asking turns the whole ring amber with the "?"
badge. Counts appear only when non-zero; the hover text adds them ("desk · warm 55% · cpu —
thinking · 3 shells (2 busy) · 2 sub-agents"). Fleet session cards spell it out: chips
"❯ 3 shells · 2 busy" / "◉ 2 sub-agents" ("❯ 2 shells left running" after the end), a row per
shell (command, how long it has run a command or "idle", **Ask to stop** / **Kill** with a
confirmation) and per sub-agent (state dot, title · label, age), and **Clean up all** /
**Ask the agent to clean up** with two or more shells. A finished session with shells still
running keeps its card (and its Command line) until they exit. Command's live-work lines
use the compact badges and mention the counts ("3 shells · 2 sub-agents", "done 3m ago · 1
shell left running").

Observed sessions (`coarse` and started outside Konsert) draw the ring at half strength. A
second machine peeks out behind the first (the component supports it; no task reports a
second machine yet). The hover tooltip and the accessible name say the activity word, the
machine(s), heat % and cause; every caller also shows a text label, never colour alone.
`prefers-reduced-motion` stops all animation (static rings and fills; no orbit, no pulse, no blink). Fleet rows put the
agent's mark in the centre instead of initials, since the row is the machine.

Used in Command's Live work (the project story, `modes/command/live-work-logic.ts`: project
name, orchestrator headline marked ✦, progress bar done today / running / needs you /
queued, "N in flight on M machines · K queued", in-flight lines, the queue collapsed, done
lines fading out over 5 minutes; Other work holds observed sessions and your terminals),
Fleet's machine rows and session cards, Wall tile headers, Work cards, the session strip,
Command's footer machine list (heat avatar only) and the phone inbox and fleet.
