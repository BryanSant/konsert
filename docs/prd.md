# Product Requirements Document: Konsert

| Field | Value |
| :--- | :--- |
| **License** | Apache License 2.0 |
| **Status** | Pre-alpha. Backward compatibility is an anti-goal until 1.0. |
| **Stack** | Go 1.27 (`konsert` hub, `konsertd` daemon), SQLite (swappable `Store`), Angular 22 zoneless PWA, MCP |
| **Audience** | Homelab builders and power users running coding agents on their own subscriptions across several machines, VMs and containers |

---

## 1. Problem

People who use coding agents seriously end up with agent work scattered across machines: a desktop, a mini PC, a Mac, WSL, containers. Some of it is started by an orchestrator, some by hand in a terminal or a desktop app. Nobody can answer simple questions:

- What agent work is running right now, on which machine, doing what, and how much of the machine is it using?
- What else is loading that machine that isn't agent work?
- Where should the next piece of work go, given what each machine can do, what's already there, and what's already running?

Konsert is a self-hosted control plane that answers those questions and acts on the answers.

## 2. Purpose: two pillars

1. **Total visibility.** Every agent session on every connected machine is known: who started it (Konsert or the user directly), which agent and account, what it's doing, which processes belong to it, what resources it uses. Load that isn't agent work is known too, so it can't be blamed on an agent or ignored.
2. **Intelligent scheduling.** Work goes to the machine best able to finish it. Hard requirements (OS, tools, SDKs, logins, hardware) are always honoured. Placement improves as Konsert learns the size and shape of each kind of work, and work migrates to a better machine when it outgrows its current one.

Visibility comes first: scheduling without it over- or under-commits machines.

## 3. Principles

