-- Work profiles (SYR-VYFYX). Each ended attempt is stamped with its work key (the same
-- kind of work: project / epic or ticket / repo), the machine class it ran on (os /
-- arch / RAM tier [/ isolation]) and its resource use from the metric history (peak
-- memory, CPU-seconds). work_profiles is recomputed from the recent attempts of a
-- (work key, machine class) each time one of them ends.

ALTER TABLE attempts ADD COLUMN work_key TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN machine_class TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN peak_rss_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN cpu_s REAL NOT NULL DEFAULT 0;
CREATE INDEX idx_attempts_work_key ON attempts(work_key, machine_class, ended_at) WHERE work_key != '';

CREATE TABLE work_profiles (
    work_key        TEXT NOT NULL,
    machine_class   TEXT NOT NULL,
    profile_json    TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (work_key, machine_class)
) WITHOUT ROWID;
