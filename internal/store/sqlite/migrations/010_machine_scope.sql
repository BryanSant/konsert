-- Operator labels, machine groups and Commander chat scope (SYR-FJA2T).
-- Labels are the user's own tags for a machine ("Work", "Gaming hardware"), kept on the hub
-- so they survive reconnects and need no change on the machine; they sit beside the
-- daemon's config tags and are never merged into them. A group is a saved set of machines.
-- A chat's scope limits where the Commander may send work; no row means all machines.
-- Deleting a machine drops its labels and its group memberships.

CREATE TABLE machine_labels (
    worker_id   TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    label       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (worker_id, label)
);
CREATE INDEX idx_machine_labels_label ON machine_labels(label);

CREATE TABLE machine_groups (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE TABLE machine_group_members (
    group_id    TEXT NOT NULL REFERENCES machine_groups(id) ON DELETE CASCADE,
    worker_id   TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, worker_id)
);
CREATE INDEX idx_machine_group_members_worker ON machine_group_members(worker_id);

CREATE TABLE chat_scopes (
    chat_id     TEXT PRIMARY KEY REFERENCES chat_sessions(id) ON DELETE CASCADE,
    scope_json  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