- **Hub-and-spoke control, direct data when it's cheaper.** The hub (`konsert`) commands; daemons (`konsertd`) connect outbound, wait passively for work and report what they see. Daemons never command each other. Bulk data that must move between machines (a repo bundle for migration) goes, in order of preference: through a git remote both can reach; directly daemon-to-daemon over SSH with fleet-managed keys, arranged by the hub, when the machines can reach each other (e.g. over Tailscale); relayed through the hub as a last resort. A laptop on hotel Wi-Fi shouldn't sit in the middle of a transfer between two desktops.
- **Bring your own subscriptions.** Agents run on the user's logged-in CLI subscriptions; API keys are optional. Subscription limits are a fleet resource the scheduler must respect.
- **Facts and memory in Konsert, judgment in the orchestrator, guardrails in Konsert.** Konsert collects facts, keeps history and enforces operator policy. The orchestrator agent makes placement and migration judgments through MCP, with that history at hand so restarts and context limits don't erase what was learned. Konsert also works without an orchestrator: a built-in deterministic scheduler places work by rules.
- **Two tiers of control, hard by default.** *Operator policy* (caps, reservations, allowed work) is hard: neither the scheduler nor the orchestrator can exceed it in normal operation. Agents forgetting established rules is the common failure, so rules are enforced by Konsert, not remembered by the agent. *Placement* (which eligible machine) is soft: the scheduler proposes, the orchestrator may override within policy.
- **Break glass, measured.** Rarely, breaking a rule is right (an all-hands CVE fix). The orchestrator may request a time-boxed override of named rules with a justification; by default the user approves it, and the operator can allow orchestrator self-approval. Every block and every override is recorded with the orchestrator's stated intent, so we can measure how often the rules got in the way and refine them.
- **Heavy data stays at the edge; transcripts are ephemeral.** Daemons keep raw terminal output, transcripts and per-process samples locally and send them only when someone looks (as with today's on-demand PTY streaming). Transcripts are fetched lazily when a user explicitly opens one, cached briefly for scrolling (default 30 minutes), then flushed; the source can always be streamed again. The hub keeps session metadata, status, summaries, aggregates and outcome history, not transcripts: they're useful while the work is live, rarely afterwards, and keeping them is a privacy risk with little value.
- **Same user, own data.** The daemon runs as the same user as the agents and reads that user's agent data to provide visibility. That's not a privacy boundary, but data still leaves the machine only as needed (see edge principle) and access to the hub is authenticated.
- **The user is always in charge.** Konsert is mostly a monitoring tool over automated work. The user can watch everything and at any moment steer, suspend, veto, take control of or hard-stop anything, from the UI, a terminal or the orchestrator. Every intervention is allowed, attributed to the user, and explained to the agents it affects; agents don't investigate the user's deliberate actions as failures, and they ask before reacting.
- **Opinionated over configurable** where it simplifies: one recommended isolation and enforcement setup per OS, with escape hatches.

## 4. Topology

```
 Browser PWA ─┐                 ┌─ konsertd (Linux host)      ─ agents, processes
 Orchestrator ├─ konsert (hub) ─┼─ konsertd (Windows host)    ─ agents, processes
 agent (MCP) ─┘  scheduler,     ├─ konsertd (macOS host)      ─ agents, processes
                 store, MCP     └─ konsertd (container / WSL) ─ agents, processes
```

- **Hub:** REST + WebSocket API, embedded PWA, MCP server, scheduler, policy, SQLite store, setup and daemon lifecycle (starts local and SSH-managed daemons; see §5.1).
- **Daemon:** one per machine identity (host, container, VM). Runs agent sessions for the hub, observes sessions it didn't start, samples processes and machine load, enforces resource envelopes, reports facts.

## 5. Functional requirements

Status markers: **[have]** exists today, **[partial]** exists but incomplete, **[gap]** missing.

### 5.1 Fleet and machines

- **[have]** Machines register with a persistent UUIDv7 identity (regenerated if the state is cloned onto another machine; fingerprint from `/etc/machine-id`, `IOPlatformUUID` or `MachineGuid`) and a short `m-` sid.
- **[have]** First-run setup: the PWA opens Add Machine with "This machine" first; the hub installs or attaches a local daemon, or bootstraps one over SSH (Linux, macOS, Windows without a console window). On restart the hub restarts hub-managed daemons that haven't reconnected.
- **[partial]** Machine facts: OS, CPU, RAM, disk, virtualization, a fixed list of tool versions, agent logins. **[gap]** GPU (model, VRAM, CUDA/Metal), architecture as a scheduling constraint, extensible capability probes (Android SDK, Xcode, CUDA, emulators, custom scripts), operator-declared capabilities and labels per machine.
- **[gap]** Machine role: `dedicated` (agent work welcome) vs `interactive` (someone uses this machine; keep load light, avoid when the user is active) vs `control-only`.

### 5.2 Sessions: managed and observed

A *session* is one agent conversation or run. Every session has an origin: **managed** (started by Konsert; Hub↔Daemon↔Agent) or **observed** (started by the user directly in a CLI, IDE or desktop app, and detected by the daemon).

- **[have]** Managed sessions: chat engines for claude, codex, grok, agy, copilot, cursor with streaming, elicitation (permission, question, auth), status, usage limits and failure classification; terminal sessions.
- **[partial]** Observed sessions: hook events (claude and a notify shim) and a process scan of four CLIs (claude, agy, codex, grok), with status guessed from CPU usage.
- **[gap]** Observed-session sources for every supported agent, in order of preference:
  1. Structured session stores the agent writes anyway (claude `~/.claude/projects/*.jsonl`, codex `~/.codex/sessions`, grok, cursor-agent and copilot session state): give prompt/turn boundaries, tool calls, files touched, model, tokens and real status (working, waiting for input, done).
  2. Agent hooks where offered.
  3. Process scan as a fallback (existence, cwd, CPU).
  Desktop and IDE agents (Claude Desktop, Codex app, Cursor, Copilot in VS Code) count when they write a detectable session store.
- **[gap]** Observed sessions appear in the same views as managed ones (read-only transcript built from normalized events), with the same status model, so a user sees *all* agent work in one place.
- **[gap]** Transcripts stay on the machine and are fetched on demand, cached on the hub for a short TTL (default 30 minutes, configurable) and then purged. The hub persists only session metadata, status, summary, files touched and counts.

### 5.3 Processes and machine load

- **[have]** Host-wide CPU, memory, swap, disk and load average on the heartbeat (latest sample only, in memory).
- **[gap]** Process attribution: each session owns a process tree (managed: its process group or job object; observed: the agent process and descendants, including build tools and test runners it spawned). Per-session CPU, memory (current and peak), process count. Long-lived helpers an agent leaves behind (Gradle daemons, dev servers) stay attributed until they exit.
- **[gap]** Load accounting per machine: `agent load` (sum of sessions) vs `other load` (the user and the system), with the top non-agent processes. "User active" signal (input idle time) on interactive machines.
- **[gap]** Metric history: downsampled per-machine and per-session series kept for a configurable retention, so work shape can be learned and questions like "what was this host doing at 3am" answered.

### 5.4 Work model and classification

- **[have]** Tasks with agent, model, account, approval mode, effort, repo/worktree, prompt, and a selector (worker, name, host group, OS, virtualization, required tool versions, required auth, min RAM, min free disk, tags).
- **[gap]** *Work key*: a stable identity for "the same kind of work" across runs (project + repo + branch/worktree + optional kind such as `build`, `test`, `ml-train`, `review`). History, profiles and placement rules attach to work keys.
- **[gap]** *Requirements* (hard): OS, architecture, tools and SDKs with versions, GPU, logins/accounts, minimum resources. Declared by the dispatcher or inherited from the work key's profile.
- **[gap]** *Work profile* (learned): duration, peak memory, CPU-seconds, disk, success rate, common failure categories, per machine class. Starts unknown; updated from outcomes.
- **[gap]** Outcome record per attempt: machine, account, queue wait, duration, exit, failure category (OOM, timeout, auth, usage limit, disk full, missing tool, agent error, cancelled), resource peaks, tokens and cost where known, git result.

### 5.5 Scheduling and placement

- **[have]** Eligibility filters (selector, agent installed, account logged in, sandbox posture) and ranking by slot utilization, free disk, recency. Queue with atomic claim; requeue on worker loss for tasks with `orphan_policy: requeue`.
- **[gap]** Capacity model: each running session reserves resources (from its profile, or a default class: small/medium/large); a machine's headroom = capacity − policy reservations − agent reservations − observed other load. Slots remain as a simple cap.
- **[gap]** Subscription-aware placement: prefer machine/account pairs with remaining quota; avoid exhausted accounts until reset; spread across accounts of the same vendor.
- **[gap]** Locality: know which machines have a checkout of each repo (path, branch, freshness) and prefer them; avoid cloning onto a new machine unless the preferred ones are saturated.
- **[gap]** Placement rules: prefer/require/avoid a machine or machine class for a scope (project, repo, branch, work key), with provenance (user, orchestrator, learned), evidence and optional expiry.
- **[gap]** Placement log: for each decision, the candidates, why others were rejected, scores, the choice and who decided (scheduler, orchestrator pin, user). Exposed as "explain placement" (also as a dry run before dispatch).
- **[gap]** Initial behaviour for unknown work: rotate across eligible machines, weighted by headroom; refine as profiles fill in.
- **[gap]** Queue: priority and per-project concurrency limits; FIFO otherwise. No preemption before 1.0.

### 5.6 Retry and migration

- **[gap]** Attempts: a task can have several attempts; requeue creates a new attempt instead of overwriting history.
- **[gap]** Failure-driven retry policy: on classified failures (OOM, disk full, timeout) the scheduler or orchestrator may retry on a larger machine; on usage limits, on another account; on auth, never (ask the user).
- **[gap]** Migration = stop on machine A, move the work, resume on machine B. Live process migration is out of scope. Work moves as: committed/pushed branch when both machines share a git remote; otherwise a git bundle sent directly A → B over SSH (fleet-managed keys, see §5.9) when B can reach A, else relayed through the hub; plus a handoff note (previous summary, failure, relevant context) given to the new session.
- **[gap]** Learned pins: after a migration succeeds where the original failed, record a placement rule for that work key (provenance `learned`, with the evidence) so future work goes straight to the right machine class.

### 5.7 Policy and enforcement

- **[partial]** Sandbox posture (refuses full-access modes), allowed workdir roots, per-task timeout, process-group / job-object cleanup.
- **[gap]** Operator policy per machine (and per machine class): max concurrent managed sessions, max load average, minimum free RAM, CPU/RAM reserved for the user, quiet hours, allowed work classes, role (§5.1). Enforced twice: by the hub at admission and by the daemon at start (the daemon refuses work that violates its policy even if the hub is wrong or bypassed).
- **[gap]** Resource envelopes for managed sessions, native per OS by default:
  - **Linux:** systemd user scope (`systemd-run --user --scope`, cgroup v2) with `MemoryMax`, `CPUQuota`, `TasksMax`; plain rlimits where systemd isn't available.
  - **Windows:** Job Object limits (job memory, CPU rate control, active process count) on the job konsertd already creates.
  - **macOS:** no cgroups; scheduling priority (`taskpolicy`/nice) plus monitoring with enforced kill on sustained over-limit memory.
  - **Optional container tier:** rootless Podman per session or per project, for Linux workloads that need hard isolation or reproducible toolchains. Requires mounting the agent's login state; not available for agents whose logins live in a macOS keychain.
- **[gap]** Observed sessions can't be capped; they're counted against headroom instead.
- **[gap]** Policy is read-only over MCP. Changes happen in the UI or CLI and are audited.
- **[gap]** Break glass: `request_override` (rules, scope, duration, justification) → user approval by default (approval card + notification), or orchestrator self-approval if the operator enabled it. Overrides expire automatically. Blocks and overrides are logged with intent and surfaced in a report ("rules that blocked work this week, and whether an override followed").
- **[gap]** Container awareness: Konsert detects Podman/Docker and can run a project's work in a container when configured, but basic resource protection never requires one.

### 5.8 Orchestrator integration

- **[have]** The orchestrator runs as an agent chat on a subscription, with per-chat MCP fleet tools: fleet status, machine facts, dispatch, list/get/steer/cancel tasks, fan-out, git and directory inspection, memory, taxonomy.
- **[gap]** Scheduling tools: fleet snapshot with headroom, running sessions (managed and observed), quotas and policies; explain placement (dry run); work profiles and history per work key; task attempts and failure categories; set/clear placement rules; requeue or migrate a task to a machine or class.
- **[gap]** A shipped *Konsert scheduler skill* for orchestrator agents: how to read the snapshot, when to trust the built-in scheduler, how to respond to failure categories, when to migrate, and how to record learned rules. Konsert owns the facts and memory; the skill owns the playbook.
- **[gap]** Notifications to the orchestrator when work it dispatched fails, stalls, needs input or completes, so it can react without polling.

### 5.8a Human control and attribution

- **[partial]** Today: stop turn, cancel, take control of terminals, steer; UI and MCP actions recorded in the audit log.
- **[gap]** Cause on every state change: actor (user via UI/CLI/Konsert terminal/SSH outside Konsert, orchestrator, agent session, scheduler, system, unknown) and confidence (certain / high / likely / unknown).
- **[gap]** Controls at every scope. Session: stop turn, suspend/resume (freeze the process tree: SIGSTOP on Unix, thread suspension on Windows), take control, cancel, veto with a reason. Process: kill one process (after process attribution). Machine: drain, suspend all, stop all. Fleet: pause everything (stop dispatching, suspend running work).
- **[gap]** Veto = cancel + reason (wrong approach, wrong machine, not now, dangerous, free text), stored for the orchestrator at the work's scope so the same approach isn't simply retried.
- **[gap]** Holds: an intervention no agent asked for places a hold on its scope (session, machine, or resource such as a service or port). While held, nothing automatic happens there (no retries, re-dispatch or automatic restarts), enforced like policy. Affected agents are told and must ask before acting; the hold ends when the user releases it or answers. Holds don't suspend sessions automatically (the user can, in one click).
- **[gap]** Open requests: an agent asking the user to *do* something ("restart nginx; I don't have sudo") creates a tracked request on a machine/resource (extends `ask_user`). User actions matching an open request are linked to it rather than treated as surprises; resolving it lets the agent continue.
- **[gap]** Terminal awareness: Konsert terminals report each command and exit status (shell integration, OSC 133 semantics; zsh/bash hooks, PowerShell PSReadLine). Full command lines stay on the machine with a short TTL; the hub keeps a redacted summary ("user stopped service nginx"). Actions from SSH sessions outside Konsert are attributed with lower confidence via process lineage (Phase 6).
- **[gap]** Event delivery: relevant user actions and holds reach the orchestrator as fleet events (starting a turn when its work is affected) and reach affected worker sessions as a note before their next prompt, plus a `recent_events` tool and a standing instruction in every managed session to check it before investigating an unexpected change.
- **[gap]** UI: consistent intervention controls on every session, process and machine; Pause everything always one click away; a "your actions" timeline showing how agents responded; taking control of a session holds it, handing back releases it with a summary for the agent.

### 5.8b Questions and steering

Users mostly answer questions agents are unsure about, not do complex steering. In a busy fleet those questions must be easy to find, understandable, and answerable from a desktop or a phone.

- **[have]** Per-chat elicitations (permission, question, auth) and `ask_user`; questions are also shown inline in the asking chat through the queue's card.
- **[have]** Good questions at the source: `ask_user` requires context, why it's needed now, each option's consequence, and a rationale for any recommendation; under-specified questions are rejected back to the agent with what to add. Managed sessions get guidance on asking (one decision, say what you'd do and why, prefer yes/no with a recommendation).
- **[have]** References in questions (tickets, PRs, files, Konsert sids) are resolved into cards.
- **[have]** Questions are persistent hub objects in one fleet-wide queue, sorted by blocking impact and grouped by ticket/task; a question stays open until answered (reloads, side conversations and timeouts don't dismiss it).
- **[have]** *Ask about this*: a side conversation attached to the question, with the original options pinned. It runs on a read-only fork of the asking agent (same conversation context; `claude --fork-session`, `codex fork`) so the original stays blocked and undisturbed, or on the orchestrator with a context pack when the agent can't fork. It can draft an answer. The answer reaches the agent with a short summary of the clarification.
- **[have]** *Let the orchestrator decide* for low-stakes questions, with a reversible receipt of what it chose and why; optional per-category rules.
- **[have]** The whole flow (queue, question, side conversation, answer) works on a phone.

### 5.9 Fleet transfer channel

- **[gap]** The hub manages a fleet SSH identity per daemon (keypair generated on the machine; public key registered with the hub; private key never leaves the machine). For a transfer, the hub tells the receiver to accept one specific key for one transfer and tells the sender where to connect; authorized keys are restricted to a single konsertd receive command, scoped and short-lived.
- **[gap]** Reachability is probed, not assumed (Tailscale addresses, LAN); the hub picks git remote → direct SSH → hub relay.
- **[gap]** Transfers are audited (who, what, bytes, path taken).

### 5.10 Human interface (PWA)

- **[have]** Fleet tree, chats with agents, terminal tabs, telemetry, usage footer, approval/model/effort pickers, first-run setup, mobile layout.
- **[gap]** Activity view of *all* sessions (managed and observed) per machine with their resource use, plus "other load".
- **[gap]** Machine detail: capacity vs headroom, policy, envelopes, recent history.
- **[gap]** Placement explanations on tasks; work-key profile and history pages; migration timeline.
- **[gap]** Deep links using sids (`/tasks/t-xxxxx`, `/machines/m-xxxxx`, `/chats/c-xxxxx`).

## 6. Non-functional requirements

- **Security:** fleet token for daemons, authenticated browser sessions (or trusted Tailscale identity), bearer tokens for MCP, secrets encrypted at rest, real email addresses never written into repos. Daemons never accept inbound connections.
- **Overhead:** observation must be cheap: daemon CPU < 1% average on an idle machine; incremental tailing of session stores; sampling intervals adaptive (faster while work runs).
- **Scale target:** 20 machines, 200 concurrent sessions, 1 year of downsampled history in SQLite.
- **Reliability:** hub restarts don't stop work; daemons re-adopt managed sessions; observed sessions are re-discovered.
- **Platforms:** Linux (amd64, arm64), macOS (arm64), Windows (amd64), Linux containers and WSL.

## 7. Non-goals (pre-1.0)

- Daemons commanding each other, or any peer-to-peer channel other than hub-arranged SSH data transfers.
- Live migration of running processes.
- Multi-tenant SaaS, RBAC beyond a single operator or trusted team.
- Preemption of running work.
- Kubernetes or Nomad integration.
