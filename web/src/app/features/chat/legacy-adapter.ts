/**
 * Adapts the optional API-key orchestrator's messages (chat.delta / chat.tool / messages
 * REST) to transcript items, so both backends render through the same transcript.
 */
import { ChatMessage, ChatToolCard } from '../../core/models/konsert.models';
import { TranscriptItem, splitAttachments } from '../../core/state/chat-transcript';

export function parseUserMessageWithAttachments(content: string): { text: string; files: string[] } {
  const { text, files } = splitAttachments(content);
  return {
    text,
    files: files.map((fullPath) => {
      const base = fullPath.split(/[\\/]/).pop() || fullPath;
      // Strip the upload's "<session>-<unix>-<rand>-" prefix.
      const parts = base.split('-');
      if (parts.length >= 3 && /^\d{9,}$/.test(parts[0])) return parts.slice(2).join('-');
      if (parts.length >= 4 && /^\d{9,}$/.test(parts[1])) return parts.slice(3).join('-');
      return base;
    }),
  };
}

export function extractTaskIdsFromToolPayload(raw: unknown): string[] {
  const found = new Set<string>();
  const visit = (val: unknown, depth: number) => {
    if (depth > 4 || val == null) return;
    if (typeof val === 'string') {
      try {
        const parsed = JSON.parse(val);
        if (parsed && typeof parsed === 'object') visit(parsed, depth + 1);
      } catch {
        // plain string
      }
      return;
    }
    if (Array.isArray(val)) {
      for (const item of val) visit(item, depth + 1);
      return;
    }
    if (typeof val === 'object') {
      const obj = val as Record<string, unknown>;
      if (typeof obj['task_id'] === 'string' && obj['task_id'].length >= 6) found.add(obj['task_id']);
      if (typeof obj['id'] === 'string' && obj['id'].length >= 6 && ('agent' in obj || 'worker_id' in obj)) found.add(obj['id']);
      for (const k of ['tasks', 'results', 'dispatched', 'data']) if (k in obj) visit(obj[k], depth + 1);
    }
  };
  visit(raw, 0);
  return Array.from(found);
}

function toolName(msg: ChatMessage): string {
  if (!msg.tool_json) return 'tool';
  try {
    return (JSON.parse(msg.tool_json) as { name?: string }).name || 'tool';
  } catch {
    return 'tool';
  }
}

function stringify(v: unknown): string {
  if (v == null) return '';
  if (typeof v === 'string') return v;
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

export function legacyItems(
  messages: ChatMessage[],
  streaming: string,
  liveTools: ChatToolCard[],
): TranscriptItem[] {
  const items: TranscriptItem[] = [];
  for (const m of messages) {
    const turnId = `m${m.id}`;
    if (m.role === 'user') {
      const p = parseUserMessageWithAttachments(m.content);
      items.push({ type: 'user', key: `lu:${m.id}`, turnId, text: p.text, files: p.files, at: m.created_at, pending: m.id < 0 });
    } else if (m.role === 'assistant' && m.content) {
      items.push({ type: 'assistant', key: `la:${m.id}`, turnId, messageId: String(m.id), text: m.content, streaming: false, at: m.created_at });
    } else if (m.role === 'tool') {
      const name = toolName(m);
      items.push({
        type: 'tool',
        key: `lt:${m.id}`,
        turnId,
        call: { id: String(m.id), name, title: name, status: 'completed', output: m.content },
        at: m.created_at,
      });
    }
  }
  for (const c of liveTools) {
    items.push({
      type: 'tool',
      key: `live-tool:${c.call_id}`,
      turnId: 'live',
      call: {
        id: c.call_id,
        name: c.name,
        title: c.name,
        status: c.status === 'error' ? 'failed' : c.status,
        input: stringify(c.args),
        output: stringify(c.result),
      },
      at: '',
    });
  }
  if (streaming) {
    items.push({ type: 'assistant', key: 'live-text', turnId: 'live', messageId: 'live', text: streaming, streaming: true, at: '' });
  }
  return items;
}
