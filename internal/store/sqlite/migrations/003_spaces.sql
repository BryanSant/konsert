CREATE TABLE IF NOT EXISTS spaces (
    id         TEXT PRIMARY KEY,
    worker_id  TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    path       TEXT NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_spaces_worker ON spaces(worker_id, created_at DESC);

ALTER TABLE tasks ADD COLUMN space_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_tasks_space ON tasks(space_id, created_at DESC);
