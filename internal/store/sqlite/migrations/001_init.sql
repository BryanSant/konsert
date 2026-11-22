CREATE TABLE IF NOT EXISTS workers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    host_group      TEXT NOT NULL,
    os              TEXT NOT NULL,
    arch            TEXT NOT NULL,
    isolation       TEXT NOT NULL DEFAULT 'host',
    permission_mode TEXT NOT NULL DEFAULT 'full_access',
    agents_json     TEXT NOT NULL,
    tags_json       TEXT NOT NULL,
    max_concurrent  INTEGER NOT NULL DEFAULT 2,
    status          TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id               TEXT PRIMARY KEY,
    worker_id        TEXT REFERENCES workers(id),
    agent            TEXT NOT NULL,
    workdir          TEXT NOT NULL,
    effective_dir    TEXT NOT NULL DEFAULT '',
    branch           TEXT NOT NULL DEFAULT '',
    mode             TEXT NOT NULL DEFAULT 'oneshot',
    permission_mode  TEXT NOT NULL DEFAULT 'full_access',
    orphan_policy    TEXT NOT NULL DEFAULT 'fail',
    prompt           TEXT NOT NULL,
    status           TEXT NOT NULL,
    summary          TEXT NOT NULL DEFAULT '',
    git_summary_json TEXT NOT NULL DEFAULT '',
    exit_code        INTEGER,
    created_at       TEXT NOT NULL,
    started_at       TEXT,
    completed_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_worker ON tasks(worker_id, status);

CREATE TABLE IF NOT EXISTS task_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    message    TEXT NOT NULL,
    meta_json  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, id ASC);

CREATE TABLE IF NOT EXISTS chat_sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    tool_json  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id ASC);
