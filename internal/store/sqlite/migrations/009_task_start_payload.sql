-- Persist each task's worker selector and full task.start payload so queued and
-- requeued tasks keep their placement constraints and launch options across
-- controller restarts (KON-4SBC7). worker_pinned records whether the worker was
-- chosen explicitly (worker_id selector or repo/space binding) rather than by
-- the scheduler, so requeue only keeps the worker when it was pinned.
ALTER TABLE tasks ADD COLUMN selector_json TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN start_payload_json TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN worker_pinned INTEGER NOT NULL DEFAULT 0;

-- Stable per-installation machine UUID reported by konsertd (WorkerInfo.MachineID).
-- The hub matches on it before falling back to the legacy name/host tuple, so two
-- hosts that share a hostname coexist as separate workers (KON-FWDRN).
ALTER TABLE workers ADD COLUMN machine_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_workers_machine_id ON workers(machine_id);
