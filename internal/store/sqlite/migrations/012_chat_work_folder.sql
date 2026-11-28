-- A Commander chat's own workspace folder (SYR-W3SVZ): home-relative, "/" between parts,
-- overriding the fleet workspace folder for work the chat sends to several machines. No
-- row means the fleet default.

CREATE TABLE chat_work_folders (
    chat_id     TEXT PRIMARY KEY REFERENCES chat_sessions(id) ON DELETE CASCADE,
    folder      TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
