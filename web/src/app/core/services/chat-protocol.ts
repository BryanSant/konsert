/**
 * Every browser <-> hub name the agent chat UI depends on, in one place, so integration
 * with the hub only has to adjust this file.
 */
export const CHAT_PROTOCOL = {
  // WebSocket, hub -> browser
  /** One AgentChatEvent (payload is the event itself, or {event}). */
  wsEvent: 'agent.chat.event',
  /** fleet.update payload kind carrying one worker's accounts: {kind, worker_id, accounts}. */
  fleetAccountsKind: 'agent_accounts',

  // WebSocket, browser -> hub
  /** {session_id, chat_id, turn_id, content, attachments?} */
  wsSend: 'chat.send',
  /** {chat_id, elicitation_id, option_id?, text?} */
  wsRespond: 'chat.respond',
  /** {chat_id, cancel?, model?, effort?, approval_mode?} */
  wsControl: 'chat.control',
  /** Existing per-session fan-out scoping. */
  wsSubscribe: 'chat.subscribe',
  wsUnsubscribe: 'chat.unsubscribe',

  // REST
  /** POST: create a chat ({title, worker_id, agent, account_id, model, effort, approval_mode, cwd, fleet_tools}). */
  restCreate: '/api/chat/sessions',
  restChat: (id: string) => `/api/chats/${encodeURIComponent(id)}`,
  restEvents: (id: string, afterSeq: number) => `/api/chats/${encodeURIComponent(id)}/events?after_seq=${afterSeq}`,
  restAccounts: '/api/agents/accounts',
  restAccountsRefresh: '/api/agents/accounts/refresh',

  /**
   * Assumed event kind for the user's own prompt in the events log ({turn_id, text}), so the
   * transcript can be rebuilt from events alone. The UI also inserts it optimistically.
   */
  userMessageKind: 'user_message',
} as const;
