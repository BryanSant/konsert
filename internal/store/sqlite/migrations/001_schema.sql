-- Konsert schema. Pre-1.0 there is exactly one migration: schema changes edit this file and
-- older databases are refused (see migrate.go) rather than upgraded.
--
-- IDs: every entity table has two identifiers.
--   id  UUIDv7 primary key. Every foreign key column holds one, as do the hub<->worker
--       protocol and the web app's internal state. workers.id is konsertd's own
--       persistent UUID.
--   sid short typed ID from internal/ids, unique, shown to users and accepted in API
--       paths next to the id: t-7qk3m task, c-w47xk chat, m-… machine, p-… project,
--       r-… repo, s-… space, ws-… workspace, wt-… worktree,
--       h-… hold, rq-… user request, q-… question, o-… override request, v-… veto.
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
    -- How the hub (re)starts this machine's konsertd: '' started by hand / unknown,
    -- 'local' the hub's own host (konsertd install), 'ssh' konsertd install over SSH to
    -- ssh_target. controller_url is the hub URL the daemon was installed with.
    launch           TEXT NOT NULL DEFAULT '',
    ssh_target       TEXT NOT NULL DEFAULT '',
    controller_url   TEXT NOT NULL DEFAULT '',
    -- Last start attempt: '' (none pending / connected), 'starting', 'failed'.
    launch_state     TEXT NOT NULL DEFAULT '',
    launch_error     TEXT NOT NULL DEFAULT '',
    launch_at        TEXT,
    -- Drained machines finish their current work and receive nothing new.
    drained          INTEGER NOT NULL DEFAULT 0,
    drained_at       TEXT
);

