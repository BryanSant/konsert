# Work model and tracker sync

Status: design, not built. Replaces the Space > Project > Repo > Worktree taxonomy.
Related: [autopilot.md](autopilot.md) (how work is executed), [prd.md](../prd.md) §5.4.

## Decisions

1. **The hub owns live work state** in SQLite. Autopilot needs atomic leases, DAG unblocking
   and several state transitions per second; a git-backed store can't give that without
   conflicts. The hub is already the single always-on authority.
2. **The data model is KanArtist's.** Hierarchy, IDs, statuses, acceptance items, comments,
   questions and `blocked_by` follow the ka Store Spec (`../ka/spec/v1`), reusing
   `github.com/KanArtist/ka/pkg/kaspec` types and validation where they fit. Users who know ka
   know Syre's board.
3. **External trackers are sync adapters**, not sources of truth for runtime state. ka is the
   first adapter (two-way); GitHub Projects, GitLab issues, Jira, Linear and Obsidian follow on
   the same interface.
4. **Agents never talk to a tracker or the hub directly.** They use ka-like MCP tools served by
   their local `syred`, which relays to the hub (the existing per-chat MCP relay).
5. **One ticket per lease.** A daemon leases one ticket at a time. Batch checkout of an epic or
   chain is deferred: hub↔daemon traffic costs no tokens, so batching only helps offline or
   flaky machines.

## Hierarchy

```
Project ── Initiative ── Epic ── Ticket
   │                              │
   └── repos (registry)           └── repo bindings, runs (tasks), digests
```

- **Project**: the conceptual product. Owns a repo registry, defaults (verify command, delivery
  mode, merge style, autonomy policy) and its tracker links. A project can add, drop, rename or
  fork repos without restructuring its work.
- **Initiative, Epic**: containers (ka kinds `initiative`, `epic`) linked by `parent`. Rollups
  (done/total, blocked, running) are computed, never stored.
- **Ticket**: the unit Autopilot leases. Kinds as in ka (feature, bug, chore, spike).
- **Ad-hoc work**: a task dispatched without a ticket (scratch terminal, a quick chat, a
  Commander one-off) stays allowed; it belongs to a project and has no lease or DAG.

`Workspace` and `Space` go away (pre-1.0: no migration). The project's repo registry replaces
`repos`; per-machine checkouts and worktrees become facts the daemons report (see PRD §5.5
locality) instead of taxonomy rows.

## Repo registry and bindings

- **Repo** (per project): `id`, `sid`, `name`, `remote_url`, `default_branch`, `verify_cmd`,
  `delivery_mode`, `merge_style`. Identity is the remote URL, not a path.
- **Checkout** (reported by daemons): `repo_id`, `worker_id`, `path`, `branch`, `last_fetch`.
- **Repo binding** (per ticket, 1..n): `repo_id`, `base_branch`, `target_branch`. The daemon
  provisions one worktree per binding under one ticket workdir:
  `<workdir root>/.syre/tickets/<ticket sid>/<repo name>`. A binding with no local checkout
  is cloned to `<workdir root>/.syre/clones/<repo name>` (a dot directory, so checkout
  discovery skips it). Worktrees are removed when the ticket reaches
  `done` and its branches are delivered (or after a retention period for `failed`).

## Ticket fields

ka fields, plus Syre's runtime fields:

| Field | Owner | Notes |
|---|---|---|
| `id`, `sid`, `project`, `parent`, `kind`, `title`, `body`, `acceptance[]`, `labels`, `priority` | human / tracker | ka semantics |
| `blocked_by[]` | human / tracker / Commander | DAG; cycles rejected |
| `repo_bindings[]` | human / Commander | |
| `hints` | human / Commander | `machine`, `agent`, `model`, `effort`, `os`, `arch`, `gpu`, `min_ram_gb`, `tools[]` |
| `verify_cmd` | human / Commander | ticket-level command; how it runs is in [autopilot.md](autopilot.md). When it is empty, each repo's `verify_cmd` runs in that repo's worktree |
| `autonomy` | project default, per-ticket override | `retry_limit`, `cross_review`, `escalate_on_blocker`, `expand_scope` |
| `status` | Syre | see below |
| `lease` | Syre | `worker_id`, `task_id`, `expires_at`, `heartbeat_at` |
| `attempts[]`, `digest`, `artifacts` | Syre | see autopilot.md |

