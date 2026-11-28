-- Break glass (SYR-ADQZX): an override can lift named machine policy rules
-- (scope 'policy', rules_json a JSON array of rule names), and every time a rule keeps
-- work off a machine is counted, once per piece of work and hour, for the weekly
-- report of blocks and the overrides that followed.

ALTER TABLE override_requests ADD COLUMN rules_json TEXT NOT NULL DEFAULT '';

CREATE TABLE policy_blocks (
    hour         INTEGER NOT NULL, -- unix seconds of the hour
    worker_id    TEXT NOT NULL,
    rule         TEXT NOT NULL,    -- policy rule (max_sessions, quiet_hours, ...) or directive kind
    n            INTEGER NOT NULL, -- pieces of work blocked that hour
    last_subject TEXT NOT NULL DEFAULT '',
    last_message TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (hour, worker_id, rule)
) WITHOUT ROWID;
