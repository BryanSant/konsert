-- 012: agent chat on subscriptions (KON-TY1XP, docs/design/agent-chat.md).
--
-- Chat sessions gain the agent backend: which worker/agent/account hosts the chat,
-- its model/effort/approval mode, the CLI's own session id (for resume), the chat
-- status pill and the last classified error. backend 'api' is the optional API-key
-- orchestrator; 'agent' (the default) drives a logged-in agent CLI on a worker.
ALTER TABLE chat_sessions ADD COLUMN backend TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE chat_sessions ADD COLUMN worker_id TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN agent TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN effort TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN approval_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN cwd TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN agent_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN status TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN fleet_tools INTEGER NOT NULL DEFAULT 0;
ALTER TABLE chat_sessions ADD COLUMN last_error_json TEXT NOT NULL DEFAULT '';
-- SHA-256 (hex) of the per-chat MCP bearer token; the token itself is never stored.
ALTER TABLE chat_sessions ADD COLUMN mcp_token_hash TEXT NOT NULL DEFAULT '';

-- Sessions that existed before this migration were API-key orchestrator chats.
UPDATE chat_sessions SET backend = 'api';

CREATE INDEX IF NOT EXISTS idx_chat_sessions_worker ON chat_sessions(worker_id, status);

-- Append-only normalized agent chat events (protocol.AgentChatEvent JSON). seq is
-- assigned by the hub per chat (see controller/hub_agentchat.go). Streamed deltas
-- are coalesced when their turn ends, so only complete messages stay stored.
CREATE TABLE IF NOT EXISTS chat_events (
    chat_id    TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    turn_id    TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL,
    json       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (chat_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_chat_events_chat_seq ON chat_events(chat_id, seq);
CREATE INDEX IF NOT EXISTS idx_chat_events_turn ON chat_events(chat_id, turn_id);

-- Agent accounts (protocol.AgentAccount JSON) reported by each worker in
-- agent.capabilities. No foreign key: rows are removed with their worker by the hub.
CREATE TABLE IF NOT EXISTS agent_accounts (
    worker_id  TEXT NOT NULL,
    account_id TEXT NOT NULL,
    json       TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (worker_id, account_id)
);
