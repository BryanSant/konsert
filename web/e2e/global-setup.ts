import { ChildProcess, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

// One state file per checkout, so suites running in two worktrees at once never read each
// other's hub.
export const E2E_STATE_FILE = path.join(
  os.tmpdir(),
  `syre-playwright-e2e-${createHash('sha256').update(__dirname).digest('hex').slice(0, 12)}.json`,
);

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
 * and commander LLM override before spawning syre and syred.
 */
const API_KEY_ENV = /^(ANTHROPIC_|OPENAI_|GEMINI_|GOOGLE_API_KEY$|GOOGLE_GENAI_|XAI_|GROK_|MISTRAL_|COHERE_|OPENROUTER_|SYRE_LLM_|CLAUDE_CODE_OAUTH_TOKEN$|CURSOR_API_KEY$|GH_TOKEN$|GITHUB_TOKEN$|COPILOT_)/;

export function subscriptionOnlyEnv(extra: Record<string, string> = {}): Record<string, string | undefined> {
  const env: Record<string, string | undefined> = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (!API_KEY_ENV.test(k)) env[k] = v;
  }
  return { ...env, ...extra };
}

/** Creates a join code with an authenticated session cookie. */
export async function createJoinCode(baseURL: string, cookie: string): Promise<string> {
  const res = await fetch(`${baseURL}/api/machines/join-codes`, {
    method: 'POST',
    headers: { Cookie: cookie, 'Content-Type': 'application/json', 'X-Syre-Request': '1' },
    body: '{}',
  });
  if (!res.ok) throw new Error(`create join code: ${res.status} ${await res.text()}`);
  return ((await res.json()) as { code: string }).code;
}

function flagValue(args: string[], name: string): string {
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === name && i + 1 < args.length) return args[i + 1];
    if (a.startsWith(name + '=')) return a.slice(name.length + 1);
  }
  return '';
}

/**
 * Runs `syred join <hub> <code> --foreground` with its own state dir. Its identity and
 * credential stay out of the developer's ~/.syre, and it never runs (or reads the
 * logins of) the host's real agent CLIs: only mock accounts.
 * Workdir and checkout roots point at a temp directory so ticket worktrees and clones
 * are not created under the developer's ~/src. Pass --allowed-roots to override.
 */
export function spawnJoin(syredBin: string, cwd: string, baseURL: string, code: string, stateDir: string, extra: string[]): ChildProcess {
  const args = ['join', baseURL, code, '--foreground', ...extra];
  let allowed = flagValue(extra, '--allowed-roots');
  if (!allowed) {
    allowed = path.join(stateDir, 'work');
    fs.mkdirSync(allowed, { recursive: true });
    args.push('--allowed-roots', allowed);
  }
  let checkouts = flagValue(extra, '--checkout-roots');
  if (!checkouts) {
    checkouts = allowed;
    args.push('--checkout-roots', checkouts);
  }
  return spawn(syredBin, args, {
    cwd,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: subscriptionOnlyEnv({
      SYRE_STATE_DIR: stateDir,
      SYRE_DISABLE_AGENT_DISCOVERY: '1',
      SYRE_ALLOWED_ROOTS: allowed,
      SYRE_CHECKOUT_ROOTS: checkouts,
    }),
  });
}

export default async function globalSetup(): Promise<() => Promise<void>> {
  const repoRoot = path.resolve(__dirname, '..', '..');
  const syreBin = path.join(repoRoot, 'bin', 'syre');
  const syredBin = path.join(repoRoot, 'bin', 'syred');

  if (!fs.existsSync(syreBin) || !fs.existsSync(syredBin)) {
    throw new Error(`Compiled binaries not found at ${syreBin} and ${syredBin}. Run 'make web && make build' first.`);
  }

  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'syre-e2e-'));
  const dbPath = path.join(tmpDir, 'syre.db');
  const workdir = path.join(tmpDir, 'workspace');
  fs.mkdirSync(workdir, { recursive: true });

  const port = await getFreePort();
  const baseURL = `http://127.0.0.1:${port}`;
  const adminToken = 'e2e-admin-secret-token';
  const workerName = 'e2e-box';

  const procs: ChildProcess[] = [];

  const controller = spawn(
    syreBin,
    [
      'serve',
      '--listen',
      `127.0.0.1:${port}`,
      '--db',
      dbPath,
      '--recovery-key',
      adminToken,
    ],
    {
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'pipe'],
      // GET /api/setup runs `syred id`, and adding this machine would run `syred
      // install`: keep the identity out of ~/.syre and never register a real service.
      env: subscriptionOnlyEnv({
        SYRE_STATE_DIR: path.join(tmpDir, 'hub-state'),
        SYRE_INSTALL_DRY_RUN: '1',
        SYRE_DISABLE_AGENT_DISCOVERY: '1',
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
    'syre /api/health'
  );

  const loginRes = await fetch(`${baseURL}/api/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token: adminToken }),
  });
  const setCookie = loginRes.headers.get('set-cookie') || '';
  const cookieHeader = setCookie.split(';')[0] || '';

  // The worker joins like a manual machine: a one-time code buys its own credential
  // (SYR-RK9YD); --foreground runs it here instead of installing a service.
  const code = await createJoinCode(baseURL, cookieHeader);
  // First root is the e2e workspace, so ticket worktrees and clones land there.
  // /var/tmp is included because some specs put git repos there and dispatch them
  // as task workdirs. Neither is the developer's ~/src.
  const roots = [workdir, '/var/tmp'].join(',');
  const worker = spawnJoin(syredBin, repoRoot, baseURL, code, path.join(tmpDir, 'state'), [
    '--name',
    workerName,
    '--host-group',
    'e2e-lab',
    '--enable-mock-driver',
    '--allowed-roots',
    roots,
    '--checkout-roots',
    roots,
  ]);
  procs.push(worker);

  // syred registers with its own persisted UUIDv7 identity; the hub mints its m- sid.
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
    'syred worker registration'
  );
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(workerId) || !/^m-[0-9a-z]{5,12}$/.test(workerSid)) {
    throw new Error(`worker registered as id=${workerId} sid=${workerSid}; want a UUIDv7 id and an m- sid`);
  }

  fs.writeFileSync(
    E2E_STATE_FILE,
    JSON.stringify({ baseURL, adminToken, workerName, workerId, workerSid, workdir, syredBin, repoRoot, tmpDir })
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
