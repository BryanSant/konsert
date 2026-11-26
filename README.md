# Syre (`syre.dev`)

**Single-binary, self-hosted distributed agent control plane.**

Syre cleanly decouples **LLM coordination** from **human observability** across physical hosts, VMs, and Podman/Docker containers:

- **Commanders** (Syre's built-in Commander Chat or external LLMs via MCP stdio / Streamable HTTP) inspect capacity, dispatch parallel tasks, steer running agents, and retrieve concise structured completion payloads (exit code, git diff stats, final summary)—**never ingesting raw PTY stdout**.
- **Humans** get an IDE-style Angular 22 Zoneless Signals PWA embedded directly in the `syre` binary (`//go:embed`) with live on-demand `@xterm/xterm` terminals (Read-Only by default, explicit **Take Control / Interactive** toggle), structured telemetry drawers, and responsive mobile support.

---

## Architecture

- **`syre` (Central Controller / Hub):** Serves the embedded Angular 22 PWA, REST API, Browser WebSocket (`/ws/client`), Worker WebSocket (`/ws/worker`), MCP Server (stdio & Streamable HTTP `/mcp`), and built-in Commander Chat (OpenAI-compatible, Anthropic, Gemini) backed by SQLite WAL (`modernc.org/sqlite` behind a swappable `store.Store` interface).
- **`syred` (Cross-Platform Worker Daemon):** Runs on Linux, macOS, Windows (ConPTY + Job Objects), and Podman/Docker containers using **outbound-only** WebSockets. Supports pluggable `AgentDriver` plugins (`claude`, `agy`, `grok`, `mock`), 512 KB local PTY ring buffers (zero idle network traffic), git worktree isolation, and **Full-Access ("YOLO") execution by default** with opt-in sandboxing (`--sandbox`, `--allowed-roots`).

---

## Quick Start

### 1. Build Controller & Worker (with Embedded PWA)

```bash
make all
```

Or run in development without authentication on localhost:

```bash
# Terminal 1: Start central controller
go run ./cmd/syre serve --no-auth

# Terminal 2: Start worker daemon with built-in mock driver
go run ./cmd/syred run --enable-mock-driver --controller ws://127.0.0.1:8420/ws/worker
```

Open `http://127.0.0.1:8420` in your browser.

### 2. Verify Worker Node Capabilities

```bash
./bin/syred doctor --enable-mock-driver
```

### 3. Production Authentication & Security

Start the controller with auth on (the default; or `--trust-tailscale-headers` behind
Tailscale Serve):

```bash
./bin/syre serve \
  --listen :8420 \
  --db /var/lib/syre/syre.db \
  --public-url https://syre.tailnet.ts.net
```

Nobody copies a token to sign in. The hub prints one line, plus a QR code in a terminal:

```
Sign in: http://desk:8420/pair/7KQ2-M9TX (10 min)
```

Open it in a browser or scan it with your phone. Each browser, phone, CLI and MCP client
gets its own credential, listed in **Settings → Devices** (rename, revoke). A device stays
signed in while it is used at least every 90 days (a setting). For another link run
`syre pair` on the hub, or use **Add a device** in a signed-in browser. See
[docs/deploy.md](docs/deploy.md#1-signing-in) for the CLI, MCP clients and the recovery key.

Add machines from the PWA (**Add machine**). The hub's own machine and hosts in your
`~/.ssh/config` are added with one click. Any other machine joins with a one-time code:
**Add machine → Manual** shows a single command to run on it, for example

```bash
syred join https://syre.tailnet.ts.net ABCD-EFGH
```

The code works once, for 15 minutes. `syred join` trades it for the machine's own
credential (kept in `~/.syre`, readable only by you) and installs syred to start at
login; add run flags as needed (`--sandbox --allowed-roots /home/dev/repos`,
`--host-group fedora-main`, `--max-concurrent 4`) or `--foreground` to run it in the
terminal. Nobody copies a token: each machine has its own credential, which
**Fleet → Details → Revoke credential** cuts off immediately, and
`syre token rotate machines` (or **Settings → Security**) revokes for every machine
that joined by hand.

### 4. MCP Integration

- **Streamable HTTP:** In the web app, **Settings → Connect an MCP client** creates a
  credential for `/mcp` and shows ready-to-paste config (a `claude mcp add --transport http …`
  command and a JSON snippet).
- **Stdio Bridge:** Configure your local desktop/CLI commander to run `syre mcp`. On the
  hub's machine it needs nothing else; elsewhere run `syre login https://hub:8420` once.

Available MCP tools: `fleet_status`, `backlog`, `manage_work`, `control`, `set_policy`, `reply`, `inspect`, `memory`, `ask_commander`.

### 5. Built-in Commander Chat Configuration

Set environment variables (or `commander:` in controller YAML):

```bash
# OpenAI / OpenRouter / xAI / Ollama / vLLM
export SYRE_LLM_PROVIDER=openai
export SYRE_LLM_MODEL=gpt-4o
export SYRE_LLM_API_KEY=...
# Optional for OpenRouter / Ollama / vLLM:
# export SYRE_LLM_BASE_URL=http://127.0.0.1:11434/v1

# Or Anthropic:
# export SYRE_LLM_PROVIDER=anthropic
# export SYRE_LLM_MODEL=claude-sonnet-4-20250514
# export ANTHROPIC_API_KEY=...

# Or Google Gemini:
# export SYRE_LLM_PROVIDER=gemini
# export SYRE_LLM_MODEL=gemini-2.5-pro
# export GEMINI_API_KEY=...
```

---

## Deployment Templates

See **[docs/deploy.md](docs/deploy.md)** for a full production guide: HTTPS via Tailscale Serve (or Caddy/Nginx), the controller as a systemd service, and workers under systemd, launchd, Podman Quadlet and Windows.

Service templates are provided under `deploy/`:
- `deploy/systemd/syre.service` & `deploy/systemd/syred.service`
- `deploy/quadlet/syred.container` (Podman Quadlet)
- `deploy/launchd/dev.syre.syred.plist` (macOS launchd)

## Releasing

Before pushing a `v*` tag (which triggers `.github/workflows/release.yml`):

1. CI is green on the commit you will tag, including the Playwright e2e job (it runs the
   subscription-only chat flow against the mock engine with no API key).
2. Run the manual **[pre-release walkthrough](docs/walkthrough.md)** against the real agent
   CLIs on a real machine with no API key, and record the sign-off in the release ticket or
   PR. A failed required step blocks the release.
3. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`.

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
