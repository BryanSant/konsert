# UI paradigms for Konsert: survey and recommendation

Status: research, November 2026. Input to the UI redesign. Nothing here is decided yet.
Related: [`docs/prd.md`](../prd.md) §2, §3, §5.8a, §5.10.

## TL;DR

- **Konsert's user is a supervisor, not a pair-programmer.** Most UIs in the agent space were built for pairing with one agent and then stretched to cover parallel ones. Their repo → worktree sidebar trees are where the clutter complaints come from: Cursor's forum calls its sidebar "a per-repo history dump" that "buries active work", Codex's sidebar fills with CLI and sub-agent runs, and Orca needs a stack of hide filters plus a community auto-collapse plugin. The tools built for *watching many agents* (Claude Code agent view, Devin, Herdr's mobile inbox, Orca's Agents feed and dashboard, Factory Mission Control) all organise around **who needs the human**, not around where the code lives.
- **Recommended default: "Watch", an exception-first supervisory overview.** From top to bottom:
  - a one-line status banner with an always-visible **Pause everything** button
  - a **Needs you** queue, triaged like an alarm list
  - a **live work list** grouped by state, where each row is a fixed-format "data block": agent, task, machine, account, who is in control, progress, staleness
  - a compact **fleet strip** of machines with headroom and quota
  - the **orchestrator chat docked on the right** in every mode

  Selecting anything opens a **focus pane** (session, machine or task) on the same screen, with all intervention controls and their acknowledgement state.
- **Three alternative modes**, as fixed task-oriented pages in the DaVinci Resolve style rather than free docking:
  1. **Fleet:** a machine and placement topology, Nomad-style, with explanations of why work was placed or not placed.
  2. **Work board:** a flow board by lifecycle state with project swimlanes, used for directing and reviewing work.
  3. **Wall:** a grid of up to 9 live session tiles for times when you want to watch. It is deliberately capped, because detection falls off on bigger walls.
- **Demote the left tree.** Projects, repos, worktrees and spaces become a filter facet (GitHub-Projects-style "Slice by"), a scope chip and command-palette targets. They stop being the primary navigation. Tabs survive only inside the focus pane and the Wall.
- **Interventions get receipts.** Every user action (suspend, veto, hold, take control, pause-all) shows a lifecycle: *sent → applied → agent acknowledged*, with the agent's reply. An action that isn't acknowledged in time escalates. Every session carries one unambiguous **control annunciator**: AUTO / ORCH / ASKING / HELD / YOU / SUSPENDED / STALE. Changes the user didn't make are loud.
- **Mobile is the Needs-you inbox, the banner and Pause everything.** You can peek and reply, suspend or veto and see the receipt, and view a read-only fleet summary. It does not include the board, the wall or terminals, apart from an emergency take-control.

---

## 1. Scope, method and the problem

### 1.1 Jobs to be done (from the brief and the PRD)

| # | Job | PRD anchor |
|---|---|---|
| J1 | **Glance:** is everything OK? What needs me? | §2 visibility, §5.8a |
| J2 | **Understand:** what each agent is doing and why, where, on which machine, using what resources and quota | §5.2, §5.3 |
| J3 | **Intervene** quickly and safely (suspend, veto, take control, pause all) and see that agents acknowledged it | §3 "user is always in charge", §5.8a |
| J4 | **Direct** work through the orchestrator chat | §5.8 |
| J5 | **Review** results (diffs, summaries) | §5.4 outcomes |
| J6 | **Fleet and capacity** awareness and placement decisions | §5.5, §5.6 |
| J7 | **Mobile** check-in | §5.10 |

### 1.2 Today's UI (inventory)

`web/src/app/layout/`:
- **Header:** brand, a Herdr-style tab strip, and Orca-style inspector icons.
- **Left `fleet-sidebar`:** Orchestrator, then Projects (spaces → repos → worktrees), Machines, Agents, Settings, and Add machine.
- **Centre:** tabs for orchestrator chat, agent chats and terminals, with an optional split.
- **Right `telemetry-drawer`:** Agent Activity, Changes, Checks & Runtime, Audit Log.
- **Footer:** hosts, resource manager, PTYs, listening ports, keep-awake, usage rings.

This is an IDE layout, in the Orca/Herdr/VS Code lineage. It is good for J4 and J5 on one session at a time and weak for J1, J3 (fleet-wide), J6 and J7.

### 1.3 Method

I ran four parallel desk-research passes (web search plus reading docs, changelogs, forum threads and papers), and checked the claims the recommendation relies on myself. Each claim cites a source in §9.
- **What I couldn't do:** no hands-on use of the products and no user studies.
- **Discontinued products:** several agent tools changed during 2026. Crystal became Nimbalyst, Superset became Paradev, Terragon shut down, Jules is winding down and Vibe Kanban is sunsetting. These are noted where relevant.
- **Grading:** where evidence is thin it is marked **[thin]**.

---

## 2. Paradigm survey

Each subsection covers what the paradigm is, what it is good at, what it is bad at, and how it maps onto J1 to J7. §2.9 has a scoring matrix.

### 2.1 IDE / worktree-tree agent workbenches

**Products:** Orca, Conductor, Paradev (was Superset), Codex app, Cursor 3 Agents Window, Claude Code desktop, OpenHands Agent Canvas, Sculptor, and Konsert today.

- **Shape:**
  - A left sidebar grouped by project/repo → worktree or session.
  - A centre work area of chat plus terminal, diff and browser panes.
  - Right or bottom panels for diffs and checks.
  - Status shows as small decorations on rows: Orca bolds unread worktrees, Paradev plays a chime and shows a dock badge, Claude desktop sends an OS notification when an unseen session finishes.
- **Good at:**
  - **Deep work on one task (J4/J5):** inline diff comments that go back to the agent (Orca, Codex, Claude desktop, Paradev) and the PR → checks → merge loop (Conductor, Claude "Merge when ready").
  - **Taking over hands-on:** terminal-native tools (Orca, Herdr, Paradev) let you drop into the real terminal, which chat-first tools can't.
  - **Mirroring the code's structure:** the tree matches how the developer thinks about repos.
