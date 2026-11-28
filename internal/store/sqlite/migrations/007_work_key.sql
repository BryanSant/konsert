-- Work keys and declared requirements on tasks (SYR-2BE8M). Each task records its work
-- key (project / epic or ticket / repo [/ kind], or the dispatcher's own) and kind at
-- dispatch, so history, profiles and placement attach to it from the start. Requirements
-- a dispatcher declares for a work key are remembered: later work of the same key
-- inherits them when it declares none.

ALTER TABLE tasks ADD COLUMN work_key TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN work_kind TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_tasks_work_key ON tasks(work_key) WHERE work_key != '';

CREATE TABLE work_requirements (
    work_key          TEXT PRIMARY KEY,
    requirements_json TEXT NOT NULL,
    set_by            TEXT NOT NULL DEFAULT '',
    updated_at        TEXT NOT NULL
) WITHOUT ROWID;
