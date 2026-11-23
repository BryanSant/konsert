-- Konsert schema. Pre-1.0 there is exactly one migration: schema changes edit this file and
-- older databases are refused (see migrate.go) rather than upgraded.
--
-- IDs: user-facing records use typed ka-style IDs minted by internal/ids (task-7qk3m,
-- chat-w47xk, WS-…, PRJ-…, REPO-…, WT-…, SPC-…); workers use their self-minted UUIDv7.

CREATE TABLE workers (
    id               TEXT PRIMARY KEY,
    machine_id       TEXT NOT NULL DEFAULT '',
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
    last_seen_at     TEXT NOT NULL
);
CREATE INDEX idx_workers_machine_id ON workers(machine_id);

CREATE TABLE workspaces (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    is_default INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE projects (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    is_default   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL
);
CREATE INDEX idx_projects_workspace ON projects(workspace_id);

CREATE TABLE repos (
    id         TEXT PRIMARY KEY,
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
    repo_id    TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch     TEXT NOT NULL DEFAULT '',
    path       TEXT NOT NULL,
    is_primary INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_worktrees_repo ON worktrees(repo_id);

CREATE TABLE spaces (
    id         TEXT PRIMARY KEY,
    worker_id  TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    path       TEXT NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX idx_spaces_worker ON spaces(worker_id, created_at DESC);

CREATE TABLE tasks (
    id                 TEXT PRIMARY KEY,
    -- external_id is the worker's stable key for a passively discovered session ('' otherwise).
    external_id        TEXT NOT NULL DEFAULT '',
    worker_id          TEXT REFERENCES workers(id),
    worker_pinned      INTEGER NOT NULL DEFAULT 0,
    selector_json      TEXT NOT NULL DEFAULT '',
    workspace_id       TEXT NOT NULL DEFAULT 'ws-00000',
    project_id         TEXT NOT NULL DEFAULT 'prj-00000',
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
    id           TEXT PRIMARY KEY,
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
    id         TEXT PRIMARY KEY,
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
    task_id    TEXT NOT NULL DEFAULT '',
    message    TEXT NOT NULL DEFAULT '',
    meta_json  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_log_time ON audit_log(created_at);
CREATE INDEX idx_audit_log_actor ON audit_log(actor, id);
CREATE INDEX idx_audit_log_worker ON audit_log(worker_id, id);
CREATE INDEX idx_audit_log_task ON audit_log(task_id, id);
CREATE INDEX idx_audit_log_action ON audit_log(action, id);

-- The Default workspace and project (ids.DefaultWorkspace/DefaultProject) are seeded by
-- EnsureTaxonomyDefaults.
