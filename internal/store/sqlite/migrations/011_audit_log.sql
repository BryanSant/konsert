-- 011: fleet-wide operator audit log (KON-WP2A5). Control actions are recorded
-- here with who/what/when/which host/which task, including actions that are not
-- tied to a task (worker removal, settings changes, host wake locks). There are
-- no foreign keys on purpose: audit rows outlive the workers and tasks they name.
-- Rows are pruned by age and count (see SQLiteStore.PruneAudit).
CREATE TABLE IF NOT EXISTS audit_log (
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

CREATE INDEX IF NOT EXISTS idx_audit_log_time ON audit_log(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_log_actor ON audit_log(actor, id);
CREATE INDEX IF NOT EXISTS idx_audit_log_worker ON audit_log(worker_id, id);
CREATE INDEX IF NOT EXISTS idx_audit_log_task ON audit_log(task_id, id);
CREATE INDEX IF NOT EXISTS idx_audit_log_action ON audit_log(action, id);

-- Backfill the per-task control events recorded before this table existed. Their
-- actor was never stored, so it is "unknown".
INSERT INTO audit_log (created_at, actor, source, action, worker_id, task_id, message)
SELECT e.created_at, 'unknown', 'legacy',
       CASE
           WHEN e.message LIKE 'Steer%' THEN 'task.steer'
           WHEN e.message LIKE 'Cancel%' THEN 'task.cancel'
           WHEN e.message LIKE 'Git commit%' THEN 'git.commit'
           ELSE 'task.control'
       END,
       COALESCE(t.worker_id, ''), e.task_id, e.message
FROM task_events e
LEFT JOIN tasks t ON t.id = e.task_id
WHERE e.kind = 'control'
ORDER BY e.id;
