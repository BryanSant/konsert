-- Subscription-first quota windows (rolling 5h/8h/weekly %) for the Orca-style footer.

CREATE TABLE IF NOT EXISTS subscription_quotas (
    id           TEXT PRIMARY KEY,
    provider     TEXT NOT NULL,
    label        TEXT NOT NULL,
    window_label TEXT NOT NULL DEFAULT '',
    used_percent REAL NOT NULL DEFAULT 0,
    reset_at     TEXT,
    source       TEXT NOT NULL DEFAULT 'probe',
    worker_id    TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL,
    UNIQUE(provider, window_label)
);

CREATE INDEX IF NOT EXISTS idx_sub_quotas_provider ON subscription_quotas(provider);
