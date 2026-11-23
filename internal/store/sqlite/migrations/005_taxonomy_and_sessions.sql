-- 4-level workspace taxonomy: Space (workspaces) > Project > Repo > Worktree
-- plus unified session metadata (model, session_kind, taxonomy bindings).

CREATE TABLE IF NOT EXISTS workspaces (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    is_default  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    is_default   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_projects_workspace ON projects(workspace_id);

CREATE TABLE IF NOT EXISTS repos (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    worker_id   TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    path        TEXT NOT NULL,
    remote_url  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_repos_project ON repos(project_id);
CREATE INDEX IF NOT EXISTS idx_repos_worker ON repos(worker_id);

CREATE TABLE IF NOT EXISTS worktrees (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch      TEXT NOT NULL DEFAULT '',
    path        TEXT NOT NULL,
    is_primary  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_worktrees_repo ON worktrees(repo_id);

-- Built-in Default Space and Default Project (never orphaned).
INSERT OR IGNORE INTO workspaces (id, name, is_default, created_at)
VALUES ('space-default', 'Default', 1, '1970-01-01T00:00:00Z');

INSERT OR IGNORE INTO projects (id, workspace_id, name, is_default, created_at)
VALUES ('project-default', 'space-default', 'Default', 1, '1970-01-01T00:00:00Z');

-- Migrate any legacy per-machine spaces into repos + primary worktrees under Default Project.
INSERT OR IGNORE INTO repos (id, project_id, worker_id, name, path, remote_url, created_at)
SELECT id, 'project-default', worker_id, name, path, '', created_at
FROM spaces;

INSERT OR IGNORE INTO worktrees (id, repo_id, branch, path, is_primary, created_at)
SELECT 'wt-' || id, id, branch, path, 1, created_at
FROM spaces;

-- Add unified session & taxonomy columns to tasks.
ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN session_kind TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE tasks ADD COLUMN workspace_id TEXT NOT NULL DEFAULT 'space-default';
ALTER TABLE tasks ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project-default';
ALTER TABLE tasks ADD COLUMN repo_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN worktree_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_tasks_taxonomy ON tasks(workspace_id, project_id, repo_id, worktree_id);
