import { ChildProcess, spawn } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

export const E2E_STATE_FILE = path.join(os.tmpdir(), 'konsert-playwright-e2e.json');

export function getFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      if (!addr || typeof addr === 'string') {
        srv.close(() => reject(new Error('unable to allocate ephemeral port')));
        return;
      }
      const port = addr.port;
      srv.close(() => resolve(port));
    });
  });
}

export async function waitFor(predicate: () => Promise<boolean>, timeoutMs: number, label: string): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      if (await predicate()) return;
    } catch {
      // retry
    }
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`Timed out waiting for ${label}`);
}

/**
 * The e2e stack must prove the subscription-first promise: nothing may work because an API
 * key happens to be in the developer's or CI runner's environment. Strip every provider key
 * and orchestrator LLM override before spawning konsert and konsertd.
 */
const API_KEY_ENV = /^(ANTHROPIC_|OPENAI_|GEMINI_|GOOGLE_API_KEY$|GOOGLE_GENAI_|XAI_|GROK_|MISTRAL_|COHERE_|OPENROUTER_|KONSERT_LLM_|CLAUDE_CODE_OAUTH_TOKEN$|CURSOR_API_KEY$|GH_TOKEN$|GITHUB_TOKEN$|COPILOT_)/;

export function subscriptionOnlyEnv(extra: Record<string, string> = {}): Record<string, string | undefined> {
  const env: Record<string, string | undefined> = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (!API_KEY_ENV.test(k)) env[k] = v;
  }
  return { ...env, ...extra };
}

export default async function globalSetup(): Promise<() => Promise<void>> {
  const repoRoot = path.resolve(__dirname, '..', '..');
  const konsertBin = path.join(repoRoot, 'bin', 'konsert');
  const konsertdBin = path.join(repoRoot, 'bin', 'konsertd');

  if (!fs.existsSync(konsertBin) || !fs.existsSync(konsertdBin)) {
    throw new Error(`Compiled binaries not found at ${konsertBin} and ${konsertdBin}. Run 'make web && make build' first.`);
  }

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'konsert-e2e-'));
  const dbPath = path.join(tmpDir, 'konsert.db');
  const workdir = path.join(tmpDir, 'workspace');
  fs.mkdirSync(workdir, { recursive: true });

  const port = await getFreePort();
  const baseURL = `http://127.0.0.1:${port}`;
  const adminToken = 'e2e-admin-secret-token';
  const fleetToken = 'e2e-fleet-secret-token';
  const workerName = 'e2e-box';

  const procs: ChildProcess[] = [];

  const controller = spawn(
    konsertBin,
    [
      'serve',
      '--listen',
      `127.0.0.1:${port}`,
      '--db',
      dbPath,
      '--admin-token',
      adminToken,
      '--fleet-token',
      fleetToken,
    ],
    {
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'pipe'],
      // GET /api/setup runs `konsertd id`, and adding this machine would run `konsertd
      // install`: keep the identity out of ~/.konsert and never register a real service.
      env: subscriptionOnlyEnv({
        KONSERT_STATE_DIR: path.join(tmpDir, 'hub-state'),
        KONSERT_INSTALL_DRY_RUN: '1',
        KONSERT_DISABLE_AGENT_DISCOVERY: '1',
      }),
    }
  );
  procs.push(controller);

  await waitFor(
    async () => {
      const res = await fetch(`${baseURL}/api/health`);
      return res.ok;
    },
    10_000,
    'konsert /api/health'
  );

  const worker = spawn(
    konsertdBin,
    [
      'run',
      '--controller',
      `ws://127.0.0.1:${port}/ws/worker`,
      '--token',
      fleetToken,
      '--name',
      workerName,
      '--host-group',
      'e2e-lab',
      '--enable-mock-driver',
    ],
    {
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'pipe'],
      // Keep the worker identity and the status outbox out of the developer's ~/.konsert, and
      // never run (or read the logins of) the host's real agent CLIs: only mock accounts.
      env: subscriptionOnlyEnv({
        KONSERT_STATE_DIR: path.join(tmpDir, 'state'),
        KONSERT_DISABLE_AGENT_DISCOVERY: '1',
      }),
    }
  );
  procs.push(worker);

  // Wait until worker registers and is online
  const loginRes = await fetch(`${baseURL}/api/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token: adminToken }),
  });
  const setCookie = loginRes.headers.get('set-cookie') || '';
  const cookieHeader = setCookie.split(';')[0] || '';

  // konsertd registers with its own persisted UUIDv7 identity; the hub mints its m- sid.
  let workerId = '';
  let workerSid = '';
  await waitFor(
    async () => {
      const res = await fetch(`${baseURL}/api/fleet`, {
        headers: { Cookie: cookieHeader },
      });
      if (!res.ok) return false;
      const body = (await res.json()) as { workers?: Array<{ id: string; sid: string; name: string; status: string }> };
      const w = body.workers?.find((w) => w.name === workerName && w.status === 'online');
      if (!w) return false;
      workerId = w.id;
      workerSid = w.sid;
      return true;
    },
    10_000,
    'konsertd worker registration'
  );
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(workerId) || !/^m-[0-9a-z]{5,12}$/.test(workerSid)) {
    throw new Error(`worker registered as id=${workerId} sid=${workerSid}; want a UUIDv7 id and an m- sid`);
  }

  fs.writeFileSync(
    E2E_STATE_FILE,
    JSON.stringify({ baseURL, adminToken, workerName, workerId, workerSid, workdir })
  );

  return async () => {
    for (const p of procs) {
      if (!p.killed) {
        p.kill('SIGTERM');
      }
    }
    await new Promise((r) => setTimeout(r, 300));
    for (const p of procs) {
      if (!p.killed) {
        p.kill('SIGKILL');
      }
    }
    try {
      fs.rmSync(tmpDir, { recursive: true, force: true });
      fs.rmSync(E2E_STATE_FILE, { force: true });
    } catch {
      // ignore cleanup errors
    }
  };
}
