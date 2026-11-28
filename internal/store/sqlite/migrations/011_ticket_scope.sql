-- A ticket's "Run on" (SYR-0M4QQ): where its work may be placed, as a MachineTarget like a
-- Commander chat's scope. No row means all machines.

CREATE TABLE ticket_scopes (
    ticket_id   TEXT PRIMARY KEY REFERENCES tickets(id) ON DELETE CASCADE,
    scope_json  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
