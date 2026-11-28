-- Placement rules and the placement log (SYR-880CP). A rule prefers, requires or
-- avoids a machine or a machine class (a prefix of os/arch/RAM tier[/isolation]) for
-- a scope: all work, a project, a repo, a branch or a work key. Rules beat scores but
-- never policy: they act only among the machines the policy admits. Each has a
-- provenance (user, commander, learned), its evidence and an optional expiry.
--
-- The log records every placement: the candidates with their scores, the machines
-- rejected and why, the choice and who made it.

CREATE TABLE placement_rules (
    id           TEXT PRIMARY KEY,
    scope_kind   TEXT NOT NULL,          -- any | project | repo | branch | work_key
    scope_value  TEXT NOT NULL DEFAULT '',
    effect       TEXT NOT NULL,          -- prefer | require | avoid
    target_kind  TEXT NOT NULL,          -- machine | class
    target       TEXT NOT NULL,
    provenance   TEXT NOT NULL,          -- user | commander | learned
    evidence     TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    ended_at     TEXT
);
CREATE INDEX idx_placement_rules_active ON placement_rules(ended_at, expires_at);

CREATE TABLE placement_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,
    subject     TEXT NOT NULL,           -- task:<id> | ticket:<id>
    work_key    TEXT NOT NULL DEFAULT '',
    decider     TEXT NOT NULL,           -- scheduler | commander | user
    worker_id   TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL
);
CREATE INDEX idx_placement_log_subject ON placement_log(subject, at);
CREATE INDEX idx_placement_log_at ON placement_log(at);