CREATE TABLE workspaces (
    id         TEXT PRIMARY KEY,
    sid        TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    is_default INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE projects (
    id           TEXT PRIMARY KEY,
    sid          TEXT NOT NULL UNIQUE,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    is_default   INTEGER NOT NULL DEFAULT 0,
    -- headline is the project's current goal, set by the orchestrator (set_narration).
    headline     TEXT NOT NULL DEFAULT '',
    headline_at  TEXT,
    created_at   TEXT NOT NULL
);
CREATE INDEX idx_projects_workspace ON projects(workspace_id);

CREATE TABLE repos (
    id         TEXT PRIMARY KEY,
    sid        TEXT NOT NULL UNIQUE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    worker_id  TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    path       TEXT NOT NULL,
    remote_url TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_repos_project ON repos(project_id);
CREATE INDEX idx_repos_worker ON repos(worker_id);

CREATE TABLE worktrees (
    id         TEXT PRIMARY KEY,
    sid        TEXT NOT NULL UNIQUE,
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch     TEXT NOT NULL DEFAULT '',
    path       TEXT NOT NULL,
    is_primary INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_worktrees_repo ON worktrees(repo_id);

CREATE TABLE spaces (
    id         TEXT PRIMARY KEY,
    sid        TEXT NOT NULL UNIQUE,
    worker_id  TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    path       TEXT NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_spaces_worker ON spaces(worker_id, created_at DESC);

CREATE TABLE tasks (
    id                 TEXT PRIMARY KEY,
    sid                TEXT NOT NULL UNIQUE,
    -- external_id is the worker's stable key for a passively discovered session ('' otherwise).
    external_id        TEXT NOT NULL DEFAULT '',
    worker_id          TEXT REFERENCES workers(id),
    worker_pinned      INTEGER NOT NULL DEFAULT 0,
    selector_json      TEXT NOT NULL DEFAULT '',
    workspace_id       TEXT NOT NULL DEFAULT '00000000-0000-7000-8000-000000000000',
    project_id         TEXT NOT NULL DEFAULT '00000000-0000-7000-8000-000000000001',
    repo_id            TEXT NOT NULL DEFAULT '',
    worktree_id        TEXT NOT NULL DEFAULT '',
    space_id           TEXT NOT NULL DEFAULT '',
    origin             TEXT NOT NULL DEFAULT 'orchestrator',
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
    exit_code          INTEGER,
    -- cause_json is the store.Cause of the last status transition (who and how sure).
    cause_json         TEXT NOT NULL DEFAULT '',
    -- suspended_by: '' | 'user' | 'pause' (Resume after Pause everything only restores 'pause').
    suspended_by       TEXT NOT NULL DEFAULT '',
    -- parent_chat_id is the orchestrator/agent chat that dispatched the task ('' otherwise).
    parent_chat_id     TEXT NOT NULL DEFAULT '',
    -- title is a short name for the task (dispatch_task title); narration is the
    -- orchestrator's one-line "now" for it (set_narration).
    title              TEXT NOT NULL DEFAULT '',
    narration          TEXT NOT NULL DEFAULT '',
    narration_at       TEXT,
    -- ticket_id / ticket_title: the ka ticket the task works on (detected in its title or
    -- prompt; the title is resolved with ka in the background).
    ticket_id          TEXT NOT NULL DEFAULT '',
    ticket_title       TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL,
    started_at         TEXT,
    completed_at       TEXT
);
CREATE INDEX idx_tasks_status ON tasks(status, created_at DESC);
CREATE INDEX idx_tasks_worker ON tasks(worker_id, status);
CREATE INDEX idx_tasks_created ON tasks(created_at DESC);
CREATE INDEX idx_tasks_worker_created ON tasks(worker_id, created_at DESC);
CREATE INDEX idx_tasks_space ON tasks(space_id, created_at DESC);
CREATE INDEX idx_tasks_taxonomy ON tasks(workspace_id, project_id, repo_id, worktree_id);
CREATE UNIQUE INDEX idx_tasks_external ON tasks(worker_id, external_id) WHERE external_id != '';

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
    -- orchestrator fallback). Hidden chats never appear in the chat list.
    question_id      TEXT NOT NULL DEFAULT '',
    fork_of          TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX idx_chat_sessions_updated ON chat_sessions(updated_at DESC);
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

CREATE TABLE agent_accounts (
    worker_id  TEXT NOT NULL,
    account_id TEXT NOT NULL,
    json       TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (worker_id, account_id)
);

-- Machine heat history: one row per worker per 1-minute bucket (bucket_start is unix
-- seconds, a multiple of 60) with the max and mean smoothed heat score in that minute and
-- the dominant cause at the max. samples counts the heartbeats folded in, so a minute
-- flushed twice (hub restart, reconnect) merges into a correct mean. Kept 8 days.
CREATE TABLE worker_heat_samples (
    worker_id    TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    bucket_start INTEGER NOT NULL,
    score_max    REAL NOT NULL,
    score_avg    REAL NOT NULL,
    samples      INTEGER NOT NULL DEFAULT 1,
    cause        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (worker_id, bucket_start)
) WITHOUT ROWID;
CREATE INDEX idx_worker_heat_samples_bucket ON worker_heat_samples(bucket_start);

CREATE TABLE usage_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL
);
CREATE INDEX idx_usage_events_provider ON usage_events(provider, model);

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

CREATE TABLE orchestrator_memory (
    id         TEXT PRIMARY KEY, -- UUIDv7; rows are upserted on (scope, mem_key)
    scope      TEXT NOT NULL, -- 'fleet' | 'session:<id>' | 'project:<id>'
    mem_key    TEXT NOT NULL,
    summary    TEXT NOT NULL,
    meta_json  TEXT NOT NULL DEFAULT '{}',
    updated_at TEXT NOT NULL,
    UNIQUE(scope, mem_key)
);
CREATE INDEX idx_orch_memory_scope ON orchestrator_memory(scope, updated_at DESC);

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
CREATE TABLE fleet_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at   TEXT NOT NULL,
    kind         TEXT NOT NULL, -- status | action | hold | request | veto | notice
    action       TEXT NOT NULL DEFAULT '',
    actor_kind   TEXT NOT NULL, -- user | orchestrator | agent | scheduler | system | unknown
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

-- Reviews (KON-BM8JR): the user's review decision on a finished task and the Expedite
-- flag of a task or chat, one row per session. Expedited queued tasks are placed first.
CREATE TABLE reviews (
    id             TEXT PRIMARY KEY,
    sid            TEXT NOT NULL UNIQUE, -- rv-…
    task_id        TEXT NOT NULL DEFAULT '',
    chat_id        TEXT NOT NULL DEFAULT '',
    decision       TEXT NOT NULL DEFAULT '', -- '' | approved | changes_requested
    note           TEXT NOT NULL DEFAULT '',
    decided_by     TEXT NOT NULL DEFAULT '',
    decided_at     TEXT,
    -- github_*: what happened to the decision on the task's pull request (gh):
    -- '' | sent | failed | no_github | no_gh | no_pr | offline, gh's message, the PR URL.
    github_status  TEXT NOT NULL DEFAULT '',
    github_message TEXT NOT NULL DEFAULT '',
    github_url     TEXT NOT NULL DEFAULT '',
    -- follow_up_task_id: the session that received requested changes ('' otherwise).
    follow_up_task_id TEXT NOT NULL DEFAULT '',
    expedite       INTEGER NOT NULL DEFAULT 0,
    expedited_at   TEXT,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_reviews_task ON reviews(task_id) WHERE task_id != '';
CREATE UNIQUE INDEX idx_reviews_chat ON reviews(chat_id) WHERE chat_id != '';

-- The Default workspace and project (ids.DefaultWorkspaceID/DefaultProjectID, sids
-- ids.DefaultWorkspaceSID/DefaultProjectSID) are seeded by EnsureTaxonomyDefaults.

-- Override requests (break-glass): an orchestrator asks the user for a time-boxed
-- exception to a slot limit or hold. Approved ones are enforced until expires_at and
-- measured (use_count).
CREATE TABLE override_requests (
    id               TEXT PRIMARY KEY,
    sid              TEXT NOT NULL UNIQUE, -- o-…
    chat_id          TEXT NOT NULL DEFAULT '', -- the requesting orchestrator chat
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
-- Device sign-in (KON-W0P8B, KON-V5JZB, KON-A29CT). Self-contained block.
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

-- Machine credentials and join codes (KON-RK9YD). Each konsertd authenticates with its
-- own credential, bound to its machine (worker_id): it can only register as that
-- machine. Only a SHA-256 of each secret is stored. A credential is minted when a
-- machine joins with a code, or by the hub when it installs konsertd (local or SSH).
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
-- (`konsertd join <hub> <code>`). Only a SHA-256 of the code is stored.
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
