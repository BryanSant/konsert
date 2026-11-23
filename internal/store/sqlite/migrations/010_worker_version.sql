-- konsertd build version reported in worker.hello (WorkerInfo.Version), so the
-- fleet view and `konsert status` can flag hub/worker version mismatches
-- (KON-B56RC). Empty means unknown.
ALTER TABLE workers ADD COLUMN version TEXT NOT NULL DEFAULT '';
