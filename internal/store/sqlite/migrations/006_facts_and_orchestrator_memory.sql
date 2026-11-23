-- Cached machine facts on workers + persistent Orchestrator fleet/thread memory.

ALTER TABLE workers ADD COLUMN facts_json TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN facts_updated_at TEXT;

CREATE TABLE IF NOT EXISTS orchestrator_memory (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL, -- 'fleet' | 'session:<id>' | 'project:<id>'
    mem_key     TEXT NOT NULL,
    summary     TEXT NOT NULL,
    meta_json   TEXT NOT NULL DEFAULT '{}',
    updated_at  TEXT NOT NULL,
    UNIQUE(scope, mem_key)
);

CREATE INDEX IF NOT EXISTS idx_orch_memory_scope ON orchestrator_memory(scope, updated_at DESC);