**Statuses** (configurable per project like ka workflows; these are the defaults and the
categories Autopilot reasons about):

| Status | Category | Board column |
|---|---|---|
| `backlog` | not ready | (hidden by default) |
| `ready` | queued | Queued |
| `running` | active | Running |
| `needs_you` | blocked on a human | Needs you |
| `review` | awaiting review/delivery | Review |
| `done` | closed | Done |
| `failed`, `canceled` | closed | Done (filtered) |

A ticket is *dispatchable* when status is `ready`, every `blocked_by` ticket is `done` (or
`review` when the project enables optimistic chaining), it has at least one repo binding, and
no hold applies.

## Leases

- The scheduler claims a dispatchable ticket with a single SQLite transaction that sets
  `status=running` and the lease (`expires_at = now + lease_ttl`, default 10 minutes).
- The daemon's heartbeat renews leases for its running tickets. A lease that expires (machine
  lost) follows the ticket's orphan policy: requeue to `ready` with a new attempt, or `failed`.
- Users and the Commander can break a lease (cancel, requeue, move); the action is attributed
  like every other intervention.

## Agent-facing work tools (via syred)

Every managed session working a ticket gets a small tool set, served by its local `syred` and
relayed to the hub. Sessions see only their own ticket and its context.

| Tool | Purpose |
|---|---|
| `ticket_get` | The dispatch packet again (objective, acceptance, contracts, repos, verify) |
| `ticket_check` | Mark an acceptance item done |
| `ticket_comment` | Progress or note (short; shown on the board) |
| `ticket_followup` | Propose a follow-up ticket (title, why, scope); becomes a `discovered_followup` in the digest |
| `ask_user` | Existing; routes to the Commander or the human per policy |
| `request_user_action` | Existing |

The session never sees tracker credentials, other tickets or the backlog.

## Tracker sync

```go
type Tracker interface {
    Kind() string                                   // "ka", "github-projects", "jira", ...
    Pull(ctx context.Context, cursor string) (changes []WorkChange, next string, err error)
    Push(ctx context.Context, changes []WorkChange) ([]PushResult, error)
    Capabilities() TrackerCaps                      // hierarchy depth, DAG, acceptance items, comments, custom fields
}
```

- **Field ownership** decides conflicts. Human-owned fields (title, body, acceptance text,
  priority, parent, labels) take the tracker's value. Syre-owned fields (status while leased,
  lease, attempts, digest) take Syre's. `blocked_by` merges as a set. Each adapter declares a
  mapping for what its tracker can't represent (e.g. Jira has no acceptance items: they map to
  a checklist field or the description).
- **Links**: each Syre ticket keeps `external_refs[]` (`tracker`, `external_id`, `url`,
  `version`). Tickets created in Syre push out; tickets created in the tracker pull in.
- **Cursor and cadence**: pull on an interval (default 30s) and on webhook where the tracker
  has one; push on change, debounced.
- **ka adapter**: the hub embeds ka's engine as a library and acts as one more replica of the
  ka store, so `ka` on any host (inside Syre or not) sees the same backlog and Syre's leases.
  Requires extracting ka's `internal/engine` (index, query, operations, sync, merge driver)
  into `pkg/`, and a ka Store Spec addition: a claim with `authority: <external system>` that ka
  clients show read-only instead of competing for.
- **Built-in only**: projects with no tracker use Syre's tables directly; nothing else needed.

## Board

The Work mode board reads tickets (and ad-hoc tasks) instead of raw tasks. Columns map from
status categories; swimlanes are Expedite plus one per Initiative or Epic (user's choice).
Ticket cards show the lease (machine, agent), attempt count, last digest line and blockers.
The same service layer backs the board, REST and the Commander's MCP tools.
