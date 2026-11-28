import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LocalHostInfo, SetupStatus, Worker } from '../core/models/syre.models';
import { FleetStore } from '../core/state/fleet.store';
import { AddMachinePanelComponent, localHostSummary } from './add-machine-panel.component';

function setupWith(local: Partial<LocalHostInfo>): SetupStatus {
  return {
    needs_setup: true,
    local: { hostname: 'studio', syred_found: true, worker_id: 'w-local', connected: false, added: false, ...local },
  };
}

function localWorker(status: string, extra: Partial<Worker> = {}): Worker {
  return {
    id: 'w-local',
    sid: 'm-local',
    name: 'studio',
    host_group: 'studio',
    os: 'darwin',
    arch: 'arm64',
    isolation: 'host',
    permission_mode: 'full_access',
    agents: [],
    tags: {},
    max_concurrent: 2,
    status,
    last_seen_at: new Date().toISOString(),
    launch: 'local',
    ...extra,
  };
}

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

function callTo(fetchMock: ReturnType<typeof vi.fn>, url: string, nth = 0): [string, RequestInit] {
  const calls = fetchMock.mock.calls.filter((c) => c[0] === url);
  return calls[nth] as [string, RequestInit];
}

describe('AddMachinePanelComponent SSH bootstrap', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn(async (url: string) => {
      if (url === '/api/ssh/hosts') {
        return new Response(JSON.stringify({ hosts: [] }), { status: 200 });
      }
      if (url === '/api/setup') {
        return new Response(JSON.stringify(setupWith({})), { status: 200 });
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
    const fixture = TestBed.createComponent(AddMachinePanelComponent);
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
        { type: 'stage', label: 'Uploading', detail: 'syred' },
        { type: 'done', result: { ok: true, target: 'box', message: 'ready' } },
      ]),
    );
    const origSet = cmp.sshStageLabel.set.bind(cmp.sshStageLabel);
    cmp.sshStageLabel.set = (v: string) => {
      stages.push(v);
      origSet(v);
    };

    await cmp.bootstrapSSHHost('box');
    const [url, init] = callTo(fetchMock, '/api/ssh/bootstrap?stream=1');
    const headers = new Headers(init.headers);
    expect(url).toBe('/api/ssh/bootstrap?stream=1');
    expect(headers.get('Content-Type')).toBe('application/json');
    expect(headers.get('X-Syre-Request')).toBe('1');
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

  it('asks the operator to verify an unknown host key, then retries with accept_host_key', async () => {
    const fixture = await create();
    const cmp = fixture.componentInstance;
    fetchMock.mockImplementationOnce(async () =>
      sseResponse([
        {
          type: 'done',
          result: {
            ok: false,
            target: 'box',
            stage: 'host_key_confirmation',
            host_key_confirmation_required: true,
            host_keys: [{ type: 'ssh-ed25519', fingerprint: 'SHA256:abc' }],
          },
        },
      ]),
    );
    await cmp.bootstrapSSHHost('box');
    expect(cmp.hostKeyPrompt()?.keys[0].fingerprint).toBe('SHA256:abc');
    expect(cmp.sshErrorMsg()).toBe('');
    fixture.detectChanges();
    expect((fixture.nativeElement as HTMLElement).textContent).toContain('SHA256:abc');

    fetchMock.mockImplementationOnce(async () =>
      sseResponse([{ type: 'done', result: { ok: true, target: 'box', message: 'ready' } }]),
    );
    await cmp.confirmHostKey({ type: 'ssh-ed25519', fingerprint: 'SHA256:abc' });
    const [, init] = callTo(fetchMock, '/api/ssh/bootstrap?stream=1', 1);
    const body = JSON.parse(init.body as string);
    expect(body).toMatchObject({ target: 'box', accept_host_key: 'SHA256:abc' });
    expect(body.controller_url).toBeUndefined();
    expect(cmp.hostKeyPrompt()).toBeNull();
    expect(cmp.sshStatusMsg()).toBe('ready');
  });

  it('flags a changed host key prominently', async () => {
    const fixture = await create();
    const cmp = fixture.componentInstance;
    fetchMock.mockImplementationOnce(async () =>
      sseResponse([
        {
          type: 'done',
          result: {
            ok: false,
            target: 'box',
            stage: 'connecting',
            error:
              'SSH host key for box has CHANGED (possible man-in-the-middle). Refusing to continue',
          },
        },
      ]),
    );
    await cmp.bootstrapSSHHost('box');
    expect(cmp.hostKeyChanged()).toBe(true);
    fixture.detectChanges();
    expect((fixture.nativeElement as HTMLElement).textContent).toContain(
      'possible man-in-the-middle',
    );
  });

  it('aborts an in-flight bootstrap when the panel is destroyed', async () => {
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

describe('AddMachinePanelComponent this machine', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let setup: SetupStatus;
  let fleetWorkers: Worker[];

  beforeEach(() => {
    setup = setupWith({});
    fleetWorkers = [];
    fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
      switch (url) {
        case '/api/ssh/hosts':
          return new Response(JSON.stringify({ hosts: [] }), { status: 200 });
        case '/api/setup':
          return new Response(JSON.stringify(setup), { status: 200 });
        case '/api/fleet':
          return new Response(JSON.stringify({ workers: fleetWorkers, tasks: [] }), { status: 200 });
        case '/api/workers/local':
          expect(init?.method).toBe('POST');
          return new Response(
            JSON.stringify(
              setup.local.connected
                ? { worker_id: setup.local.worker_id, state: 'connected' }
                : { worker_id: 'w-local', worker_sid: 'm-local', state: 'starting' },
            ),
            { status: setup.local.connected ? 200 : 202 },
          );
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

  async function create(firstRun = true) {
    const fixture = TestBed.createComponent(AddMachinePanelComponent);
    fixture.componentRef.setInput('firstRun', firstRun);
    fixture.detectChanges();
    const cmp = fixture.componentInstance;
    await vi.waitFor(() => {
      expect(cmp.local()).not.toBeNull();
      expect(cmp.setup.loading()).toBe(false);
      expect(cmp.sshHostsLoading()).toBe(false);
    });
    fixture.detectChanges();
    return fixture;
  }

  function text(fixture: { nativeElement: unknown }): string {
    return (fixture.nativeElement as HTMLElement).textContent ?? '';
  }

  it('summarizes each local state', () => {
    expect(localHostSummary(setupWith({ connected: true }).local)).toBe('syred is running — will attach');
    expect(localHostSummary(setupWith({}).local)).toMatch(/^Will start syred/);
    expect(localHostSummary(setupWith({ syred_found: false, fix: 'Put syred on PATH.' }).local)).toBe(
      'syred not found: Put syred on PATH.',
    );
    expect(localHostSummary(setupWith({ launch_state: 'failed', launch_error: 'boom' }).local)).toBe(
      'Could not start syred: boom',
    );
  });

  it('puts this machine first in the first-run dialog', async () => {
    const fixture = await create();
    const t = text(fixture);
    expect(t).toContain('Add your first machine');
    expect(t).toContain('This machine (studio)');
    expect(t.indexOf('This machine (studio)')).toBeLessThan(t.indexOf('From your ssh config'));
    expect(t).toContain('Will start syred');
  });

  it('attaches a running syred without starting another', async () => {
    setup = setupWith({ connected: true, worker_id: 'w-custom' });
    const fixture = await create();
    expect(text(fixture)).toContain('syred is running — will attach');
    const btn = (fixture.nativeElement as HTMLElement).querySelector<HTMLButtonElement>('[data-testid="add-local"]');
    expect(btn?.textContent?.trim()).toBe('Attach');
    fleetWorkers = [localWorker('online', { id: 'w-custom' })];
    await fixture.componentInstance.addLocal();
    fixture.detectChanges();
    expect(fetchMock.mock.calls.filter((c) => c[0] === '/api/workers/local')).toHaveLength(1);
    expect(fixture.componentInstance.localPhase()).toBe('online');
    expect(text(fixture)).toContain('Attached the syred running on studio');
  });

  it('starts syred and follows the machine until it is online', async () => {
    const fixture = await create();
    fleetWorkers = [localWorker('offline', { launch_state: 'starting' })];
    await fixture.componentInstance.addLocal();
    fixture.detectChanges();
    expect(fixture.componentInstance.localPhase()).toBe('starting');
    expect(text(fixture)).toContain('Starting syred...');

    // The hub reports the daemon connected (over the socket in the app).
    TestBed.inject(FleetStore).upsertWorker(localWorker('online'));
    fixture.detectChanges();
    expect(fixture.componentInstance.localPhase()).toBe('online');
    expect(text(fixture)).toContain('Online');
  });

  it('shows a failed start with a retry', async () => {
    const fixture = await create();
    fleetWorkers = [localWorker('offline', { launch_state: 'failed', launch_error: 'launchctl failed' })];
    await fixture.componentInstance.addLocal();
    fixture.detectChanges();
    expect(fixture.componentInstance.localPhase()).toBe('failed');
    const btn = (fixture.nativeElement as HTMLElement).querySelector<HTMLButtonElement>('[data-testid="add-local"]');
    expect(btn?.textContent?.trim()).toBe('Retry');
  });

  it('explains how to fix a missing syred and offers no add button', async () => {
    setup = setupWith({ syred_found: false, worker_id: undefined, fix: 'Put syred next to the syre binary.' });
    const fixture = await create();
    expect(text(fixture)).toContain('syred not found: Put syred next to the syre binary.');
    expect((fixture.nativeElement as HTMLElement).querySelector('[data-testid="add-local"]')).toBeNull();
  });
});
