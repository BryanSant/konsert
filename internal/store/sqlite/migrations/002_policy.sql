-- Provider-disable substitution notes, and Commander set_policy proposals
-- held for the user when commander_policy_writes is confirm.

ALTER TABLE attempts ADD COLUMN substitution TEXT NOT NULL DEFAULT '';

CREATE TABLE policy_proposals (
    id          TEXT PRIMARY KEY,
    sid         TEXT NOT NULL UNIQUE,
    chat_id     TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    target      TEXT NOT NULL,
    until       TEXT,
    params_json TEXT NOT NULL DEFAULT '{}',
    reason      TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending',
    actor_id    TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    decided_at  TEXT,
    decided_by  TEXT NOT NULL DEFAULT '',
    applied_id  TEXT NOT NULL DEFAULT '',
    CHECK (status IN ('pending', 'approved', 'denied'))
);

CREATE INDEX idx_policy_proposals_status ON policy_proposals(status, created_at);
