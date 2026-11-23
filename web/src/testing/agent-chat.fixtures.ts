/**
 * Fixtures shaped exactly like the Go protocol JSON (internal/protocol/agentchat.go):
 * snake_case tags, RFC 3339 times, omitempty fields left out. Used by specs and the
 * dev-only visual preview.
 */
import { AgentAccount, AgentChatEvent, WorkerAgentAccount } from '../app/core/models/agent-chat.models';

export const FIXTURE_WORKER_ID = 'w-bocamacmini';
export const FIXTURE_CHAT_ID = 'chat-1';

const CLAUDE_EFFORTS = [
  { id: 'low', label: 'Low', short: 'Low' },
  { id: 'medium', label: 'Medium', short: 'Med' },
  { id: 'high', label: 'High', short: 'High' },
  { id: 'xhigh', label: 'Extra high', short: 'XHigh' },
  { id: 'max', label: 'Max', short: 'Max' },
];

const CLAUDE_APPROVALS = [
  { id: 'plan', label: 'Plan only', description: 'Reads and plans, never edits or runs commands', risk: 0, icon: 'eye' },
  {
    id: 'default',
    label: 'Ask for approval',
    description: "Asks before edits and commands your settings don't allow",
    risk: 1,
    icon: 'hand',
  },
  { id: 'acceptEdits', label: 'Accept edits', description: 'Edits files without asking, asks before commands', risk: 2, icon: 'edit' },
  { id: 'auto', label: 'Approve for me', description: 'Reviews approval requests for you', risk: 3, icon: 'shield-check' },
  {
    id: 'bypassPermissions',
    label: 'Full access',
    description: 'Never asks before edits or commands',
    risk: 4,
    icon: 'shield-alert',
  },
];

export function claudeAccount(over: Partial<AgentAccount> = {}): AgentAccount {
  return {
    id: 'claude',
    agent: 'claude',
    label: 'Claude',
    is_default: true,
    version: '2.3.1',
    auth: { state: 'ok', method: 'oauth', email: 'me@example.com' },
    plan: 'max',
    models: [
      {
        id: 'opus',
        display_name: 'Opus 5',
        description: 'Most capable for complex work',
        efforts: CLAUDE_EFFORTS,
        default_effort: 'high',
        context_tokens: 1000000,
      },
      {
        id: 'sonnet',
        display_name: 'Sonnet 5.5',
        description: 'Best for everyday tasks',
        is_default: true,
        efforts: CLAUDE_EFFORTS.slice(0, 4),
        default_effort: 'medium',
      },
      { id: 'haiku', display_name: 'Haiku 5', description: 'Fastest for quick answers' },
    ],
    models_source: 'discovered',
    default_model: 'sonnet',
    approval_modes: CLAUDE_APPROVALS,
    default_approval_mode: 'default',
    usage: [
      { window: 'five_hour', used_percent: 42, resets_at: '2026-11-23T22:40:00Z', state: 'ok' },
      { window: 'seven_day', used_percent: 18, resets_at: '2026-11-28T09:00:00Z', state: 'ok' },
    ],
    checked_at: '2026-11-23T20:00:00Z',
    chat_supported: true,
    ...over,
  };
}

export function claude2Account(over: Partial<AgentAccount> = {}): AgentAccount {
  return claudeAccount({
    id: 'claude2',
    label: 'Claude 2',
    is_default: false,
    auth: { state: 'ok', method: 'setup_token' },
    // The user's real situation: weekly window exhausted.
    usage: [
      { window: 'five_hour', used_percent: 12, resets_at: '2026-11-23T23:40:00Z', state: 'ok' },
      { window: 'seven_day', used_percent: 100, resets_at: '2026-11-25T23:40:00Z', state: 'exhausted' },
    ],
    ...over,
  });
}

export function codexLoggedOut(over: Partial<AgentAccount> = {}): AgentAccount {
  return {
    id: 'codex',
    agent: 'codex',
    label: 'Codex',
    is_default: true,
    auth: {
      state: 'logged_out',
      detail: 'codex login status exited 1',
      relogin: { command: 'codex login --device-auth', url: 'https://chatgpt.com/codex' },
    },
    checked_at: '2026-11-23T20:00:00Z',
    chat_supported: true,
    ...over,
  };
}

export function cursorAccount(over: Partial<AgentAccount> = {}): AgentAccount {
  return {
    id: 'cursor',
    agent: 'cursor',
    label: 'Cursor',
    is_default: true,
    auth: { state: 'ok', method: 'keychain' },
    models: [
      { id: 'auto', display_name: 'Auto', description: 'Cursor picks the model', is_default: true },
      { id: 'composer-2', display_name: 'Composer 2', description: "Cursor's own coding model" },
    ],
    default_model: 'auto',
    approval_modes: [
      { id: 'ask', label: 'Ask for approval', description: 'Asks before every tool call', risk: 1, icon: 'hand' },
      { id: 'force', label: 'Full access', description: 'Never asks', risk: 4, icon: 'shield-alert' },
    ],
    default_approval_mode: 'ask',
    usage_note: 'Cursor exposes usage only in its TUI',
    checked_at: '2026-11-23T20:00:00Z',
    chat_supported: true,
    ...over,
  };
}

export function workerAccounts(workerId = FIXTURE_WORKER_ID): WorkerAgentAccount[] {
  return [claudeAccount(), claude2Account(), codexLoggedOut(), cursorAccount()].map((a) => ({
    ...a,
    worker_id: workerId,
  }));
}

let seq = 0;
export function resetSeq(n = 0): void {
  seq = n;
}

/** Builds one AgentChatEvent with an auto-incrementing seq. */
export function ev(kind: string, fields: Partial<AgentChatEvent> = {}): AgentChatEvent {
  seq++;
  return {
    chat_id: FIXTURE_CHAT_ID,
    turn_id: 't1',
    seq,
    kind,
    at: new Date(Date.UTC(2026, 10, 23, 20, 0, seq)).toISOString(),
    ...fields,
  };
}

/** A complete turn: thinking, streamed text, a tool call, a plan, the final message. */
export function sampleTurn(): AgentChatEvent[] {
  resetSeq();
  return [
    ev('session_ready', { turn_id: undefined, agent_session_id: 'sess-abc', model: 'sonnet', effort: 'medium', approval_mode: 'default' }),
    ev('user_message', { text: 'Run the tests and fix failures' }),
    ev('status', { status: 'working' }),
    ev('thought_delta', { text: 'Let me look at ' }),
    ev('thought_delta', { text: 'the test suite.' }),
    ev('tool_call', {
      tool_call: { id: 'tc1', name: 'Bash', kind: 'execute', title: 'Run: go test ./...', status: 'running', input: '{"command":"go test ./..."}' },
    }),
    ev('tool_call', {
      tool_call: { id: 'tc1', name: 'Bash', status: 'completed', output: 'ok  \tkonsert/internal\t0.42s' },
    }),
    ev('plan', {
      plan: [
        { text: 'Run tests', status: 'completed' },
        { text: 'Fix failures', status: 'in_progress' },
        { text: 'Commit', status: 'pending' },
      ],
    }),
    ev('message_delta', { message_id: 'm1', text: 'All tests ' }),
    ev('message_delta', { message_id: 'm1', text: 'pass.' }),
    ev('message', { message_id: 'm1', text: 'All tests pass. Want me to commit?' }),
    ev('usage', { usage: { input_tokens: 1200, output_tokens: 340, cache_read_tokens: 9000, cost_usd: 0.012 } }),
    ev('turn_end', { stop_reason: 'end_turn' }),
    ev('status', { status: 'done' }),
  ];
}
