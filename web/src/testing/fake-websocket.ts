/**
 * Minimal in-memory WebSocket double for unit tests. Install it with
 * `FakeWebSocket.install()` (returns a restore function) and drive it with
 * `open()`, `receive()` and `serverClose()`.
 */
export class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  static instances: FakeWebSocket[] = [];

  static get last(): FakeWebSocket {
    const ws = FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
    if (!ws) throw new Error('no FakeWebSocket created yet');
    return ws;
  }

  static install(): () => void {
    const original = globalThis.WebSocket;
    FakeWebSocket.instances = [];
    (globalThis as { WebSocket: unknown }).WebSocket = FakeWebSocket;
    return () => {
      (globalThis as { WebSocket: unknown }).WebSocket = original;
      FakeWebSocket.instances = [];
    };
  }

  readonly url: string;
  readyState = FakeWebSocket.CONNECTING;
  binaryType: BinaryType = 'blob';
  readonly sent: Array<string | ArrayBuffer> = [];
  closeCalls: Array<{ code?: number; reason?: string }> = [];

  onopen: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;

  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }

  send(data: string | ArrayBuffer): void {
    if (this.readyState !== FakeWebSocket.OPEN) {
      throw new Error('InvalidStateError: socket is not open');
    }
    this.sent.push(data);
  }

  close(code?: number, reason?: string): void {
    this.closeCalls.push({ code, reason });
    if (this.readyState === FakeWebSocket.CLOSED) return;
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code: code ?? 1000, reason: reason ?? '' } as CloseEvent);
  }

  // ---- test drivers ----

  open(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event('open'));
  }

  receive(data: string | ArrayBuffer): void {
    this.onmessage?.({ data } as MessageEvent);
  }

  receiveJSON(type: string, payload: unknown): void {
    this.receive(JSON.stringify({ type, ts: new Date().toISOString(), payload }));
  }

  /** Simulates the server or network dropping the connection. */
  serverClose(code = 1006): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.({ code, reason: '' } as CloseEvent);
  }

  /** Parsed JSON envelopes sent by the client. */
  sentJSON(): Array<{ type: string; payload: unknown }> {
    return this.sent
      .filter((d): d is string => typeof d === 'string')
      .map((d) => JSON.parse(d) as { type: string; payload: unknown });
  }
}
