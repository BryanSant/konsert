-- Metric history (SYR-PEEBE): per-session and agent/other load series from the
-- daemons' process attribution, in 1-minute rows (period 60) rolled up into hourly
-- rows (period 3600). Subjects: task:<id>, chat:<id>, observed:<worker>/<external id>,
-- agent:<worker>, other:<worker>. Averages are sample-weighted (n heartbeats).

CREATE TABLE metric_samples (
    subject   TEXT NOT NULL,
    period    INTEGER NOT NULL,
    t         INTEGER NOT NULL,
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    n         INTEGER NOT NULL,
    cpu_avg   REAL NOT NULL,
    cpu_max   REAL NOT NULL,
    rss_avg   REAL NOT NULL,
    rss_max   REAL NOT NULL,
    procs_max INTEGER NOT NULL,
    PRIMARY KEY (subject, period, t)
) WITHOUT ROWID;
CREATE INDEX idx_metric_samples_period_t ON metric_samples(period, t);
CREATE INDEX idx_metric_samples_worker ON metric_samples(worker_id, period, t);

-- The highest memory and CPU a task's processes reached (0: never sampled).
ALTER TABLE tasks ADD COLUMN peak_rss_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN peak_cpu_pct REAL NOT NULL DEFAULT 0;
