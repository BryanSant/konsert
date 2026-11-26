-- Syre schema. Pre-1.0 there is exactly one migration: schema changes edit this file and
-- older databases are refused (see migrate.go) rather than upgraded.
--
-- IDs: every entity table has two identifiers.
--   id  UUIDv7 primary key. Every foreign key column holds one, as do the hub<->worker
--       protocol and the web app's internal state. workers.id is syred's own
--       persistent UUID.
--   sid short typed ID from internal/ids, unique, shown to users and accepted in API
--       paths next to the id: t-7qk3m task, c-w47xk chat, m-… machine, p-… project,
--       r-… repo, tk-… ticket,
--       h-… hold, rq-… user request, q-… question, o-… override request, v-… veto,
--       dr-… policy directive.
-- Event/log/child tables keep integer or composite keys. External identities live in
-- their own columns: chat_sessions.agent_session_id (the agent CLI's session) and
-- tasks.external_id (the worker's key for a discovered session).

CREATE TABLE workers (
    id               TEXT PRIMARY KEY,
    sid              TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    host_group       TEXT NOT NULL,
    os               TEXT NOT NULL,
    arch             TEXT NOT NULL,
    isolation        TEXT NOT NULL DEFAULT 'host',
    permission_mode  TEXT NOT NULL DEFAULT 'full_access',
    agents_json      TEXT NOT NULL,
    tags_json        TEXT NOT NULL,
    max_concurrent   INTEGER NOT NULL DEFAULT 2,
    status           TEXT NOT NULL,
    version          TEXT NOT NULL DEFAULT '',
    facts_json       TEXT NOT NULL DEFAULT '',
    facts_updated_at TEXT,
    last_seen_at     TEXT NOT NULL,
    -- How the hub (re)starts this machine's syred: '' started by hand / unknown,
    -- 'local' the hub's own host (syred install), 'ssh' syred install over SSH to
    -- ssh_target.
    launch           TEXT NOT NULL DEFAULT '',
    ssh_target       TEXT NOT NULL DEFAULT '',
    -- Last start attempt: '' (none pending / connected), 'starting', 'failed'.
    launch_state     TEXT NOT NULL DEFAULT '',
    launch_error     TEXT NOT NULL DEFAULT '',
    launch_at        TEXT,
    -- Drained machines finish their current work and receive nothing new.
    drained          INTEGER NOT NULL DEFAULT 0,
    drained_at       TEXT,
    -- Per-machine default terminal shell ID ('' = worker default).
    default_shell       TEXT NOT NULL DEFAULT '',
    -- Windows: configure agent CLI shell tools to use Git Bash when available (1 = on).
    agents_use_git_bash INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE projects (
    id            TEXT PRIMARY KEY,
    sid           TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    is_default    INTEGER NOT NULL DEFAULT 0,
    -- headline is the project's current goal, set by the commander (set_narration).
    headline      TEXT NOT NULL DEFAULT '',
    headline_at   TEXT,
    -- Verification and autonomy defaults (docs/design/work.md).
    verify_cmd    TEXT NOT NULL DEFAULT '',
    autonomy_json TEXT NOT NULL DEFAULT '{}',
    -- Autopilot Play toggle and explicit pause override (blocks global autopilot when 1).
    autopilot        INTEGER NOT NULL DEFAULT 0,
    autopilot_paused INTEGER NOT NULL DEFAULT 0,
    -- Delivery overrides (SYR-H50YW): '' inherits global default.
    delivery_mode TEXT NOT NULL DEFAULT '',
    merge_style   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

-- Per-project repository registry keyed by remote_url (docs/design/work.md).
-- Per-machine checkouts and worktrees are reported by daemons as facts, not stored here.
CREATE TABLE repos (
    id             TEXT PRIMARY KEY,
    sid            TEXT NOT NULL UNIQUE,
    project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    remote_url     TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT '',
    verify_cmd     TEXT NOT NULL DEFAULT '',
    -- Delivery overrides (SYR-H50YW): '' inherits project / global default.
    delivery_mode  TEXT NOT NULL DEFAULT '',
    merge_style    TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    UNIQUE(project_id, remote_url)
);
CREATE INDEX idx_repos_project ON repos(project_id);

-- Work items (docs/design/work.md): ka fields plus Syre runtime fields.
-- Attempts and post-run digests (docs/design/autopilot.md) are added in later tickets.
CREATE TABLE tickets (
    id                 TEXT PRIMARY KEY,
    sid                TEXT NOT NULL UNIQUE,
    project_id         TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    parent_id          TEXT REFERENCES tickets(id) ON DELETE SET NULL,
    kind               TEXT NOT NULL, -- initiative | epic | feature | bug | chore | spike
    title              TEXT NOT NULL,
    body               TEXT NOT NULL DEFAULT '',
    acceptance_json    TEXT NOT NULL DEFAULT '[]',
    labels_json        TEXT NOT NULL DEFAULT '[]',
    priority           TEXT NOT NULL DEFAULT 'P2', -- P0 | P1 | P2 | P3
    status             TEXT NOT NULL DEFAULT 'backlog', -- backlog | ready | running | needs_you | review | done | failed | canceled
    orphan_policy      TEXT NOT NULL DEFAULT 'requeue', -- requeue | fail
    hints_json         TEXT NOT NULL DEFAULT '{}',
    verify_cmd         TEXT NOT NULL DEFAULT '',
    autonomy_json      TEXT NOT NULL DEFAULT '{}',
    attempt            INTEGER NOT NULL DEFAULT 0,
    lease_worker_id    TEXT NOT NULL DEFAULT '',
    lease_task_id      TEXT NOT NULL DEFAULT '',
    lease_expires_at   TEXT,
    lease_heartbeat_at TEXT,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);
CREATE INDEX idx_tickets_project ON tickets(project_id, status, priority, created_at);
CREATE INDEX idx_tickets_parent ON tickets(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX idx_tickets_status ON tickets(status, priority, created_at);
CREATE INDEX idx_tickets_lease_expires ON tickets(lease_expires_at) WHERE lease_expires_at IS NOT NULL;
CREATE INDEX idx_tickets_lease_worker ON tickets(lease_worker_id) WHERE lease_worker_id != '';

-- DAG dependency edges: ticket_id is blocked by blocked_by_id. Cycles are rejected in the store.
CREATE TABLE ticket_edges (
    ticket_id     TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    blocked_by_id TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (ticket_id, blocked_by_id),
    CHECK (ticket_id != blocked_by_id)
) WITHOUT ROWID;
CREATE INDEX idx_ticket_edges_blocker ON ticket_edges(blocked_by_id);

-- Per-ticket bindings to one or more repos in the project's registry.
CREATE TABLE ticket_repo_bindings (
    ticket_id     TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    repo_id       TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    base_branch   TEXT NOT NULL DEFAULT '',
    target_branch TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (ticket_id, repo_id)
) WITHOUT ROWID;
CREATE INDEX idx_ticket_repo_bindings_repo ON ticket_repo_bindings(repo_id);

-- External tracker links (ka, github-projects, jira, linear, etc.).
CREATE TABLE external_refs (
    ticket_id   TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    tracker     TEXT NOT NULL,
    external_id TEXT NOT NULL,
    url         TEXT NOT NULL DEFAULT '',
    version     TEXT NOT NULL DEFAULT '',
    -- synced_at is the ticket's updated_at as of the last pull or push for this ref;
    -- a later updated_at means Syre changed the ticket and it needs pushing.
    synced_at   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (ticket_id, tracker, external_id)
) WITHOUT ROWID;
CREATE INDEX idx_external_refs_lookup ON external_refs(tracker, external_id);

-- External trackers synced into a project's work (docs/design/work.md#tracker-sync).
-- id is also the tracker name stored in external_refs.tracker.
CREATE TABLE trackers (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    config_json  TEXT NOT NULL DEFAULT '{}',
    cursor       TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_pull_at TEXT NOT NULL DEFAULT '',
    last_push_at TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);
CREATE INDEX idx_trackers_project ON trackers(project_id);

-- Short comments on tickets posted by agent sessions or operators.
CREATE TABLE ticket_comments (
    id         TEXT PRIMARY KEY,
    ticket_id  TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    author     TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_ticket_comments_ticket ON ticket_comments(ticket_id, created_at ASC);

CREATE TABLE tasks (
    id                 TEXT PRIMARY KEY,
    sid                TEXT NOT NULL UNIQUE,
    -- external_id is the worker's stable key for a passively discovered session ('' otherwise).
    external_id        TEXT NOT NULL DEFAULT '',
    worker_id          TEXT REFERENCES workers(id),
    worker_pinned      INTEGER NOT NULL DEFAULT 0,
    selector_json      TEXT NOT NULL DEFAULT '',
    project_id         TEXT NOT NULL DEFAULT '00000000-0000-7000-8000-000000000001',
    repo_id            TEXT NOT NULL DEFAULT '',
    origin             TEXT NOT NULL DEFAULT 'commander',
    session_kind       TEXT NOT NULL DEFAULT 'chat',
    agent              TEXT NOT NULL,
    model              TEXT NOT NULL DEFAULT '',
    workdir            TEXT NOT NULL,
    effective_dir      TEXT NOT NULL DEFAULT '',
    branch             TEXT NOT NULL DEFAULT '',
    mode               TEXT NOT NULL DEFAULT 'oneshot',
    permission_mode    TEXT NOT NULL DEFAULT 'full_access',
    orphan_policy      TEXT NOT NULL DEFAULT 'fail',
    prompt             TEXT NOT NULL,
    start_payload_json TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL,
    summary            TEXT NOT NULL DEFAULT '',
    git_summary_json   TEXT NOT NULL DEFAULT '',
    digest_json        TEXT NOT NULL DEFAULT '',
    followups_json     TEXT NOT NULL DEFAULT '',
    worktrees_json     TEXT NOT NULL DEFAULT '',
    worktrees_cleaned_at TEXT,
    exit_code          INTEGER,
    -- cause_json is the store.Cause of the last status transition (who and how sure).
    cause_json         TEXT NOT NULL DEFAULT '',
    -- suspended_by: '' | 'user' | 'pause' (Resume after Pause everything only restores 'pause').
    suspended_by       TEXT NOT NULL DEFAULT '',
    -- parent_chat_id is the commander/agent chat that dispatched the task ('' otherwise).
    parent_chat_id     TEXT NOT NULL DEFAULT '',
    -- title is a short name for the task (dispatch_task title); narration is the
    -- commander's one-line "now" for it (set_narration).
    title              TEXT NOT NULL DEFAULT '',
    narration          TEXT NOT NULL DEFAULT '',
    narration_at       TEXT,
    -- ticket_id / ticket_title: optional link to a Syre or external ticket the task works on.
    ticket_id          TEXT NOT NULL DEFAULT '',
    ticket_title       TEXT NOT NULL DEFAULT '',
    -- keep: the user pinned the task ("Keep"); retention never compacts it.
    -- compacted_at: when retention dropped its verbose events, keeping the milestones
    -- (see store.TaskEventKeepKinds). Cleared when the task runs again.
    keep               INTEGER NOT NULL DEFAULT 0,
    compacted_at       TEXT,
    created_at         TEXT NOT NULL,
    started_at         TEXT,
    completed_at       TEXT
);
CREATE INDEX idx_tasks_status ON tasks(status, created_at DESC);
CREATE INDEX idx_tasks_worker ON tasks(worker_id, status);
CREATE INDEX idx_tasks_created ON tasks(created_at DESC);
CREATE INDEX idx_tasks_worker_created ON tasks(worker_id, created_at DESC);
CREATE INDEX idx_tasks_project ON tasks(project_id, repo_id);
CREATE INDEX idx_tasks_ticket ON tasks(ticket_id) WHERE ticket_id != '';
CREATE UNIQUE INDEX idx_tasks_external ON tasks(worker_id, external_id) WHERE external_id != '';
-- Retention candidates: finished tasks not compacted yet, oldest first.
CREATE INDEX idx_tasks_compact ON tasks(completed_at) WHERE compacted_at IS NULL AND completed_at IS NOT NULL;

CREATE TABLE task_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    message    TEXT NOT NULL,
    meta_json  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_task_events_task ON task_events(task_id, id ASC);

CREATE TABLE chat_sessions (
    id               TEXT PRIMARY KEY,
    sid              TEXT NOT NULL UNIQUE,
    title            TEXT NOT NULL,
    backend          TEXT NOT NULL DEFAULT 'agent',
    provider         TEXT NOT NULL,
    model            TEXT NOT NULL,
    worker_id        TEXT NOT NULL DEFAULT '',
    agent            TEXT NOT NULL DEFAULT '',
    account_id       TEXT NOT NULL DEFAULT '',
    effort           TEXT NOT NULL DEFAULT '',
    approval_mode    TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    agent_session_id TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT '',
    fleet_tools      INTEGER NOT NULL DEFAULT 0,
    last_error_json  TEXT NOT NULL DEFAULT '',
    mcp_token_hash   TEXT NOT NULL DEFAULT '',
    cause_json       TEXT NOT NULL DEFAULT '',
    suspended_by     TEXT NOT NULL DEFAULT '',
    suspended_prev   TEXT NOT NULL DEFAULT '', -- status to restore on resume
    -- notes_cursor: the last fleet_events.id already delivered to this chat as a note.
    notes_cursor     INTEGER NOT NULL DEFAULT 0,
    -- Side-conversation answerers (hidden chats): question_id is the question the chat
    -- answers about; fork_of is the chat whose agent session it forked ('' for the
    -- commander fallback). Hidden chats never appear in the chat list.
    question_id      TEXT NOT NULL DEFAULT '',
    fork_of          TEXT NOT NULL DEFAULT '',
    -- Optional linkage when this chat session works on a task / ticket.
    task_id          TEXT NOT NULL DEFAULT '',
    ticket_id        TEXT NOT NULL DEFAULT '',
    -- Retention (SYR-CSCWP). keep: pinned by the user, never compacted. Retention drops
    -- the bulky events (tool calls, reasoning, plans, usage; see store.ChatEventKeepKinds)
    -- of turns older than the detail period: compacted_seq is the last seq it covered,
    -- compacted_before the age cutoff it used and compacted_at when it last dropped
    -- something. compact_next_at is the created_at of the first event after
    -- compacted_seq (NULL when there is none), so candidates are found by index.
    keep             INTEGER NOT NULL DEFAULT 0,
    compacted_seq    INTEGER NOT NULL DEFAULT 0,
    compacted_before TEXT,
    compacted_at     TEXT,
    compact_next_at  TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX idx_chat_sessions_updated ON chat_sessions(updated_at DESC);
CREATE INDEX idx_chat_sessions_compact ON chat_sessions(compact_next_at) WHERE compact_next_at IS NOT NULL;
CREATE INDEX idx_chat_sessions_worker ON chat_sessions(worker_id, status);

CREATE TABLE chat_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    tool_json  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_chat_messages_session ON chat_messages(session_id, id ASC);

CREATE TABLE chat_events (
    chat_id    TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    turn_id    TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL,
    json       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (chat_id, seq)
);
CREATE INDEX idx_chat_events_turn ON chat_events(chat_id, turn_id);

-- One row per loop iteration of a ticket or ad-hoc task (docs/design/autopilot.md).
-- A self-repair turn or a requeue inserts a new attempt number; it does not overwrite
-- the previous row. ticket_id is NULL for ad-hoc tasks. status '' means still open.
CREATE TABLE attempts (
    id               TEXT PRIMARY KEY,
    ticket_id        TEXT REFERENCES tickets(id) ON DELETE CASCADE,
    task_id          TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    chat_id          TEXT REFERENCES chat_sessions(id) ON DELETE SET NULL,
    attempt          INTEGER NOT NULL,
    worker_id        TEXT NOT NULL DEFAULT '',
    account_id       TEXT NOT NULL DEFAULT '',
    agent            TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',
    queued_at        TEXT,
    started_at       TEXT,
    ended_at         TEXT,
    queue_wait_s     INTEGER,
    duration_s       INTEGER,
    exit_code        INTEGER,
    status           TEXT NOT NULL DEFAULT '',
    failure_category TEXT NOT NULL DEFAULT '',
    verify_cmd       TEXT NOT NULL DEFAULT '',
    verify_exit      INTEGER,
    verify_passed    INTEGER,
    verify_failed    INTEGER,
    tokens_in        INTEGER NOT NULL DEFAULT 0,
    tokens_out       INTEGER NOT NULL DEFAULT 0,
    cost_usd         REAL,
    git_json         TEXT NOT NULL DEFAULT '',
    CHECK (status IN ('', 'review', 'needs_you', 'failed', 'completed', 'canceled'))
);
CREATE INDEX idx_attempts_task ON attempts(task_id, attempt);
CREATE UNIQUE INDEX idx_attempts_ticket_n ON attempts(ticket_id, attempt) WHERE ticket_id IS NOT NULL;
CREATE UNIQUE INDEX idx_attempts_adhoc_n ON attempts(task_id, attempt) WHERE ticket_id IS NULL;

CREATE TABLE agent_accounts (
    worker_id  TEXT NOT NULL,
    account_id TEXT NOT NULL,
    json       TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (worker_id, account_id)
);

-- Machine vitals and heat, in three tiers (SYR-WY5F2; docs/design/activity.md). At most
-- ~2.9k raw + 720 hourly + 365 daily rows per machine, however old it is.
--
-- Raw tier: one row per machine per 15-second bucket (t: unix seconds, a multiple of 15)
-- holding the mean of the n heartbeats in it (memory and swap in MB), the GPU mean over
-- the gpu_n heartbeats that had a reading (NULL when none did), and the mean and max
-- smoothed heat with the cause at the max. n lets a bucket flushed twice (hub restart)
-- merge into a correct mean. Kept 12 hours.
CREATE TABLE machine_vitals (
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    t         INTEGER NOT NULL,
    n         INTEGER NOT NULL,
    cpu       REAL NOT NULL,
    load      REAL NOT NULL,
    mem       REAL NOT NULL,
    mem_total INTEGER NOT NULL,
    swap      REAL NOT NULL,
    gpu_n     INTEGER NOT NULL DEFAULT 0,
    gpu       REAL,
    heat      REAL NOT NULL,
    heat_max  REAL NOT NULL,
    cause     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (worker_id, t)
) WITHOUT ROWID;
CREATE INDEX idx_machine_vitals_t ON machine_vitals(t);

-- Roll-up tiers: min / sample-weighted mean / max per metric over an hour (period 3600,
-- kept 30 days) or a UTC day (period 86400, kept 365 days), t the period start. Hourly
-- rows are recomputed from the raw tier and daily rows from the hourly tier, so rolling
-- up again (late, twice, after a gap) rewrites the same values.
CREATE TABLE machine_vitals_rollup (
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    period    INTEGER NOT NULL,
    t         INTEGER NOT NULL,
    n         INTEGER NOT NULL,
    cpu_min   REAL NOT NULL,
    cpu_avg   REAL NOT NULL,
    cpu_max   REAL NOT NULL,
    load_min  REAL NOT NULL,
    load_avg  REAL NOT NULL,
    load_max  REAL NOT NULL,
    mem_min   REAL NOT NULL,
    mem_avg   REAL NOT NULL,
    mem_max   REAL NOT NULL,
    mem_total INTEGER NOT NULL,
    swap_min  REAL NOT NULL,
    swap_avg  REAL NOT NULL,
    swap_max  REAL NOT NULL,
    gpu_n     INTEGER NOT NULL DEFAULT 0,
    gpu_min   REAL,
    gpu_avg   REAL,
    gpu_max   REAL,
    heat_min  REAL NOT NULL,
    heat_avg  REAL NOT NULL,
    heat_max  REAL NOT NULL,
    cause     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (worker_id, period, t)
) WITHOUT ROWID;
CREATE INDEX idx_machine_vitals_rollup_t ON machine_vitals_rollup(period, t);

-- Each machine's durable outbound spool as the hub applied it (SYR-VZR9F,
-- docs/design/spool.md): applied_seq is the highest daemon seq of epoch the hub applied.
-- Records that append (chat events, task events, usage, vitals) advance it in the same
-- transaction, so a replay after a hub crash never applies them twice.
CREATE TABLE worker_spool (
    worker_id   TEXT PRIMARY KEY REFERENCES workers(id) ON DELETE CASCADE,
    epoch       TEXT NOT NULL DEFAULT '',
    applied_seq INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT NOT NULL
) WITHOUT ROWID;

-- Per-call token usage, kept 30 days (retention_usage_days); older calls are folded into
-- usage_daily.
CREATE TABLE usage_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    account_id    TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL
);
CREATE INDEX idx_usage_events_time ON usage_events(created_at);

-- Daily usage totals per provider/model/account (day: UTC YYYY-MM-DD), rolled up from
-- usage_events by retention and kept a year (retention_usage_daily_days). Each roll-up
-- batch adds its calls here and deletes them from usage_events in one transaction, so a
-- restart never counts a call twice.
CREATE TABLE usage_daily (
    day           TEXT NOT NULL,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    account_id    TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    requests      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, provider, model, account_id)
) WITHOUT ROWID;

CREATE TABLE subscription_quotas (
    id           TEXT PRIMARY KEY, -- UUIDv7; rows are upserted on (provider, window_label)
    provider     TEXT NOT NULL,
    label        TEXT NOT NULL,
    window_label TEXT NOT NULL DEFAULT '',
    used_percent REAL NOT NULL DEFAULT 0,
    reset_at     TEXT,
    source       TEXT NOT NULL DEFAULT 'probe',
    worker_id    TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL,
    UNIQUE(provider, window_label)
);
CREATE INDEX idx_sub_quotas_provider ON subscription_quotas(provider);

CREATE TABLE commander_memory (
    id         TEXT PRIMARY KEY, -- UUIDv7; rows are upserted on (scope, mem_key)
    scope      TEXT NOT NULL, -- 'fleet' | 'session:<id>' | 'project:<id>'
    mem_key    TEXT NOT NULL,
    summary    TEXT NOT NULL,
    meta_json  TEXT NOT NULL DEFAULT '{}',
    updated_at TEXT NOT NULL,
    UNIQUE(scope, mem_key)
);
CREATE INDEX idx_orch_memory_scope ON commander_memory(scope, updated_at DESC);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    actor      TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    worker_id  TEXT NOT NULL DEFAULT '',
    worker_sid TEXT NOT NULL DEFAULT '', -- sids as of the event, for display
    task_id    TEXT NOT NULL DEFAULT '',
    task_sid   TEXT NOT NULL DEFAULT '',
    message    TEXT NOT NULL DEFAULT '',
    meta_json  TEXT NOT NULL DEFAULT '',
    -- actor_kind and cause_json attribute the action (store.Cause).
    actor_kind TEXT NOT NULL DEFAULT '',
    cause_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_log_time ON audit_log(created_at);
CREATE INDEX idx_audit_log_actor ON audit_log(actor, id);
CREATE INDEX idx_audit_log_worker ON audit_log(worker_id, id);
CREATE INDEX idx_audit_log_task ON audit_log(task_id, id);
CREATE INDEX idx_audit_log_action ON audit_log(action, id);

-- Fleet timeline: every task/chat status transition and every intervention, hold and
-- request, with its cause (PRD §5.8a). Feeds "Your actions", recent_events and agent notes.
-- Retention: everything for 90 days, then only human actions and failures for a year
-- (store.FleetEventKeepSQL); events of pinned or unfinished work are kept.
CREATE TABLE fleet_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at   TEXT NOT NULL,
    kind         TEXT NOT NULL, -- status | action | hold | request | veto | notice | directive | pulse
    action       TEXT NOT NULL DEFAULT '',
    actor_kind   TEXT NOT NULL, -- user | commander | agent | scheduler | system | unknown
    actor_source TEXT NOT NULL DEFAULT '',
    actor_id     TEXT NOT NULL DEFAULT '',
    confidence   TEXT NOT NULL DEFAULT '',
    request_id   TEXT NOT NULL DEFAULT '',
    command_id   TEXT NOT NULL DEFAULT '',
    hold_id      TEXT NOT NULL DEFAULT '',
    worker_id    TEXT NOT NULL DEFAULT '',
    task_id      TEXT NOT NULL DEFAULT '',
    chat_id      TEXT NOT NULL DEFAULT '',
    from_status  TEXT NOT NULL DEFAULT '',
    to_status    TEXT NOT NULL DEFAULT '',
    summary      TEXT NOT NULL DEFAULT '',
    meta_json    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_fleet_events_time ON fleet_events(created_at);
CREATE INDEX idx_fleet_events_worker ON fleet_events(worker_id, id);
CREATE INDEX idx_fleet_events_task ON fleet_events(task_id, id);
CREATE INDEX idx_fleet_events_chat ON fleet_events(chat_id, id);
CREATE INDEX idx_fleet_events_actor ON fleet_events(actor_kind, id);

-- Holds: an intervention no agent asked for blocks automatic actions on its scope until
-- the user releases it.
CREATE TABLE holds (
    id            TEXT PRIMARY KEY,
    sid           TEXT NOT NULL UNIQUE, -- h-…
    scope_kind    TEXT NOT NULL, -- session | machine | resource
    worker_id     TEXT NOT NULL DEFAULT '',
    task_id       TEXT NOT NULL DEFAULT '',
    chat_id       TEXT NOT NULL DEFAULT '',
    resource_json TEXT NOT NULL DEFAULT '', -- {"kind":"service","name":"nginx"} / {"kind":"port","port":80}
    summary       TEXT NOT NULL DEFAULT '',
    cause_json    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    released_at   TEXT,
    released_by   TEXT NOT NULL DEFAULT '',
    release_note  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_holds_active ON holds(released_at, worker_id);

-- Open requests: an agent asked the user to do something on a machine.
CREATE TABLE user_requests (
    id            TEXT PRIMARY KEY,
    sid           TEXT NOT NULL UNIQUE, -- rq-…
    chat_id       TEXT NOT NULL DEFAULT '',
    task_id       TEXT NOT NULL DEFAULT '',
    worker_id     TEXT NOT NULL DEFAULT '',
    resource_json TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL,
    status        TEXT NOT NULL, -- open | done | declined | canceled
    created_at    TEXT NOT NULL,
    resolved_at   TEXT,
    resolved_by   TEXT NOT NULL DEFAULT '',
    resolution    TEXT NOT NULL DEFAULT '',
    command_id    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_user_requests_status ON user_requests(status, worker_id);

-- Questions: every question an agent asked the user, fleet-wide, until answered.
CREATE TABLE questions (
    id              TEXT PRIMARY KEY,
    sid             TEXT NOT NULL UNIQUE, -- q-…
    chat_id         TEXT NOT NULL DEFAULT '',
    task_id         TEXT NOT NULL DEFAULT '',
    worker_id       TEXT NOT NULL DEFAULT '',
    agent           TEXT NOT NULL DEFAULT '',
    account_id      TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL, -- question | permission
    source          TEXT NOT NULL, -- ask_user | elicitation
    elicitation_id  TEXT NOT NULL DEFAULT '',
    prompt          TEXT NOT NULL,
    context         TEXT NOT NULL DEFAULT '',
    why_now         TEXT NOT NULL DEFAULT '',
    options_json    TEXT NOT NULL DEFAULT '',
    allow_text      INTEGER NOT NULL DEFAULT 0,
    recommendation  TEXT NOT NULL DEFAULT '', -- the recommended option id
    rationale       TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    blocks_json     TEXT NOT NULL DEFAULT '',
    group_key       TEXT NOT NULL DEFAULT '',
    refs_json       TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL, -- open | answered | delegated | expired | cancelled
    blocked_since   TEXT NOT NULL,
    snoozed_until   TEXT,
    abandoned       INTEGER NOT NULL DEFAULT 0,
    ask_count       INTEGER NOT NULL DEFAULT 1,
    rejections      INTEGER NOT NULL DEFAULT 0,
    answer_json     TEXT NOT NULL DEFAULT '',
    delegation_json TEXT NOT NULL DEFAULT '',
    thread_json     TEXT NOT NULL DEFAULT '',
    -- delivered: 1 once the answer reached the agent (or nothing is left to deliver).
    delivered       INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    closed_at       TEXT
);
CREATE INDEX idx_questions_status ON questions(status, blocked_since);
CREATE INDEX idx_questions_chat ON questions(chat_id, status);

-- Side-conversation messages of a question.
CREATE TABLE question_messages (
    question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    role        TEXT NOT NULL, -- user | answerer | system
    text        TEXT NOT NULL,
    turn_id     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (question_id, seq)
);

-- Under-specified ask_user calls sent back to the agent (question quality metrics).
CREATE TABLE question_rejects (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    TEXT NOT NULL DEFAULT '',
    agent      TEXT NOT NULL DEFAULT '',
    missing    TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_question_rejects_time ON question_rejects(created_at);

-- Reviews and delivery outcomes (SYR-H50YW): the user's review decision on finished work,
-- the delivery outcome (direct merge, PR/MR + merge, open PR/MR, keep branch), PR state
-- tracking and the Expedite flag, one row per session. Expedited queued tasks are placed first.
CREATE TABLE reviews (
    id                TEXT PRIMARY KEY,
    sid               TEXT NOT NULL UNIQUE, -- rv-…
    task_id           TEXT NOT NULL DEFAULT '',
    chat_id           TEXT NOT NULL DEFAULT '',
    decision          TEXT NOT NULL DEFAULT '', -- '' | approved | changes_requested
    note              TEXT NOT NULL DEFAULT '',
    decided_by        TEXT NOT NULL DEFAULT '',
    decided_at        TEXT,
    -- Delivery outcome (SYR-H50YW): mode used, status ('' | delivered | pr_open | auto_merge |
    -- manual | conflict | closed | failed | offline), human message, merge method/SHA, PR info.
    delivery_mode     TEXT NOT NULL DEFAULT '',
    delivery_status   TEXT NOT NULL DEFAULT '',
    delivery_message  TEXT NOT NULL DEFAULT '',
    merge_method      TEXT NOT NULL DEFAULT '',
    merge_sha         TEXT NOT NULL DEFAULT '',
    pr_number         INTEGER NOT NULL DEFAULT 0,
    pr_url            TEXT NOT NULL DEFAULT '',
    pr_state          TEXT NOT NULL DEFAULT '', -- '' | open | merged | closed
    pr_checks         TEXT NOT NULL DEFAULT '', -- '' | none | pending | passed | failed
    pr_auto_merge     INTEGER NOT NULL DEFAULT 0,
    compare_url       TEXT NOT NULL DEFAULT '',
    pr_remote_url     TEXT NOT NULL DEFAULT '',
    pr_last_comment_at TEXT,
    -- follow_up_task_id: the session that received requested changes ('' otherwise).
    follow_up_task_id TEXT NOT NULL DEFAULT '',
    expedite          INTEGER NOT NULL DEFAULT 0,
    expedited_at      TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_reviews_task ON reviews(task_id) WHERE task_id != '';
CREATE UNIQUE INDEX idx_reviews_chat ON reviews(chat_id) WHERE chat_id != '';

-- Persisted deduplication of PR/MR review comments already delivered to the agent.
CREATE TABLE review_seen_comments (
    task_id    TEXT NOT NULL,
    comment_id TEXT NOT NULL,
    seen_at    TEXT NOT NULL,
    PRIMARY KEY (task_id, comment_id)
) WITHOUT ROWID;

-- The Default project (ids.DefaultProjectID, sid ids.DefaultProjectSID) is seeded by
-- EnsureDefaultProject.

-- Override requests (break-glass): a commander asks the user for a time-boxed
-- exception to a slot limit or hold. Approved ones are enforced until expires_at and
-- measured (use_count).
CREATE TABLE override_requests (
    id               TEXT PRIMARY KEY,
    sid              TEXT NOT NULL UNIQUE, -- o-…
    chat_id          TEXT NOT NULL DEFAULT '', -- the requesting commander chat
    scope_kind       TEXT NOT NULL, -- machine_slots | hold | session | machine
    worker_id        TEXT NOT NULL DEFAULT '',
    task_id          TEXT NOT NULL DEFAULT '',
    target_chat_id   TEXT NOT NULL DEFAULT '',
    hold_id          TEXT NOT NULL DEFAULT '',
    extra_slots      INTEGER NOT NULL DEFAULT 0,
    duration_seconds INTEGER NOT NULL,
    reason           TEXT NOT NULL,
    consequences     TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL, -- pending | approved | denied | expired
    created_at       TEXT NOT NULL,
    decided_at       TEXT,
    decided_by       TEXT NOT NULL DEFAULT '',
    decision_note    TEXT NOT NULL DEFAULT '',
    approved_at      TEXT,
    expires_at       TEXT,
    ended_at         TEXT,
    use_count        INTEGER NOT NULL DEFAULT 0,
    first_used_at    TEXT,
    last_used_at     TEXT
);
CREATE INDEX idx_override_requests_status ON override_requests(status, expires_at);

-- Policy directives (docs/design/autopilot.md): typed, timed, attributed policy.
-- Active means ended_at IS NULL and (until IS NULL or until is still ahead).
-- provider.disable is reserved for the next ticket; this table stores it, the API does not.
CREATE TABLE directives (
    id          TEXT PRIMARY KEY,
    sid         TEXT NOT NULL UNIQUE, -- dr-…
    kind        TEXT NOT NULL, -- machine.pause | machine.throttle | machine.drain | autopilot.pause | provider.disable
    target      TEXT NOT NULL, -- worker id (machine.*) or project id (autopilot.pause)
    params_json TEXT NOT NULL DEFAULT '{}',
    until       TEXT, -- expiry; NULL means until someone ends it
    provenance  TEXT NOT NULL, -- user | commander | system
    actor       TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    ended_at    TEXT,
    ended_by    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_directives_active ON directives(target, kind) WHERE ended_at IS NULL;
CREATE INDEX idx_directives_until ON directives(until) WHERE ended_at IS NULL AND until IS NOT NULL;

-- Vetoes awaiting the agent's acknowledgement (its next turn after delivery ending
-- normally); escalated when unacknowledged past the deadline.
CREATE TABLE vetoes (
    id                TEXT PRIMARY KEY,
    sid               TEXT NOT NULL UNIQUE, -- v-…
    reason            TEXT NOT NULL,
    note              TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL DEFAULT '',
    worker_id         TEXT NOT NULL DEFAULT '',
    task_id           TEXT NOT NULL DEFAULT '',
    chat_id           TEXT NOT NULL DEFAULT '',
    fleet_event_id    INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL,
    delivered_at      TEXT,
    delivered_chat_id TEXT NOT NULL DEFAULT '',
    delivered_turn_id TEXT NOT NULL DEFAULT '',
    acked_at          TEXT,
    acked_chat_id     TEXT NOT NULL DEFAULT '',
    escalated_at      TEXT,
    dismissed_at      TEXT,
    dismissed_by      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_vetoes_open ON vetoes(acked_at, dismissed_at);
CREATE INDEX idx_vetoes_event ON vetoes(fleet_event_id);

-- ---------------------------------------------------------------------------------
-- Device sign-in (SYR-W0P8B, SYR-V5JZB, SYR-A29CT). Self-contained block.
-- ---------------------------------------------------------------------------------

-- Devices: one revocable credential per signed-in browser, phone, CLI or MCP client.
-- Only the SHA-256 of the secret is stored. Expiry slides: a credential expires after
-- the configured idle period (settings device_idle_days) without use; expires_at is
-- last_used_at plus that period as of the last use.
CREATE TABLE devices (
    id              TEXT PRIMARY KEY,
    sid             TEXT NOT NULL UNIQUE, -- dv-…
    name            TEXT NOT NULL,
    kind            TEXT NOT NULL, -- browser | phone | cli | mcp
    secret_hash     TEXT NOT NULL UNIQUE, -- hex SHA-256 of the bearer/cookie secret
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL DEFAULT '', -- how it was issued (pairing code sid, recovery key, device)
    last_used_at    TEXT NOT NULL,
    last_ip         TEXT NOT NULL DEFAULT '',
    last_user_agent TEXT NOT NULL DEFAULT '', -- short summary ("Chrome on macOS"), not the raw header
    expires_at      TEXT NOT NULL,
    revoked_at      TEXT
);
CREATE INDEX idx_devices_active ON devices(revoked_at, last_used_at);

-- Pairing codes: single-use, short-lived XXXX-XXXX codes. purpose 'pair' signs in the
-- browser that redeems it (/pair/{code}); purpose 'login' is the CLI device-code flow:
-- a signed-in browser approves it and the CLI, polling with its own secret (poll_hash),
-- receives the new credential. Only hashes are stored.
CREATE TABLE pairing_codes (
    id          TEXT PRIMARY KEY,
    sid         TEXT NOT NULL UNIQUE, -- pc-…
    purpose     TEXT NOT NULL, -- pair | login
    code_hash   TEXT NOT NULL UNIQUE,
    poll_hash   TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '', -- device kind a login code issues (cli | mcp)
    device_name TEXT NOT NULL DEFAULT '', -- the name a login code's device asked for
    created_by  TEXT NOT NULL DEFAULT '',
    created_ip  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    approved_at TEXT,
    approved_by TEXT NOT NULL DEFAULT '',
    denied_at   TEXT,
    redeemed_at TEXT,
    device_id   TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_pairing_codes_poll ON pairing_codes(poll_hash) WHERE poll_hash != '';
CREATE INDEX idx_pairing_codes_expires ON pairing_codes(expires_at);

-- Machine credentials and join codes (SYR-RK9YD). Each syred authenticates with its
-- own credential, bound to its machine (worker_id): it can only register as that
-- machine. Only a SHA-256 of each secret is stored. A credential is minted when a
-- machine joins with a code, or by the hub when it installs syred (local or SSH).
-- Rows stay after revocation for the record; removing the machine deletes them.
CREATE TABLE machine_credentials (
    id             TEXT PRIMARY KEY,
    sid            TEXT NOT NULL UNIQUE, -- mc-…
    worker_id      TEXT NOT NULL,
    secret_hash    TEXT NOT NULL, -- hex SHA-256 of the 32-byte secret
    source         TEXT NOT NULL DEFAULT '', -- join | local | ssh | rotate
    created_at     TEXT NOT NULL,
    last_used_at   TEXT,
    revoked_at     TEXT,
    revoked_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_machine_credentials_worker ON machine_credentials(worker_id, revoked_at);

-- Short-lived, single-use codes a manual machine trades for its credential
-- (`syred join <hub> <code>`). Only a SHA-256 of the code is stored.
CREATE TABLE machine_join_codes (
    id         TEXT PRIMARY KEY,
    code_hash  TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    worker_id  TEXT NOT NULL DEFAULT '' -- the machine that joined with it
);
CREATE INDEX idx_machine_join_codes_expiry ON machine_join_codes(expires_at);
