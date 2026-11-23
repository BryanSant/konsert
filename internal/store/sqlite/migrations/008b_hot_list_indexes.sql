-- 008b: indexes for hot list queries so they can walk an index in order
-- instead of scanning and sorting the whole table (temp B-tree).
-- Named 008b so it sorts after 008_ and before 009_ under sort.Strings.

-- ListTasks with no filter: ORDER BY created_at DESC LIMIT ?
CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at DESC);

-- ListTasks filtered by worker: WHERE worker_id = ? ORDER BY created_at DESC LIMIT ?
CREATE INDEX IF NOT EXISTS idx_tasks_worker_created ON tasks(worker_id, created_at DESC);

-- ListChatSessions: ORDER BY updated_at DESC LIMIT ?
CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated ON chat_sessions(updated_at DESC);
