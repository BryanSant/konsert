-- Web Push subscriptions (SYR-Z15J6). One per browser or installed PWA, tied to the signed-in
-- device that made it: revoking the device drops its subscriptions. prefs_json holds the
-- device's notification choices (categories, quiet hours). A subscription the push service
-- answers 404/410 for is gone and is deleted.

CREATE TABLE push_subscriptions (
    endpoint        TEXT PRIMARY KEY,
    device_id       TEXT NOT NULL,
    p256dh          TEXT NOT NULL,
    auth            TEXT NOT NULL,
    prefs_json      TEXT NOT NULL DEFAULT '{}',
    user_agent      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    last_sent_at    TEXT,
    last_error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_push_subscriptions_device ON push_subscriptions(device_id);