- **Bad at:**
  - **The tree reflects storage, not attention.** Cursor's sidebar is "unusable for multi-repo orchestration… repo groups bury active work". The staff answer was to switch "Group by" to Status, and staff admitted the option is hidden behind an icon that looks like a filter. Codex users say every CLI and automated run "gets its own entry in Recents", with reports of 200+ sub-agents showing as working. Claude desktop's Recents list "grows forever" (#54044). Orca ships many filters (hide sleeping, automation-created, CLI-created) and has a community plugin to auto-collapse idle projects. Devin hides child sessions under their parent by default.
  - **The vendor fixes all converge on one answer:** group by status, hide automated runs, nest children under parents, auto-archive on merge, and add a separate inbox. They are bolting an attention view onto a storage view.
  - **Little fleet awareness.** Machines are at most a connection target (SSH, cloud). Load, headroom, quota and placement are absent. Herdr is the exception: it lists machines in its sidebar and dims a disconnected machine while showing its cached state.
- **Konsert mapping:** J4 and J5 good; J2 fair for one session; J1, J3 (fleet), J6 and J7 poor. **Keep the focus-pane content (chat, terminal, diff, checks). Drop the tree as the primary navigation.**

### 2.2 Attention-triage lists ("needs you first")

**Products:** Claude Code agent view (`claude agents`), Devin's sidebar "Group by status" and bell inbox, Orca's Agents feed and experimental Needs You / Working / Done dashboard, Herdr's mobile inbox, Kiro's "Needs attention" state, agent-deck. **Non-agent relatives:** Linear Triage and Inbox, PagerDuty Operations Console.

- **Shape:** one list of every session, grouped and ordered by how much it needs a human. In Claude agent view the order is Pinned / Ready for review / Needs input / Working / Completed. Each row has:
  - a state icon (animated for working, yellow for needs input, green done, red failed, grey stopped)
  - a one-line summary written by a model, either the latest status or *the question being asked* ("double jump or wall climb?")
  - an age
  - a PR badge coloured by state

  The interactions are Space to peek and reply without attaching, Enter to attach, Ctrl+X to stop, and a tab title showing "2 awaiting input". Devin's rows name the *specific approval* they are waiting on, with a hover preview. Herdr suppresses alerts for whatever is in focus and treats "done" as "finished while you weren't looking", which clears once viewed.
- **Non-agent relatives worth copying:**
  - **Linear Triage:** single-key Accept / Decline / Duplicate / Snooze, where snooze lasts "until a time you choose or new activity occurs, whichever comes first".
  - **Linear Inbox:** a Priority tab and an Other tab.
  - **Linear My Issues:** a focus order of urgent → SLA → blockers → active → triage → backlog.
  - **PagerDuty Operations Console:**
    - Live vs Paused: Paused stops *new* rows from appearing but still updates the existing ones. It is meant for storms.
    - Up to 5 pinned incidents.
    - Bulk acknowledge and snooze.
    - A detail side panel that remembers its width.
- **Good at:**
  - J1 above all. The answer to "what needs me?" is the first rows of the screen.
  - Peek-and-reply is the fastest J3/J4 loop there is.
  - It scales to hundreds of sessions because healthy ones collapse into groups ("… N more").
- **Bad at:**
  - No spatial sense of the fleet (J6).
  - No project context unless it is added as a facet.
  - A list says nothing about load or *projection*: what will run out, and when.
  - Its quality depends on how good the state detection is. Herdr admits its idle fallback "does not always prove the agent finished".
- **Konsert mapping:** J1 excellent; J3/J4 good through peek; J2 fair; J5 via badges; J7 excellent (it is the natural mobile layout); J6 none. **This is the spine of the default view.**

### 2.3 Flow and kanban boards

**Products:** Vibe Kanban (To do → In progress → In review → Done; sunsetting), Nimbalyst (formerly Crystal; columns backlog / plan / run / review), Linear board, Jira, GitHub Projects, Trello.

- **Good at:**
  - Turning state into a place on screen.
  - A second dimension through swimlanes or "Group by".
  - WIP limits as a capacity signal. In Jira, a column header turns red when over its maximum and yellow when under its minimum. In GitHub Projects, column limits are advisory and highlighted when exceeded.
  - Field sums per column, e.g. tokens or cost per state.
  - "Slice by" as a side facet panel.
  - Saved views, and `Cmd+B` to switch between list and board in Linear.
  - A Jira **"Expedite" swimlane** pinned above "Everything else".
- **Bad at:**
  - **Time and liveness.** A card in "In progress" gives no sign of whether it is moving, stuck or stale.
  - **Where work runs.** Boards have no concept of machines or capacity beyond WIP counts.
  - **Who changes state.** Boards assume humans drag cards. Agent state changes many times a minute, and the user should rarely be dragging anything.
  - **Market signal.** The agent kanban tools are the products shrinking: Vibe Kanban is sunsetting and Crystal pivoted. **[thin]** That evidence is about the market, not about usability.
- **Konsert mapping:** J5 good (an "In review" column is a review queue). J4 fair: dispatch by adding a card. J1 fair: a Needs-you column or an Expedite lane. J2, J3, J6 weak. **Worth having as an alternative mode for planning and review. Not the default.**

### 2.4 Run and workflow UIs (CI, durable workflows, data pipelines)

**Products:** Buildkite, GitHub Actions, Temporal Web UI, Airflow 3, Dagster, Argo Workflows.

- **Good at:**
  - Many concurrent runs, drill-down from run to step to logs, and retry at several levels.
  - **Structured human gates.** Buildkite's block and input steps carry a prompt, fields and select options, and set how a waiting build looks to the outside. Argo's intermediate inputs offer an enum dropdown and a description. GitHub Actions' "Review deployments" has a Waiting state and approve or reject with a comment.
  - **Retry semantics.** Dagster marks "failed, auto-retry pending" as an **amber Warning**, distinct from red **Degraded**. In Temporal, dashed red means retrying and dashed purple means pending, and pending activities show **the current attempt and the time of the next retry**. Temporal's predefined **"Task Failures"** view catches stuck-but-not-failed workflows.
  - **Monitoring shortcuts.** Buildkite's `f` jumps to the next failure, `j` turns on **Follow mode**, and its details drawer docks right, bottom or centre and remembers where it was.
  - **Status cards agents can update.** Buildkite annotations use `--context` to update a card in place and `--priority` to order them.
  - **Health of the system itself.** Airflow puts scheduler, triggerer and processor health on the home page.
  - **Timelines.** Temporal's history Timeline, Compact and Event Groups views, and Grid views of runs × tasks in Airflow and Dagster.
- **Bad at:**
  - Treating the fleet as a first-class thing. Only Buildkite has agent and queue columns.
  - Huge graphs are unreadable. Buildkite admits Canvas is "less useful when zoomed out on large builds".
  - These UIs are built for pipelines that someone wrote in advance. Agent work is more open-ended.
- **Konsert mapping:** J2 very good, both for a single task's attempt history and for "why is it waiting". J3: the gate patterns map directly onto elicitations and override requests. J5 good. **Borrow the vocabulary (attempts, amber retry-pending, structured gates, follow mode, next-failure) for the task and session focus pane. It is not a mode of its own.**

### 2.5 Schedulers and cluster dashboards

**Products:** Nomad UI, k9s, Lens/Freelens, Headlamp, Rancher. Datadog Host Map is covered here too.

- **Good at:**
  - **Nomad topology:**
    - Clients are grouped by datacenter, with height proportional to memory. Each client has a CPU row and a memory row, and allocations are rectangles sized by what they reserve.
    - Clicking an allocation **highlights its sibling allocations across machines**.
    - **Empty clients show red and labelled "empty"**, and drain or ineligible clients carry icons.
    - Its design goals: "optimize for density", zero config, and exploring before anything breaks.
  - **Placement explanations, per reason, as counts.**
    - Nomad: `Constraint "${attr.kernel.name} = windows" filtered 1 nodes`, plus a blocked evaluation "waiting for additional capacity" that retries when capacity appears.
    - Kubernetes: `0/3 nodes are available: 1 Insufficient cpu, 2 Insufficient memory`.
  - **Drain options (Rancher):** grace period, timeout and force.
  - **k9s:**
    - A `:` command bar with context switching (`:pod @ctx`).
    - `/` regex filtering, `?` for the keys that work in the current context, and a `--readonly` mode.
  - **Datadog Host Map:** hexagons filled by a metric and sized by a second metric, with group-by, and nested resources inside each host.
- **Bad at:**
  - Time and history: Nomad's topology is point-in-time only.
  - Reservations look like usage, though they aren't.
  - These tools show state, not work in motion.
  - Topology views lose their labels at scale. That doesn't matter for Konsert's target of about 20 machines.
- **Konsert mapping:** J6 excellent. J2 good for "what's on this machine". J3: drain, cordon and stop-all per machine. **This is the template for the Fleet mode and for the "explain placement" UI (PRD §5.5).**

### 2.6 Observability and incident tools

**Products:** Grafana, Datadog, Honeycomb, htop/btop, PagerDuty, incident.io, status pages.

- **Good at:**
  - **Grafana:**
    - Best-practice guidance: "Display only the servers that are in trouble", one question per dashboard, and drill down from overview to detail.
    - The **State timeline** panel draws one row per entity with segments coloured by state. It is the best answer to "what happened overnight".
    - The **Alert list** sorts by "Importance" (firing > no data > pending > ok > paused) and keeps **No Data** and **Recovering** as states of their own.
  - **Datadog:**
    - The Service Map draws a **worst-of status ring** on each node that rolls up its monitors.
    - **Watchdog** writes anomalies up as grouped "stories" that **expire after 48 h**, with one-click suggested monitors.
  - **Honeycomb BubbleUp:** select an anomalous region and compare it field by field with the baseline. Applied to Konsert, that answers "why is account X draining?" by agent, repo, model or machine.
  - **htop/btop:** dense per-host meters. btop has **numbered layout presets, with preset 0 as a reset that always works**, and rolls child-process resources up into the parent. That is exactly PRD §5.3's process attribution.
  - **PagerDuty acknowledge vs resolve:**
    - Acknowledging claims ownership and halts escalation. **When the ack timeout runs out, the incident goes back to triggered.**
    - Deduplication by key.
    - Content-based grouping, with a preview that replays past alerts under new settings.
  - **incident.io:** a curated timeline of state changes plus pinned items, which "reads like a story rather than a log".
  - **Status pages:** a one-line top banner, five fixed component states, and 90-day bars of daily history.
- **Bad at:**
  - Free-form dashboards spread without limit; Grafana's own maturity model calls this out.
  - Query-first tools are poor ambient displays.
  - Paging setup is heavy.
- **Konsert mapping:** J1 and J6 very good. J3 through acknowledgement semantics. **Borrow:** the banner, worst-of rings, state timelines, timed acknowledgements, stories and grouping.

### 2.7 Supervisory-control HMI (process control, ATC, mission control, cockpits, CCTV)

This is the closest analogue to Konsert's actual job: a person supervising automation that runs itself most of the time.

- **ISA-101 / High-Performance HMI (Hollifield et al.):**
  - Grey, low-saturation baseline, with **colour reserved for abnormal states**.
  - Analog indicators that show a value against its normal and alarm range, and embedded trends instead of bare numbers.
  - A four-level display hierarchy: area overview → unit → equipment → diagnostics.
  - **Evidence is contested [thin]:** an industry-funded simulator study (EPRI 2009) found better operator performance, but there is no peer-reviewed RCT, and critics in the trade press call grey screens boring and sleep-inducing. Even proponents say "it's not exclusively about colour".
- **Alarm management (ISA-18.2 / IEC 62682 / EEMUA 191, 4th ed. 2024):**
  - An alarm *requires an operator response*. Anything else is a notification.
  - Target rates: about 1 alarm per 10 minutes per operator is acceptable, about 2 is the maximum manageable. EEMUA's target is under 1.
  - Flood: >10 alarms in 10 minutes **[thin, secondary sources]**.
  - **Shelving** is temporary, operator-controlled suppression with an expiry, visible to others and logged. The "4 h maximum with auto-unshelve" figure is **[thin]**.
  - **Rationalisation:** every alarm has a defined response, consequence and priority.
- **SCADA overview:** the whole plant on one screen with no scrolling at normal size, and a separate alarm summary instead of alarms mixed into the graphic.
- **Air traffic control:**
  - Each target carries a fixed-format **data block**, full or limited.
  - **Flight strips do cognitive work.** When paper strips were removed, controllers became slower to grant requests and spent more time on the radar (FAA AM96-05). The lesson: let the human order, flag and annotate items, and don't auto-sort away their working memory.
  - **Nuisance-alert rates are high** (62% of en-route Conflict Alerts were judged unnecessary, and 91% of minimum-altitude warnings), and the FAA warns they "may desensitize controllers".
- **NASA mission control:**
  - Fixed **positions** with call signs and areas of responsibility.
  - **Voice loops** let controllers overhear relevant traffic "without disrupting their own activities" (Patterson, Watts-Perotti & Woods 1999).
  - A **go/no-go poll** before critical events, where any position can call no-go.
- **Mode annunciation and "who is in control":**
  - Airbus FMA puts a white box around a mode for about 10 seconds after a change.
  - Autopilot disconnect is deliberately loud (MASTER WARN, red AP OFF, the "cavalry charge" aural) and must be acknowledged.
  - **Asiana 214 (NTSB):** an unnoticed autothrottle HOLD mode, with the system's complexity "increased the likelihood of mode error".
  - **AF447 (BEA):** the stall warning *stopped* when airspeed data went invalid, a perverse cue.
  - **Tesla (NTSB HAR-20/01):** steering-torque monitoring was an inadequate proxy for whether the driver was engaged.
- **CCTV walls:**
  - There is no traceable standard for how many feeds one operator can watch.
  - One prison study found about 83–84% detection with 4–9 monitors and about 64% with 16 **[thin; secondary sources all cite this one study]**.
  - Detection was perfect only when one display was watched at a time (Donald et al. 2015).
  - Vigilance degrades after roughly 20–40 minutes.
- **Good at:** exactly J1 and J3. A quiet normal state, exceptions that stand out, unambiguous control state, and loud uncommanded changes.
- **Bad at:** these systems assume trained operators, fixed plants and long shifts, so they are less helpful for J4 and J5 (directing creative work, reviewing diffs).
- **Konsert mapping:** **this is the source of the visual language and alerting rules for every mode** (see §6).

### 2.8 Pro tools with workspaces and docking

**Products:** Photoshop, Blender, DaVinci Resolve, Bloomberg Launchpad, TradingView, VS Code.

- **Good at:**
  - **DaVinci Resolve:** seven fixed *pages* ordered along the workflow (Media → Cut → Edit → Fusion → Color → Fairlight → Deliver), one click apart. The layout is deliberately not dockable, so nobody gets lost.
  - **Blender:** task-workspace tabs, with free area-splitting confined *inside* a tab. A workspace can also carry context (a pinned scene, an object mode, an add-on filter).
  - **VS Code:**
    - **Bounded docking:** views move only between a few named containers.
    - Reset per view and reset all.
    - Detachable editor groups.
    - Profiles that apply by context and can be exported.
  - **Bloomberg Launchpad:** **link groups**. Change the security in one component and every component in its group follows, while other groups stay independent.
  - **TradingView:** sync split into separate axes (*symbol*, *interval*, *time range*, *crosshair*), with group membership shown by an emoji badge on each chart.
- **Bad at:**
  - **Docking chaos.** Photoshop users complain that the active workspace changes silently as they move panels, that "Lock Workspace" doesn't fully lock, and that the usual fix is "Reset Essentials".
  - **Resolve's fixed layout frustrates experts:** forum threads ask "let us move panels around please".
  - **Bloomberg's learning curve** is notoriously steep.
- **Konsert mapping:** a *mechanism*, not a paradigm. **Use Resolve/Blender-style fixed modes**, VS Code's bounded resize-and-collapse inside each mode, Bloomberg/TradingView-style selection linking, and btop/Photoshop's lesson that "reset to preset" must always be one action away. **Reject free docking** for a single-operator monitoring tool: it costs a lot to build, raises support load and invites the "lost my layout" failure, and nothing in J1–J7 needs it. **[Opinion]**

### 2.9 Scoring matrix

● strong, ◐ partial, ○ weak. This is the author's judgment from the evidence above.

| Paradigm | J1 glance | J2 understand | J3 intervene + ack | J4 direct | J5 review | J6 fleet | J7 mobile |
|---|---|---|---|---|---|---|---|
| IDE / worktree tree (today) | ○ | ◐ | ◐ (per session) | ● | ● | ○ | ○ |
| Attention-triage list | ● | ◐ | ● (peek, reply, stop) | ◐ | ◐ | ○ | ● |
| Flow / kanban board | ◐ | ○ | ○ | ◐ | ● | ○ | ○ |
| Run / workflow UI | ◐ | ● (one task) | ◐ (gates) | ○ | ● | ○ | ○ |
| Scheduler / topology | ◐ | ◐ | ◐ (drain) | ○ | ○ | ● | ○ |
| Observability / incident | ● | ◐ | ◐ (ack) | ○ | ○ | ● | ◐ |
| Supervisory HMI | ● | ◐ | ● | ○ | ○ | ◐ | ◐ |
| Mission-control wall | ◐ | ● (≤9) | ◐ | ○ | ○ | ◐ | ○ |
| Free docking / presets | depends on layout | depends | depends | depends | depends | depends | ○ |

No single paradigm covers everything. The triage list and the HMI rules together cover J1, J3 and J7, which are the jobs a *monitoring* tool exists for. The IDE focus pane covers J4 and J5. Topology covers J6. That points to a composite default, plus a few specialised modes.

---

## 3. Human-factors evidence (what constrains the design)

| Finding | Source | Design consequence for Konsert |
|---|---|---|
| **Automation adds supervisory duties; it doesn't remove the human.** It leaves people with the hardest tasks and takes away the practice they need for them. Monitoring for rare events is exactly what humans are bad at. | Bainbridge 1983; Simkute et al. 2024 (GenAI ironies) | Never make safety depend on the user watching. Taking control must arrive *with context*: goal, last N actions, plan, and what changed since the user last looked. |
| **Out-of-the-loop.** Higher levels of automation lower situation awareness and slow recovery when automation fails. Intermediate levels keep people engaged. | Endsley & Kiris 1995; Kaber & Endsley | Show plans and checkpoints before work runs. "Approve plan" is a cheap way to keep the user in the loop. |
| **Types × levels of automation.** Acquisition, analysis, decision and action each have their own level. Human-performance effects are the primary criterion. | Parasuraman, Sheridan & Wickens 2000; Sheridan & Verplank 1978 | Make autonomy visible per session (approval mode, override rights). The control annunciator shows the *current* level. |
| **Situation awareness has three levels:** perception, comprehension, projection. Most SA errors happen at Level 1 (the information wasn't perceived). | Endsley 1995 | Every data block carries state (L1), goal and progress (L2), and projection (L3): "quota runs out ~14:20", "context ~85%", "ETA". Projection is the level most often missing and the most useful for scheduling. |
| **Management by exception** depends on the automation reporting its own exceptions. Complacency and automation bias are attentional and not cured by expertise. | Dekker & Woods 1999; Parasuraman & Manzey 2010 | Exceptions must come from *independent* checks (policy, budget, tests, no progress in N minutes), not only from the agent asking for help. Silence is a signal. |
| **Alarm fatigue.** 89.5% of ICU arrhythmia alarms were false. Up to 99% of clinical alarms are non-actionable. ATC nuisance alerts desensitise controllers. | Drew et al. 2014; reviews; FAA | Every interrupting item names an action, or it becomes a notification. Track the actionable rate as a health metric for Konsert itself. Group floods by root cause. |
| **Alarm-rate targets:** about 1 per 10 minutes acceptable, about 2 maximum. | ISA-18.2, EEMUA 191 | An **attention budget**: aim for ≤ 6 interrupting items per hour on average, *for the human* rather than per agent. Show it in Settings. [Industrial targets are a proxy; Konsert's pace is unknown, so treat as **[thin]**.] |
| **Mode confusion and automation surprises** come from uncommanded mode transitions and weak feedback. | Sarter & Woods 1995; Asiana 214; AF447 | One annunciator per session. Transitions the user didn't start are loud and must be acknowledged. Losing data shows **STALE** and never quietly clears a warning. |
| **Engagement can't be inferred from proxies.** | NTSB HAR-20/01 (Tesla) | An agent's acknowledgement of a hold or veto must be *explicit* and shown. Silence is "unacknowledged", never "fine". |
| **Trust calibration:** match trust to capability, specific to function and situation. | Lee & See 2004 | Show track records per agent, model, account and work key (success, veto and revert rates). Show process (plan, evidence), not just outcomes. |
| **Interruption timing.** User-negotiated delivery is best overall, and immediate delivery only where timeliness is critical. Interrupting at breakpoints costs less. A short warning helps people resume. | McFarlane 2002; Iqbal & Bailey 2006–08 | Default to a pull queue with a badge. Push only P1. Batch P3. Never steal focus from a terminal the user is typing in. |
| **Glanceability:** abstraction, few states and low distraction. | Matthews 2006–07 | The banner and PWA badge have about 3 states (OK / N need you / something's wrong), readable in under a second. |
| **Watching live feeds degrades** past a handful of feeds and after about 20–40 minutes. | CCTV literature [thin] | Cap the Wall at 9 tiles and have it suggest which ones deserve a tile. |
| **Recent agent-oversight studies.** ParallelPilot (n=16): an ambient dashboard plus planning and logging raised throughput 63% and supervision by +1 agent at peak, but **did not improve perceived control or perceived success at redirecting agents**. Grunde-McLaughlin et al. 2026: a better trace UI cut time-to-find-errors and raised confidence, but **not accuracy**. | arXiv 2609.33113; arXiv 2602.16844 (preprints, small n) **[thin]** | Awareness isn't enough. Pair the overview with *cheap evidence* (diff, tests, plan) and with interventions that verifiably take effect. Don't optimise for confidence: measure whether interventions worked. |
| **Oversight is shifting from approving each action to monitoring and interrupting.** Experienced Claude Code users use full auto-approve more (about 20% → 40%+ of sessions) *and* interrupt more (about 5% → 9% of turns). On complex tasks Claude asks for clarification more than twice as often as humans interrupt it. | Anthropic, "Measuring AI agent autonomy in practice" (Feb 2026) | Agent-initiated questions are the largest source of needs-you items, so make answering them cheap (peek and reply). Interrupting must be one keystroke or tap, from anywhere. |
| **Interruptibility as a governance requirement:** pause, redirect, stop and reverse, without the agent being able to tamper. | OpenAI "Practices for governing agentic AI"; Singapore Consensus 2026 | Pause everything is enforced by Konsert (hold plus SIGSTOP), not requested from the agent. The receipt shows whether the enforcement *and* the acknowledgement happened. |

---

## 4. Recommendation

### 4.1 Shape of the product

```
 Modes (fixed pages, keys 1-4):  [1 Watch]  [2 Work]  [3 Fleet]  [4 Wall]
 Levels (ISA-101-style drill):   overview ──▶ focus pane ──▶ full page (deep link)
 Always present in every mode:   status banner + Pause everything · Needs-you count · ⌘K
                                 orchestrator dock (collapsible) · selection linking
```

- **Modes** are task-oriented pages, as in Resolve and Blender. Inside a mode you can resize and collapse regions (VS Code-style bounded layout), and "Reset layout" is always one click away. There is no free docking.
- **Selection is linked**, Bloomberg-style. Selecting a session in any mode makes the focus pane, the orchestrator dock's context chip and the fleet strip highlight follow it. Selecting a machine filters the work list. One link group is enough for v1. **[Opinion: multiple groups are a power-user feature to add once the Wall exists.]**
- **The three levels**, applied in every mode:
  - L1 is the mode's overview.
  - L2 is the **focus pane**: a right-hand or slide-over panel for the selected session, task, machine or account.
  - L3/L4 is a full page with a deep link (`/s/c-xxxx`, `/t/t-xxxx`, `/m/m-xxxx`; PRD §5.10 gap): transcript, terminal, diff, attempt history, process tree.

### 4.2 Default mode: **Watch** (the supervisory overview)

**Why it should be the default:**
- It serves the jobs a monitoring tool exists for: J1, J3, and J7 when shrunk.
- It follows how the multi-agent tools are converging (needs-you first).
- It applies the HMI rules: quiet normal state, separate alarm summary, and one screen with no scrolling at the target scale.
- It keeps J4 (orchestrator dock) and J5 (focus pane) one step away.

```
┌──────────────────────────────────────────────────────────────────────────────────────────────┐
│ ● konsert   1 Watch  2 Work  3 Fleet  4 Wall      ⌘K search/command       [❚❚ PAUSE EVERYTHING]│
├──────────────────────────────────────────────────────────────────────────────────────────────┤
│ ▲ 2 need you · 1 held · 14 working · 3 queued · fleet 5/6 up (gpu-box STALE 4m) · claude-a 81%│  ← status banner (grey when OK)
├───────────────────────────────────────────────────────────────┬──────────────────────────────┤
│ NEEDS YOU (2)                                     [snooze] [⇧A ack all]│ ORCHESTRATOR             ▾ │
│ ▌P1 veto not acknowledged · codex@mini-pc · t-8k2  "fix flaky e2e"  │ ctx: ☐ t-8k2 (selected)      │
│ ▌   you vetoed 48s ago ("wrong approach") — no ack yet   [retry][kill]│ ──────────────────────────── │
│ ▌P2 question · claude@desk · c-3fa "which migration tool?" [peek ␣] │ you: put the release builds  │
│ ▌P2 override request · orchestrator · "allow 2nd session on mac"    │      on the mac, not desk    │
│      justification: CVE fix · 30m          [approve] [deny] [details]│ orch: ok — pinning work key  │
├───────────────────────────────────────────────────────────────┤  release/* → mac (rule r-12) │
│ LIVE WORK            group: state ▾  slice: all projects ▾  ⌕ filter│  2 tasks re-queued. [explain]│
│ ─ held / suspended (1) ───────────────────────────────────────│ ──────────────────────────── │
│  HELD  grok  t-71c  api: rate limiter   desk     ⏸ by you 12m  ✓ack │ > message orchestrator…      │
│ ─ working (14) ───────────── sorted by attention, then age ───│                              │
│  AUTO  claude c-3fa web: auth refactor  desk   ▁▂▃▅ 3/5 steps 22m │──────────────────────────────│
│        acct claude-a 81% (resets 14:20) · ctx 64% · cpu 1.4c      │ YOUR ACTIONS (recent)        │
│  ORCH  codex  t-8k2 e2e: flaky test     mini-pc ▁▁▁▁ no progress 9m⚠│ 12:02 hold t-71c  ✓ grok ack │
│  AUTO  cursor c-9d1 docs: api ref       mac    ▃▅▆▇ 4/6 steps  8m │ 11:58 veto t-8k2  … waiting  │
│  YOU   term   s-2aa ops shell           desk   you have control   │ 11:40 drain gpu-box ✓ 2 moved│
│  …11 more working ▸            observed (user-started) 3 ▸        │                              │
│ ─ queued (3) ── 2 waiting for capacity: "no mac slot free" [why] ─│                              │
│ ─ done, unseen (4) ── 2 ready for review (PR #212 ✓ checks) ──────│                              │
├───────────────────────────────────────────────────────────────┴──────────────────────────────┤
│ FLEET  desk ▇▇▅░ 3/4  mini-pc ▅▃░░ 2/4  mac ▇▇▇▇ 4/4 FULL  wsl ▂░░░ 0/2  gpu-box STALE ░░░░  │  ← fleet strip: headroom bars,
│ QUOTA  claude-a ▇▇▇▇▇▇▇▇░░ 81% →14:20   claude-b ▃░ 18%   codex ▅▅░ 45%   copilot ▂ 9%       │     agent vs other load, quotas
└──────────────────────────────────────────────────────────────────────────────────────────────┘
```

**Rows are ATC-style data blocks.** The collapsed block has a fixed format:
`[CONTROL] agent  id  project: title  machine  [progress sparkline] [progress text] [age]`.
Expanding a row adds the account and quota projection, context %, CPU/RAM, PR or check badge, and the latest one-line summary.
- The annunciator always occupies the first column: AUTO / ORCH / ASKING / HELD / SUSPENDED / YOU / STALE.
- **Observed sessions** (started by the user directly; PRD §5.2) appear in the same list with an `observed` marker and read-only controls where enforcement isn't possible.

**Clicking a row opens the focus pane, which replaces the orchestrator dock or slides over it on narrow screens:**

```
┌─ FOCUS: c-3fa  claude · web: auth refactor ───────────────────────────────── [⤢ full page] [×] ┐
│ CONTROL  AUTO  (approval: ask-on-write) · started by orchestrator 22m ago · desk (m-4kd)       │
│ [␛ stop turn] [⏸ suspend] [✋ hold] [⛔ veto…] [⌨ take control] [⋯ kill/cancel]                  │
│ GOAL  "refactor session handling to use new auth middleware" · plan 3/5 ▸                      │
│ NOW   editing src/auth/session.ts · last tool: run tests (41/60 pass) 40s ago                  │
│ NEXT  ETA ~15m (profile: web/* median 31m) · quota claude-a hits 95% ~14:05 ⚠                  │
│ RES   cpu 1.4c · mem 2.1G (peak 3.0G) · 7 procs ▸ · ctx 64%                                     │
│ ──────────────── tabs: [Activity] [Chat] [Terminal] [Changes 4 files] [Attempts 1] [Placement]  │
│ Activity: curated timeline (state changes, tool calls, your actions, agent acks), live tail    │
└────────────────────────────────────────────────────────────────────────────────────────────────┘
```

- The GOAL / NOW / NEXT / RES header gives Endsley's comprehension (L2) and projection (L3) on every session.
- The tabs replace today's centre tab strip *and* right inspector. Activity is the old Agent Activity, Changes the old Changes, and Attempts and Placement the old Checks & Runtime. Each is scoped to the selected item.

### 4.3 Alternative mode A: **Fleet** (machines, capacity and placement, J6)

**Model:** Nomad topology combined with the Datadog Host Map and the Grafana State timeline.

```
┌ 3 Fleet ──────────────── group: role ▾  fill: cpu ▾  [drain] [suspend all] [stop all] on selected ┐
│ DEDICATED                                                  INTERACTIVE                            │
│ ┌ desk (linux, 16c/64G) ──────────────┐ ┌ mini-pc ───────┐ ┌ mac (user active ●) ───────────────┐ │
│ │cpu ▇▇▇▇▇▇▅▅░░ agent 9c · other 2c   │ │cpu ▅▅▃░░░░░    │ │cpu ▇▇▇▇▇▇▇▇▇░ FULL (cap 4)         │ │
│ │mem ▇▇▇▇▅░░░░░ 31/64G                │ │mem ▅▃░░░░░░    │ │mem ▇▇▇▇▇▇░░░░                      │ │
│ │[c-3fa ▆][t-71c ⏸][s-2aa YOU][    ]  │ │[t-8k2 ⚠][    ] │ │[c-9d1][t-55a][t-55b][t-55c]        │ │
│ │repos: web ✓ api ✓ docs ✓            │ │repos: e2e ✓    │ │accts: claude-a claude-b copilot    │ │
│ └─────────────────────────────────────┘ └────────────────┘ └────────────────────────────────────┘ │
│ ┌ wsl ─── EMPTY (2 slots) ───┐ ┌ gpu-box ── STALE 4m (last: 1 session, 70% gpu) [why] ───────────┐│
├───────────────────────────────────────────────────────────────────────────────────────────────────┤
│ QUEUE  t-90a "release build"  waiting for capacity:  mac FULL (cap 4) · desk ✗ needs macOS ·     │
│        wsl ✗ no xcode · gpu-box ✗ stale  → retries automatically  [explain] [pin…] [override…]  │
├───────────────────────────────────────────────────────────────────────────────────────────────────┤
│ STATE TIMELINE (last 12h)  desk   ███▒▒███████░░░███   mac ░░░██████████   gpu-box ███████▓▓▓XXX   │
│                            claude-a quota ▁▂▃▅▆▇▇█ limit@03:10 ··· reset 08:00                     │
└───────────────────────────────────────────────────────────────────────────────────────────────────┘
```

- **Machine cards:**
  - Height or size reflects capacity.
  - Bars split **agent load vs other load** (PRD §5.3).
  - Session chips show their own annunciator.
  - **Empty** and **stale** machines are called out, as in Nomad.
- **Clicking a chip** highlights that task's sibling attempts and fan-out across machines, as Nomad does with sibling allocations.
- **The queue shows placement reasons per machine, with counts.** This is the UI for PRD §5.5 "explain placement", including a dry run before dispatch.
- **Machine controls:** drain, with Rancher-style options (finish current, migrate, force after a timeout), suspend all and stop all.
- **Migrations** (§5.6) draw as an arrow on the timeline from the old machine to the new one, with the transfer path (git remote / SSH / relay).

### 4.4 Alternative mode B: **Work** (a flow board by lifecycle, J4/J5)

**Model:** the Linear board (`Cmd+B` to toggle board and list), Jira's Expedite lane, and GitHub Projects' Slice by and field sums. Lanes are projects or work keys. Columns are lifecycle states owned by Konsert, so agents and the scheduler move the cards and the user rarely drags. The review column is the main reason this mode exists.

```
┌ 2 Work ───── lanes: project ▾  slice: machine/agent/account ▾  [board ⇄ list ⌘B]  [+ new task] ┐
│            │ QUEUED (3)     │ RUNNING (14) max 16│ NEEDS YOU (2)  │ REVIEW (2)       │ DONE today 9│
│ ⚑ EXPEDITE │                │                    │ t-8k2 veto ⏳   │                  │             │
│ web        │ t-90a release  │ c-3fa auth 3/5 ▆   │ c-3fa? → (dup) │ #212 login fix   │ #208 ✓      │
│            │  wait: mac full│ c-9d1 docs 4/6     │                │  ✓checks 4 files │             │
│ api        │                │ t-71c ⏸ HELD       │ override req.  │ #213 limiter     │ #205 ✓ #207 │
│ e2e        │ t-91b          │ t-8k2 ⚠ no prog 9m │                │                  │             │
│ (observed) │                │ 3 user sessions    │                │                  │             │
│ Σ tokens   │ —              │ 1.2M               │                │ 340k             │ 2.9M        │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
 Card click → focus pane (Changes tab for REVIEW cards: diff, summary, receipts vs. acceptance list,
             inline comments back to the agent, [approve PR] [request changes] [veto]).
```

- The **WIP limit** in each column header turns amber when over and dims when under (idle capacity).
- The **"Retrying"** state is amber inside RUNNING, with the attempt number and the time of the next retry (Dagster/Temporal). It is not shown as failed.
- **Review cards show agent "receipts"** checked against acceptance criteria, as in Project Progress Desk **[thin, demo-only]**, alongside checks and diffs.

### 4.5 Alternative mode C: **Wall** (watching live sessions on purpose, J2)

**Model:** a mission-control or CCTV wall, constrained by the evidence.

```
┌ 4 Wall ───── layout: 2×2 ▾ (max 3×3)   fill: [pinned] + [suggested: needs you / anomalous] ─────┐
│ ┌ c-3fa claude · desk · AUTO ─────────┐ ┌ t-8k2 codex · mini-pc · ORCH ⚠ no progress 9m ─────┐  │
│ │ ▸ editing src/auth/session.ts        │ │ $ npm test -- e2e/login                            │  │
│ │   running tests… 41/60               │ │   (same failure ×3)                                │  │
│ │ [␣ reply] [⏸] [✋] [⌨ take]           │ │ [␣ reply] [⏸] [✋] [⛔ veto] [⌨ take]               │  │
│ └──────────────────────────────────────┘ └────────────────────────────────────────────────────┘  │
│ ┌ s-2aa terminal · desk · YOU ────────┐ ┌ (suggested) c-9d1 cursor · mac · AUTO ──── [pin][×] ┐  │
│ │ live PTY (interactive)               │ │ ▸ writing docs/api.md                              │  │
│ └──────────────────────────────────────┘ └────────────────────────────────────────────────────┘  │
│ banner + Needs-you count stay visible; a P1 anywhere flashes the banner, never rearranges tiles │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
```

- Tiles show a live tail or the PTY, which is streamed only while visible (PRD's edge principle).
- **The cap is 9.** The default is 4, made of pinned tiles plus *suggested* ones (needs you, anomalous, recently changed).
- **Tiles never move on their own.** The user's arrangement is their working memory, as with flight strips.
- This mode replaces today's split panes and multi-tab terminals for anyone who wants to watch. A detachable second-window version of the Wall or Watch view is the "second monitor" story.

### 4.6 Considered and not recommended (for now)

- **Free docking with saved workspaces (Photoshop / Bloomberg Launchpad):** high build and maintenance cost, the "lost my layout" failure, and no job that requires it. Revisit only if users ask for a custom multi-monitor wall. Bounded resize and collapse plus four modes covers the need. Store per-mode layouts and offer "Reset layout".
- **Keeping the repo/worktree tree as the left rail:** see §2.1. Its information survives as a facet (§4.7).
- **A Grafana-style dashboard builder:** dashboards spread without limit, and Konsert should be opinionated (PRD §3). Ship fixed, good views.
- **A separate "orchestrator mode":** the orchestrator is how you *direct* work, so it belongs in a dock available in every mode, not on a page that hides the work.

### 4.7 What changes from today

| Today | Proposal |
|---|---|
| Left tree: Orchestrator / Projects (spaces → repos → worktrees) / Machines / Agents | **Removed as primary navigation.** Projects/repos/worktrees become a **Slice-by facet** and **scope chip** ("web ▾") in Watch and Work, lanes in Work, and targets in ⌘K (`:repo web`, `:m mac`, k9s-style). Machines move to the Fleet mode and fleet strip. Agents are the work list. The orchestrator moves to the dock. Spaces fold into saved views (filter + grouping + mode), as in Temporal's Saved Views. |
| Centre tab strip as the main surface | **Tabs only inside the focus pane** (Activity / Chat / Terminal / Changes / Attempts / Placement) and in the Wall. Tabs are no longer the way you find things; the work list is. |
| Right inspector (Agent Activity, Changes, Checks & Runtime, Audit Log) | Becomes **the focus pane's tabs**, always scoped to the selection. The Audit Log becomes the **"Your actions" timeline** (filtered to user actions plus agent responses) and a full audit page. |
| Footer (hosts, resource manager, PTYs, ports, keep-awake, usage rings) | **Status banner** (top, the glance line) plus **fleet and quota strip** (bottom of Watch). PTYs and ports move to machine focus pages. Keep-awake moves to Settings or the machine page. |
| Status shown by colour everywhere | **Grey baseline.** Colour is reserved for states that need action. One annunciator column per row. |
| Stop / cancel / take-control per session only | **Controls at every scope** (session / machine / fleet; PRD §5.8a) with receipts. Pause everything is pinned in the header. |

### 4.8 How "needs you" works (exception-based)

**Classify by required action** (alarm rationalisation; ISA-18.2):

| Priority | What | Delivery | Examples |
|---|---|---|---|
| **P1 interrupt** | Time-critical, and harm is possible without action | Banner flashes red, OS/push notification, sound optional | An intervention not acknowledged within N seconds. An uncommanded control change (agent resumed while held, take-control lost). An agent about to do something irreversible outside policy. A machine down with running managed work. Policy breach. |
| **P2 queue** | Needs a decision, can wait for a breakpoint | Needs-you list and badge; push on mobile only if older than X | Elicitations (permission, question, auth). Override requests (break glass). Open requests ("restart nginx"). Repeated failure with no automatic retry left. Quota about to run out on queued work. |
| **P3 notify** | Informational | Counts in the banner and "done, unseen" group; batched | Done, ready for review, retry succeeded, migration finished, learned rule recorded. |

**Rules:**
- Each item states **the expected action** and offers it inline: answer, approve or deny, retry, kill, release. An item with no action is demoted to P3. **[ISA-18.2; clinical fatigue data]**
- **Acknowledge, snooze and shelve are separate:**
  - **Ack** (PagerDuty): "mine, stop escalating". It **re-fires after a timeout** if the item is unresolved.
  - **Snooze** (Linear): until a time *or* until new activity, whichever comes first.
  - **Shelve**: suppress a class of items on a scope with an expiry, logged and shown in the banner as "2 shelved".
- **Group floods by root cause:**
  - "gpu-box STALE: 3 sessions affected" is one P1, not four. Key on (machine, account, failure class), as in PagerDuty content-based grouping.
  - During a flood, offer a **Paused list** mode (new items are counted but not inserted), as in the PagerDuty Ops Console.
- **Retry-pending is amber, not red** (Dagster). STALE / NO DATA is its own grey-hatched state (Grafana "No Data"; the AF447 lesson). **"No progress in N minutes"** is an exception Konsert raises itself, rather than waiting for the agent to ask (Temporal's Task Failures view; management by exception).
- **Attention budget:**
  - Settings shows "interrupts/hour (7-day avg)" and the "% of needs-you items you acted on".
  - If the actionable rate falls below about 50%, suggest demoting or shelving the noisiest class, as Watchdog's suggested monitors do in reverse.
  - **[Opinion; thresholds need tuning on real use.]**
- **Respect focus.** Nothing steals keyboard focus from a terminal the user controls (YOU state). P1 shows on the banner and in a toast. If an item belongs to the session in focus, it is suppressed elsewhere (Herdr).

### 4.9 How interventions and acknowledgements show

Every user action is a tracked object, an **intervention receipt**. It ties together PRD §5.8a's holds, attribution and event delivery.

```
 ⛔ veto t-8k2 "wrong approach: don't mock the auth server"         by you · Watch UI · 11:58:02
   ✓ sent → hub            11:58:02
   ✓ enforced              11:58:03   process tree stopped (SIGTERM), hold placed on t-8k2 scope
   ✓ orchestrator notified 11:58:04   recent_events delivered
   ✓ acknowledged          11:58:19   orch: "Understood — won't retry with mocks; will ask before
                                       re-dispatching e2e work. Proposed: use test auth container."
   ○ hold released         —          [release hold] [reply]
```

- **Lifecycle:** `sent → enforced (by Konsert) → delivered → acknowledged (agent's own words) → released / resolved`. Enforcement and acknowledgement are shown separately. Pause everything is effective once *enforced*, whatever the agent says. The acknowledgement shows that the agent *understood*, which matters because §3 requires agents not to treat user actions as failures to investigate. **[NTSB HAR-20/01: don't infer engagement from proxies; interruptibility literature.]**
- **Where it shows:**
  - A small receipt chip on the affected row (`⏸ by you 12m ✓ack`, or `… waiting 48s`).
  - The "Your actions" timeline in the dock.
  - The focus pane's Activity tab.
- **Timeouts:** if acknowledgement doesn't arrive within N seconds (default 30 s for managed sessions, configurable), the receipt becomes **P1 "not acknowledged"** with actions [resend] [suspend] [kill]. Observed sessions can't acknowledge, so their receipts end at *enforced* or *not enforceable* and say which.
- **Control annunciator** on every session, machine and the fleet:

  | Label | Meaning | Visual |
  |---|---|---|
  | `AUTO` | Agent working on its own within policy | grey text |
  | `ORCH` | Directed by the orchestrator (dispatched or steered) | grey text, orchestrator glyph |
  | `ASKING` | Agent waiting on the human | amber |
  | `HELD` | User hold; nothing automatic happens on this scope | blue outline |
  | `SUSPENDED` | Process tree frozen | blue fill |
  | `YOU` | The user has the terminal or session | white-on-blue, strongest |
  | `STALE` | No data from the daemon; the last known state is shown dimmed | grey hatched |

  - Any change of annunciator gets a **10-second highlight box** (Airbus FMA).
  - A change the user didn't make (`HELD → AUTO` without a release, losing `YOU`) is **P1 and must be acknowledged** (autopilot disconnect).
  - The fleet level has one too: `FLEET: RUNNING` or `FLEET: PAUSED by you 3m` in the header, beside the Pause button.
- **Handing back control** ("hand back" after `YOU`) asks for, or drafts, a short summary for the agent. It is a receipt too, with an acknowledgement. **[PRD §5.8a; Bainbridge: handover with context.]**
- **Go/no-go for risky fleet-wide actions** (resume all after a pause, approve a break-glass override): show a poll of affected sessions with ready / blocked / objection before confirming. **[NASA go/no-go; opinion that it is worth building after v1.]**

### 4.10 Navigation and the orchestrator

- **Keyboard first:**
  - `1`–`4` switch modes, `⌘K` opens the palette, `:` opens a k9s-style command line (`:s @mac`, `:t failed`, `:m gpu-box`), `/` filters, `?` shows the keys for the current context.
  - `j`/`k` move. `␣` peeks (reply without opening). `Enter` focuses. `f` jumps to the next exception (Buildkite). `F` follows the active item (Buildkite follow mode).
  - `p` pauses everything, with confirmation by hold-to-confirm or a second press.
- **Deep links for everything** (`/s/…`, `/t/…`, `/m/…`, `/a/<account>`, `/r/<receipt>`), so notifications open straight onto the item.
- **The orchestrator dock:**
  - It is collapsible and present in every mode. On narrow screens it is a bottom sheet.
  - It is **context-aware**: the current selection appears as a removable chip ("ctx: t-8k2"), so "move this to the mac" needs no ids.
  - Its actions (dispatch, pin, migrate, override request) appear inline as **cards with an [explain] link** to the placement log, and in the Activity timelines of the affected items.
  - Orchestrator decisions that need the user (override requests, ambiguous plans) are posted as **P2 Needs-you items** as well as chat messages, so they can't scroll out of sight.
  - Direct chat with any single agent lives in that session's focus pane, Chat tab, so "the orchestrator" and "an agent" are never confused.
- **The orchestrator's loop** (mission-control "voice loops"): a dimmed, collapsible feed of what the orchestrator is doing ("dispatched t-90a to mac; rejected desk: needs macOS"), separate from the chat. The user can follow it passively without being interrupted.

### 4.11 Mobile story (J7)

Mobile is not a reflow of the desktop. It is the **pull queue plus the emergency brake**, and the evidence supports this: Herdr, Orca, Superset/Paradev and Omnara all centre mobile on "needs you" and replying.

```
┌──────────────────────────┐   ┌──────────────────────────┐   ┌──────────────────────────┐
│ konsert   FLEET: RUNNING │   │ ← c-3fa claude · desk    │   │ ← Fleet (read-only)      │
│ ▲ 2 need you · 14 working│   │ ASKING · web: auth       │   │ desk    ▇▇▅░ 3/4         │
│ [❚❚ PAUSE EVERYTHING]    │   │ "which migration tool?"  │   │ mini-pc ▅▃░░ 2/4         │
├──────────────────────────┤   │  ○ goose  ○ atlas  ○ … │   │ mac     ▇▇▇▇ FULL        │
│ P1 veto not ack'd  48s   │   │ [reply…]          [send] │   │ gpu-box STALE 4m         │
│    t-8k2 · mini-pc  ›    │   │──────────────────────────│   │ claude-a 81% → 14:20     │
│ P2 question  c-3fa    ›  │   │ GOAL / NOW / NEXT        │   │ codex    45%             │
│ P2 override req.      ›  │   │ [⏸ suspend] [✋ hold]     │   │ (tap → machine summary)  │
├──────────────────────────┤   │ [⛔ veto…]  [⋯]           │   └──────────────────────────┘
│ Working 14 ›  Review 2 › │   │ receipts: ⏸ 12:02 ✓ack   │
│ Your actions ›  Fleet ›  │   └──────────────────────────┘
└──────────────────────────┘
```

- **Included:**
  - The banner plus fleet control state, and Pause everything.
  - The Needs-you inbox with peek-and-reply and structured answers (Buildkite/Argo-style option buttons).
  - Session controls with receipts.
  - Working and review lists (with PR summary and diff stats), "Your actions", and a read-only fleet and quota summary.
  - Push only for P1, plus P2 items older than a threshold.
  - The PWA badge shows the count of needs-you items.
- **Excluded:** the board, the Wall, layout controls and placement editing. Terminals are only available as "emergency take control" behind a confirmation, because typing in a remote shell on a phone is error-prone. **[Opinion]**
- **Orchestrator:** a full-screen chat sheet with voice dictation, since "tell the orchestrator" is a natural mobile action.

---

## 5. How the modes cover the jobs

| Job | Watch (default) | Work | Fleet | Wall | Mobile |
|---|---|---|---|---|---|
| J1 glance | ● banner, needs-you, groups | ◐ Needs-you column | ◐ stale/empty/full | ◐ | ● |
| J2 understand | ● data blocks + focus pane | ◐ | ● what's on each machine | ● live | ◐ |
| J3 intervene + ack | ● inline controls, receipts | ◐ via focus | ● drain/suspend/stop per machine | ● per tile | ● |
| J4 direct | ● dock | ● new task, dock | ◐ pin/override | ○ | ◐ chat sheet |
| J5 review | ◐ "ready for review" group → focus | ● REVIEW column | ○ | ○ | ◐ summary/approve |
| J6 fleet / placement | ◐ fleet strip, queue reasons | ○ | ● topology, explain, timeline | ○ | ◐ read-only |
| J7 mobile | the mobile layout is Watch reduced | n/a | n/a | n/a | ● |

---

## 6. Design principles (with sources)

1. **Organise around attention, not storage.** The first thing on screen is what needs the human. Repos and worktrees are facets. *(Claude Code agent view; Devin; Cursor and Codex sidebar complaints; Linear Triage.)*
2. **Quiet when normal; colour means act.** A grey baseline, with saturated colour only for states that need action. Use shape and position as well as colour. *(ISA-101 / Hollifield; EPRI 2009; contested, so test it.)*
3. **Every alert names an action, and there is a budget for them.** Anything without an action is a notification. Aim for about 1 interrupt per 10 minutes, group floods, and allow ack, snooze and shelve with expiries. *(ISA-18.2, EEMUA 191; Drew et al. 2014; PagerDuty; Linear.)*
4. **Always show who is in control.** One annunciator per session, machine and fleet. A change of control is highlighted, and a change the user didn't make is loud and must be acknowledged. *(Airbus FMA; Sarter & Woods 1995; NTSB Asiana 214.)*
5. **Interventions are enforced by Konsert and acknowledged by agents, and both are shown.** Never infer acknowledgement from silence, and escalate when it is missing. *(PRD §3/§5.8a; NTSB HAR-20/01; OpenAI governing practices; Singapore Consensus.)*
6. **Stale is a state.** Losing data never clears a warning or looks like "fine". *(BEA AF447; Grafana No Data; Herdr dimmed cached machines.)*
7. **Perception, comprehension and projection on every item:** state, goal and progress, and what happens next (ETA, quota run-out, context limit). *(Endsley 1995.)*
8. **Overview, then focus, then detail, all on one screen with linked selection.** Drill-down follows display-hierarchy levels, and selection propagates. *(ISA-101 levels; Grafana best practices; Bloomberg link groups; TradingView sync.)*
9. **Pull by default, push only when time-critical, and never steal focus.** *(McFarlane 2002; Iqbal & Bailey; Herdr focus suppression.)*
10. **Don't depend on watching.** Live views are optional and capped (≤ 9), and the system suggests what deserves a tile. *(CCTV literature [thin]; vigilance research; Bainbridge 1983.)*
11. **Awareness must lead to cheap evidence and effective control.** Diff, tests, plan and receipts are one keystroke away. Measure whether interventions worked, not how confident users feel. *(ParallelPilot 2026; Grunde-McLaughlin et al. 2026.)*
12. **Make the automation's track record and reasoning visible**, specific to agent, account and work key. *(Lee & See 2004.)*
13. **Opinionated modes, bounded layout and a reset that always works.** No free docking, no dashboard builder. *(DaVinci Resolve pages; Blender workspaces; VS Code bounded docking; Photoshop complaints; btop preset 0; PRD §3.)*
14. **Let users arrange their working set and never reorder it under them.** Pins, manual order and annotations persist. *(FAA flight-strip studies; Claude agent view Shift+↑/↓ reorder.)*

---

## 7. Where evidence is thin, and open questions

- **Grey-scale HMI.** The supporting evidence is industry and simulator studies with no peer-reviewed RCT, and critics call it boring and sleep-inducing. Konsert's dark UI already starts close to grey. *Mitigation:* an A/B with a "colourful" theme once the redesign exists, measuring time to spot the needs-you item.
- **Attention-budget numbers.** The ISA-18.2 and EEMUA rates come from control rooms with trained operators on shift. Konsert's user is part-time and often doing something else. Treat ~6 per hour as a starting hypothesis and instrument it.
- **The Wall cap.** The 4/9/16-monitor numbers trace back to one prison CCTV study. The cap of 9 is a judgment call.
- **The value of kanban for agent work.** The agent kanban tools are the products shrinking (Vibe Kanban sunsetting, Crystal pivoting). That is market evidence, not usability evidence. Work mode is recommended mainly for its REVIEW column. If usage is low, fold that column into Watch's "ready for review" group and drop the mode.
- **Agent UI studies are 2026 preprints with small samples** (n≈14–16), and some vendor features came only from changelog titles or search snippets: Conductor's "Workspace Status", Orca's experimental dashboard, Warp's panels.
- **State detection quality decides everything.** An attention-first UI is only as good as Konsert's ability to tell `working`, `asking`, `stalled` and `done` apart for *observed* sessions (PRD §5.2 [gap]). Herdr admits heuristic idle detection is unreliable. **The redesign depends on the session-store parsers.**
- **Open question:** should the default for a brand-new user with one machine and no orchestrator be a simplified Watch, or the orchestrator chat? *Suggestion:* Watch, with the dock expanded and an empty-state prompt "Tell the orchestrator what to do", which collapses once work exists.
- **Open question:** is acknowledgement text from the agent worth the tokens on every intervention? *Suggestion:* yes for the orchestrator and for sessions directly affected, but through a cheap structured ack (`ack: understood/objection + one line`) delivered in the next-prompt note (PRD §5.8a event delivery).

## 8. Suggested migration path from today's UI

1. **Watch, version 1.**
   - Make the work list (a list grouped by state, with data blocks and an annunciator) the default centre view.
   - Move the current right inspector into a focus pane.
   - Turn the footer into a banner plus fleet strip.
   - Pin Pause everything in the header.
   - Keep the old tree behind a toggle for one release, then remove it.
2. **Needs-you and receipts:** P1/P2/P3 classification, ack/snooze/shelve, intervention receipts, and the annunciator. This depends on PRD §5.8a holds and event delivery.
3. **Fleet mode** together with process attribution, headroom and the placement log (PRD §5.3, §5.5).
4. **Mobile Watch** (inbox, peek-and-reply, Pause) and PWA push for P1.
5. **Work board** (REVIEW column first) and **Wall**, then decide from usage whether to keep both.

---

## 9. Sources

### Agent-work tools
- Orca: https://github.com/stablyai/orca · https://orca.dev/docs/model/worktrees · https://orca.dev/docs/activity · https://onorca.dev/docs/ssh · https://www.onorca.dev/changelog/1-4-162 · https://onorca.dev/plugins/eliasjnior/orca-plugins
- Herdr: https://herdr.dev/ · https://herdr.dev/faq · https://flaviocopes.com/herdr/
- Conductor: https://www.conductor.build/llms-full.txt · https://www.conductor.build/docs/core/parallel-agents · https://www.conductor.build/changelog · https://thenewstack.io/a-hands-on-review-of-conductor-an-ai-parallel-runner-app/
- Superset / Paradev: https://raw.githubusercontent.com/superset-sh/superset/main/README.md · https://raw.githubusercontent.com/ParadevAI/Paradev/HEAD/README.md · https://thebridge.jp/2026/10/paradev-superset-yc-parallel-coding-agents-seed-funding · https://hn.svelte.dev/item/46368739
- Claude Code: https://claude.com/blog/claude-code-desktop-redesign · https://code.claude.com/docs/en/desktop · https://code.claude.com/docs/en/agent-view · https://code.claude.com/docs/en/mobile · https://code.claude.com/docs/en/claude-code-on-the-web · https://github.com/anthropics/claude-code/issues/54044
- OpenAI Codex: https://openai.com/index/introducing-the-codex-app/ · https://learn.chatgpt.com/docs/code-review?surface=app · https://learn.chatgpt.com/docs/features · https://community.openai.com/t/codex-cli-and-agent-sessions-flood-the-desktop-app-sidebar-with-no-way-to-scope-them/1387453 · https://community.openai.com/t/codex-cloud-web-original-code-review-workflow-lost-legacy-task-history-disappearing/1416056 · https://community.openai.com/t/codex-app-does-not-display-worktree-threads/1373261
- GitHub Copilot / Agent HQ / mission control: https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/manage-and-track-agents · https://github.blog/ai-and-ml/github-copilot/how-to-orchestrate-agents-using-mission-control/ · https://github.blog/news-insights/company-news/welcome-home-agents/
- Cursor: https://cursor.com/blog/cursor-3 · https://forum.cursor.com/t/agents-window-sidebar-unusable-for-multi-repo-orchestration-no-recent-list-no-multi-chat-view-repo-groups-bury-active-work/166317 · https://forum.cursor.com/t/worktrees-in-agent-sidebar/156637
- Devin: https://docs.devin.ai/release-notes/2026 · https://docs.devin.ai/work-with-devin/devin-review
- Factory: https://docs.factory.com/factory-app/overview · https://factory.ai/news/factory-desktop · https://docs.factory.ai/jp/web/missions
- OpenHands: https://docs.openhands.dev/openhands/usage/agent-canvas/overview · https://docs.openhands.dev/openhands/usage/cloud/cloud-ui
- Warp: https://docs.warp.dev/agents/capabilities/agent-notifications/ · https://docs.warp.dev/platform/managing-cloud-agents/
- Vibe Kanban: https://raw.githubusercontent.com/BloopAI/vibe-kanban/HEAD/README.md
- Crystal / Nimbalyst: https://nimbalyst.com/app/ · https://raw.githubusercontent.com/stravu/crystal/v0.3.4/README.md
- Sculptor: https://imbue.com/sculptor/
- Amp: https://ampcode.com/news/agents-panel · https://ampcode.com/news/agents-everywhere
- Kiro: https://kiro.dev/docs/web/autonomous-mode/
- Claude Squad: https://raw.githubusercontent.com/smtg-ai/claude-squad/main/README.md
- agent-deck: https://raw.githubusercontent.com/asheshgoplani/agent-deck/main/README.md
- Omnara: https://apps.apple.com/app/id6748426727
- Jules: https://jules.google/docs/review-plan/ · https://techradar.com/pro/google-kills-off-jules-its-ai-coding-agent-which-promised-to-do-all-the-jobs-you-didnt-want-to
- Terragon: https://vibed-lab-agents.pages.dev/agent/terragon-labs-terragon-oss

### Boards, CI/workflow, schedulers
- Linear: https://linear.app/docs/triage · https://linear.app/docs/inbox · https://linear.app/docs/board-layout · https://linear.app/docs/my-issues
- Jira: https://support.atlassian.com/jira-software-cloud/docs/configure-swimlanes/ · https://support.atlassian.com/jira-software-cloud/docs/configure-columns/
- GitHub Projects: https://docs.github.com/en/issues/planning-and-tracking-with-projects/customizing-views-in-your-project/changing-the-layout-of-a-view · https://docs.github.com/en/issues/planning-and-tracking-with-projects/customizing-views-in-your-project/customizing-the-board-layout
- Buildkite: https://buildkite.com/docs/pipelines/build-page · https://buildkite.com/resources/changelog/266-introducing-the-new-build-page-engineered-for-scale-and-flexibility/ · https://buildkite.com/resources/changelog/356-a-simpler-build-page-layout-with-a-new-list-view/ · https://buildkite.com/releases/2024-q1/investigate-jobs-from-the-waterfall-view · https://buildkite.com/docs/pipelines/configure/step-types/block-step · https://buildkite.com/docs/agent/v3/cli-annotate
- GitHub Actions: https://docs.github.com/en/actions/how-tos/monitor-workflows/use-the-visualization-graph · https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs · https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/review-deployments
- Temporal: https://docs.temporal.io/web-ui · https://temporal.io/blog/the-dark-magic-of-workflow-exploration · https://temporal.io/changelog/updated-event-history-timeline-view-is-now-available
- Airflow: https://airflow.apache.org/docs/apache-airflow/stable/ui.html
- Dagster: https://dagster.io/docs/guides/observe/asset-health-status · https://dagster.io/docs/concepts/webserver/ui · https://www.github.com/dagster-io/dagster/pull/21934
- Argo: https://argo-workflows.readthedocs.io/en/latest/intermediate-inputs/ · https://oneuptime.com/blog/post/2026-08-02-argo-workflows-retry-vs-resubmit/view
- Nomad: https://www.hashicorp.com/blog/see-your-entire-cluster-at-once-with-nomad-s-topology-visualization · https://developer.hashicorp.com/nomad/docs/monitor/cluster-topology · https://developer.hashicorp.com/nomad/tutorials/web-ui/web-ui-submit-job · https://developer.hashicorp.com/nomad/docs/v2.0.x/concepts/scheduling/placement · https://developer.hashicorp.com/nomad/commands/v1.9.x/eval/status · https://developer.hashicorp.com/nomad/docs/release-notes/nomad/v1_8_x
- Kubernetes tools: https://k9scli.io/ · https://k9scli.io/topics/commands/ · https://docs.lenshq.io/k8slens/using-lens/nodes/ · https://headlamp.netlify.app/blog/2025/01/17/making-sense-of-kubernetes-with-headlamps-map-view · https://newreleases.io/project/github/headlamp-k8s/headlamp/release/v0.18.0 · https://oneuptime.com/blog/post/2026-03-19-rancher-cordon-drain/view · https://repost.aws/knowledge-center/eks-pod-scheduling-node-availability · https://learn.microsoft.com/troubleshoot/azure/azure-kubernetes/availability-performance/troubleshoot-pod-scheduler-errors

### Observability, ops, pro tools
- Grafana: https://grafana.com/docs/grafana/latest/panels-visualizations/visualizations/state-timeline/ · https://grafana.com/docs/grafana/latest/panels-visualizations/visualizations/alert-list/ · https://grafana.com/docs/grafana/latest/visualizations/dashboards/build-dashboards/best-practices/ · https://grafana.com/blog/grafana-drilldown-apps-the-improved-queryless-experience-formerly-known-as-the-explore-apps/
- Datadog: https://docs.datadoghq.com/infrastructure/hostmap/ · https://docs.datadoghq.com/tracing/services/services_map/ · https://docs.datadoghq.com/watchdog/alerts/
- Honeycomb: https://docs.honeycomb.io/investigate/analyze/identify-outliers/
- htop / btop: https://man7.org/linux/man-pages/man1/htop.1.html · https://github.com/aristocratos/btop
- PagerDuty: https://docs.pagerduty.com/incident-management/incidents/overview · https://docs.pagerduty.com/ai-automation/aiops/operations-console · https://support.pagerduty.com/docs/intelligent-alert-grouping · https://support.pagerduty.com/docs/content-based-alert-grouping · https://support.pagerduty.com/docs/time-based-alert-grouping · https://support.pagerduty.com/docs/unified-alert-grouping
- incident.io: https://incident.io/changelog/tell-the-full-story-with-your-incident-timeline · https://incident.io/changelog
- Statuspage: https://support.atlassian.com/statuspage/docs/display-historical-uptime-of-components · https://community.atlassian.com/t5/Statuspage-questions/Component-status-quot-Degraded-performance-quot-is-not-marked/qaq-p/2590668
- Photoshop: https://community.adobe.com/t5/photoshop-ecosystem-discussions/photoshop-workspace-keeps-resetting-to-an-undesired-layout/td-p/13416025 · https://community.adobe.com/t5/photoshop-ecosystem-discussions/lock-workspace-not-possible/m-p/12080396/highlight/true
- Blender: https://docs.blender.org/manual/en/latest/interface/window_system/workspaces.html
- DaVinci Resolve: https://www.blackmagicdesign.com/products/davinciresolve · https://forum.blackmagicdesign.com/viewtopic.php?p=833905 · https://forum.blackmagicdesign.com/viewtopic.php?p=852939
- Bloomberg: https://www.bloomberg.com/ux/2017/11/10/relaunching-launchpad-disguising-ux-revolution-within-evolution/ · https://www.bloomberg.com/ux/2020/08/11/consistency-more-than-just-a-buzzword/ · https://www.bloomberg.com/company/stories/how-bloomberg-terminal-ux-designers-conceal-complexity
- TradingView: https://in.tradingview.com/support/solutions/43000629992 · https://www.tradingview.com/blog/en/interval-syncing-in-tradingview-desktop-55068
- VS Code: https://code.visualstudio.com/docs/configure/custom-layout · https://code.visualstudio.com/docs/configure/profiles

### Supervisory control and HMI
- ISA-101 / HP-HMI: https://www.automation.com/article/the-high-performance-hmi · https://control.com/technical-articles/going-gray/ · https://restservice.epri.com/publicdownload/000000000001017637/0/Product · https://www.automationworld.com/article/technologies/hmi-software/great-graphics-debate-going-gray-good-hmis · https://www.automationworld.com/control/article/22031298/the-case-for-more-colorful-hmi-graphics · https://gevernova.com/software/blog/busting-myths-about-high-performance-hmi
- Alarm management: https://www.yokogawa.com/us/library/resources/media-publications/implementing-alarm-management-per-the-ansi-isa-182-standard-control-engineering/ · https://www.icheme.org/media/17392/lc-0151_21-lead-process-safety-metrics-alarm-rationalisation-final.pdf · https://www.icheme.org/media/16019/icheme-alarm-management-practical-guide-06_05_2021.pdf · https://eemua.org/getattachment/9d3f8071-55c3-49bf-a74a-3bf6ad4a2e0f/Contents-EEMUA-Publication-191-Edition4-November-2024.pdf · https://library.e.abb.com/public/72f20c70c7b44d889d463db81df5c38d/SCADA%20Alarm%20Management%20White%20Paper.pdf
- ATC: https://hf.tc.faa.gov/publications/2008-nuisance-alerts-in-operational-atc-environments/ · https://hf.tc.faa.gov/publications/2007-human-factors-analysis/ · https://www.faa.gov/data_research/research/med_humanfacs/oamtechreports/1990s/1996/9605 · https://www.faa.gov/data_research/research/med_humanfacs/oamtechreports/1990s/1998/9826 · https://commons.erau.edu/ntas/2020/presentations/67/
- NASA mission control: https://link.springer.com/article/10.1023/A:1008722214282 · https://www.interruptions.net/literature/Patterson-CSCW-JCC99.pdf · https://www.interruptions.net/literature/Watts-CSCW96-p48-watts.pdf · https://en.wikipedia.org/wiki/Flight_controller · https://wikipedia.com/wiki/Launch_status_check
- Mode annunciation / accidents: https://www.ntsb.gov/investigations/Pages/DCA13MA120.aspx · https://ntsb.gov/news/events/Documents/Asiana%20Board%20Meeting%20Operations.pdf · https://www.faa.gov/sites/faa.gov/files/f-cp090601.en_.pdf · https://spectrum.ieee.org/riskfactor/aerospace/aviation/air-france-flight-447-crash-caused-by-a-combination-of-factors · https://www.ntsb.gov/investigations/AccidentReports/Reports/HAR2001.pdf · https://static.nhtsa.gov/odi/rcl/2023/RCLQRT-23V838-9544.pdf · https://static.nhtsa.gov/odi/inv/2024/INOA-RQ24009-12046.pdf · https://safetyfirst.airbus.com/app/themes/mh_newsdesk/iframe.php?p=68662 · https://docs.flybywiresim.com/pilots-corner/a32nx/a32nx-briefing/flight-deck/glareshield/fcu/
- CCTV: https://ipvm.com/forums/video-surveillance/topics/video-operator-viewing-effectiveness · https://www.frontiersin.org/journals/human-neuroscience/articles/10.3389/fnhum.2013.00615/pdf · https://pmc.ncbi.nlm.nih.gov/articles/PMC3786256 · https://www.sintef.no/globalassets/project/hfc/documents/spl386-r-cctv-literature-survey-finalversion.pdf

### Human factors and agent oversight
- Sheridan & Verplank 1978: https://ntrs.nasa.gov/citations/19790007441
- Parasuraman, Sheridan & Wickens 2000 (DOI 10.1109/3468.844354): https://dblp.uni-trier.de/rec/journals/tsmc/ParasuramanSW00.html
- Bainbridge 1983: https://gwern.net/doc/sociology/technology/1983-bainbridge.pdf · Simkute et al. 2024: https://arxiv.org/pdf/2402.11364 · Ergonomics editorial: https://informahealthcare.com/doi/full/10.1080/00140139.2023.2296176
- Endsley & Kiris 1995: https://dblp.org/rec/journals/hf/EndsleyK95 · https://pubmed.ncbi.nlm.nih.gov/10048306/
- Endsley 1995 SA (DOI 10.1518/001872095779049543): https://ntrs.nasa.gov/citations/19950007634
- Parasuraman & Manzey 2010: https://journals.sagepub.com/doi/10.1177/0018720810376055
- Alarm fatigue: https://pmc.ncbi.nlm.nih.gov/articles/PMC4206416 · https://pmc.ncbi.nlm.nih.gov/articles/PMC5403122 · https://pmc.ncbi.nlm.nih.gov/articles/PMC11851092
- Glanceability (Matthews): https://www2.eecs.berkeley.edu/Pubs/TechRpts/2007/EECS-2007-56.html · https://www2.eecs.berkeley.edu/Pubs/TechRpts/2006/EECS-2006-113.html
- Sarter & Woods: https://ntrs.nasa.gov/citations/19980016965 · https://ntrs.nasa.gov/api/citations/19940008774/downloads/19940008774.pdf
- Lee & See 2004: https://pubmed.ncbi.nlm.nih.gov/15151155/ · https://csel.eng.ohio-state.edu/productions/intel/research/trust/Lee%20%26%20See%20Trust%20Review.pdf
- Interruptions: https://www.interruptions.net/literature/McFarlane-HCI02_2.pdf · https://hci.rwth-aachen.de/materials/conferences/CHI2004/1p287.pdf · https://www.ideals.illinois.edu/items/11122
- ParallelPilot (2026): https://arxiv.org/abs/2609.33113v1
- Overseeing agents without constant oversight (2026): https://arxiv.org/abs/2602.16844
- Project Progress Desk (2026): https://arxiv.org/abs/2611.13287
- OrchVis (2025): https://arxiv.org/html/2510.24937v1
- Anthropic, Measuring AI agent autonomy in practice (2026): https://www.anthropic.com/research/measuring-agent-autonomy
- OpenAI, Practices for governing agentic AI systems: https://cdn.openai.com/papers/practices-for-governing-agentic-ai-systems.pdf · Auto-review: https://alignment.openai.com/auto-review
- Singapore Consensus (2026): https://arxiv.org/pdf/2608.14611

---

## 10. Decision (2026-11-23, after review of the mockups)

The owner reviewed the clickable mockups and decided:

- **Work, Fleet and Wall are accepted** as designed, with three changes:
  - Fleet's state timeline gets a range picker: 12 h (default), 24 h, 3 days, 1 week.
  - Wall tiles are any session type: chat tiles have their own reply box; terminal tiles are typeable after Take control. Up to 9 tiles are visible per page and the Wall pages horizontally through more pinned sessions (soft cap ~24); only visible tiles stream.
- **"Watch" is replaced by "Command"**, the desktop default and a chat-first page: live work on the left, a tabbed conversation area in the centre (the orchestrator as a pinned tab; any agent chat, terminal or observed session opens as another tab, with a GOAL/NOW strip and controls on agent tabs), Needs you on the right, the status/Pause control on top and the fleet/quota strip at the bottom. On desktop, chatting, brainstorming, organising and queuing work is the main job; the orchestrator is front and centre, not a side dock.
- **Mobile defaults to watching and answering**: the needs-you inbox with live work and Pause everything. Chat, new work and Fleet are reachable but not the default.
- **First run**: Command opens with a welcome in the Orchestrator tab that explains Konsert and offers "Add this machine" (the orchestrator needs a machine to run on); the side panels explain what will appear there; the composer enables once a machine is online.
- These modes **replace** the current IDE-style layout (left tree, centre tab strip, right inspector); they are not added beside it.
- **Pause everything is a quiet emergency control**: always visible but low-prominence ("Pause all" in the desktop header; a small "Emergency: pause everything" link at the bottom of the mobile inbox, never a large top button), behind a confirmation that explains the effect with Cancel as the default. Resume needs no confirmation. Like an emergency number: comforting to see, rarely used.
