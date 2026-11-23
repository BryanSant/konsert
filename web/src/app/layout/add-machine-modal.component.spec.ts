import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AddMachineModalComponent } from './add-machine-modal.component';

function sseResponse(
  frames: unknown[],
  opts: { hang?: boolean; signal?: AbortSignal } = {},
): Response {
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const f of frames) controller.enqueue(enc.encode(`data: ${JSON.stringify(f)}\n\n`));
      if (opts.hang) {
        opts.signal?.addEventListener('abort', () =>
          controller.error(new DOMException('aborted', 'AbortError')),
        );
      } else {
        controller.close();
      }
    },
  });
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
}

describe('AddMachineModalComponent SSH bootstrap', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(async (url: string) => {
      if (url === '/api/ssh/hosts') {
        return new Response(JSON.stringify({ hosts: [] }), { status: 200 });
      }
      throw new Error(`unexpected ${url}`);
    });
    vi.stubGlobal('fetch', fetchMock);
    TestBed.configureTestingModule({});
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    TestBed.resetTestingModule();
  });

  async function create() {
    const fixture = TestBed.createComponent(AddMachineModalComponent);
    fixture.detectChanges();
    await fixture.whenStable();
    return fixture;
  }

  it('goes through ApiClientService with JSON + CSRF headers and streams stages', async () => {
    const fixture = await create();
    const cmp = fixture.componentInstance;
    const stages: string[] = [];
    fetchMock.mockImplementationOnce(async () =>
      sseResponse([
        { type: 'stage', label: 'Uploading', detail: 'konsertd' },
        { type: 'done', result: { ok: true, target: 'box', message: 'ready' } },
      ]),
    );
    const origSet = cmp.sshStageLabel.set.bind(cmp.sshStageLabel);
    cmp.sshStageLabel.set = (v: string) => {
      stages.push(v);
      origSet(v);
    };

    await cmp.bootstrapSSHHost('box');
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const headers = new Headers(init.headers);
    expect(url).toBe('/api/ssh/bootstrap?stream=1');
    expect(headers.get('Content-Type')).toBe('application/json');
    expect(headers.get('X-Konsert-Request')).toBe('1');
    expect(JSON.parse(init.body as string)).toMatchObject({ target: 'box' });
    expect(stages).toContain('Uploading');
    expect(cmp.sshStatusMsg()).toBe('ready');
    expect(cmp.bootstrappingTarget()).toBe('');
  });

  it('surfaces HTTP errors instead of treating them as success', async () => {
    const fixture = await create();
    const cmp = fixture.componentInstance;
    fetchMock.mockImplementationOnce(
      async () =>
        new Response(JSON.stringify({ error: 'ssh: permission denied' }), { status: 502 }),
    );
    await cmp.bootstrapSSHHost('box');
    expect(cmp.sshErrorMsg()).toBe('ssh: permission denied');
    expect(cmp.sshStatusMsg()).toBe('');
  });

  it('aborts an in-flight bootstrap when the modal is destroyed', async () => {
    const fixture = await create();
    const cmp = fixture.componentInstance;
    let signal: AbortSignal | undefined;
    fetchMock.mockImplementationOnce(async (_url: string, init: RequestInit) => {
      signal = init.signal ?? undefined;
      return sseResponse([{ type: 'stage', label: 'Connecting' }], { hang: true, signal });
    });
    const p = cmp.bootstrapSSHHost('box');
    await vi.waitFor(() => expect(signal).toBeDefined());
    fixture.destroy();
    await p;
    expect(signal?.aborted).toBe(true);
    expect(cmp.sshErrorMsg()).toBe('');
  });
});
