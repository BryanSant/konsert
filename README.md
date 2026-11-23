# Konsert (`konsert.dev`)

**Single-binary, self-hosted distributed agent control plane.**

Konsert cleanly decouples **LLM orchestration** from **human observability** across physical hosts, VMs, and Podman/Docker containers:

- **Orchestrators** (Konsert's built-in Orchestrator Chat or external LLMs via MCP stdio / Streamable HTTP) inspect capacity, dispatch parallel tasks, steer running agents, and retrieve concise structured completion payloads (exit code, git diff stats, final summary)—**never ingesting raw PTY stdout**.
- **Humans** get an IDE-style Angular 22 Zoneless Signals PWA embedded directly in the `konsert` binary (`//go:embed`) with live on-demand `@xterm/xterm` terminals (Read-Only by default, explicit **Take Control / Interactive** toggle), structured telemetry drawers, and responsive mobile support.

---

## Architecture

- **`konsert` (Central Controller / Hub):** Serves the embedded Angular 22 PWA, REST API, Browser WebSocket (`/ws/client`), Worker WebSocket (`/ws/worker`), MCP Server (stdio & Streamable HTTP `/mcp`), and built-in Orchestrator Chat (OpenAI-compatible, Anthropic, Gemini) backed by SQLite WAL (`modernc.org/sqlite` behind a swappable `store.Store` interface).
- **`konsertd` (Cross-Platform Worker Daemon):** Runs on Linux, macOS, Windows (ConPTY + Job Objects), and Podman/Docker containers using **outbound-only** WebSockets. Supports pluggable `AgentDriver` plugins (`claude`, `agy`, `grok`, `mock`), 512 KB local PTY ring buffers (zero idle network traffic), git worktree isolation, and **Full-Access ("YOLO") execution by default** with opt-in sandboxing (`--sandbox`, `--allowed-roots`).

---

## Quick Start

### 1. Build Controller & Worker (with Embedded PWA)

```bash
make all
```

Or run in development without authentication on localhost:

```bash
# Terminal 1: Start central controller
go run ./cmd/konsert serve --no-auth

# Terminal 2: Start worker daemon with built-in mock driver
go run ./cmd/konsertd run --enable-mock-driver --controller ws://127.0.0.1:8420/ws/worker
```

Open `http://127.0.0.1:8420` in your browser.

### 2. Verify Worker Node Capabilities

```bash
./bin/konsertd doctor --enable-mock-driver
```

### 3. Production Authentication & Security

Generate secure tokens:

```bash
FLEET_TOKEN=$(./bin/konsert token generate)
ADMIN_TOKEN=$(./bin/konsert token generate)
```

Start the controller with auth enabled (or `--trust-tailscale-headers` behind Tailscale Serve):

```bash
./bin/konsert serve \
  --listen :8420 \
  --db /var/lib/konsert/konsert.db \
  --fleet-token "$FLEET_TOKEN" \
  --admin-token "$ADMIN_TOKEN"
```

Connect workers outbound:

```bash
./bin/konsertd run \
  --controller wss://konsert.tailnet.ts.net/ws/worker \
  --token "$FLEET_TOKEN" \
  --host-group fedora-main \
  --max-concurrent 4
```

To opt into sandboxing on sensitive hosts:

```bash
./bin/konsertd run \
  --controller wss://konsert.tailnet.ts.net/ws/worker \
  --token "$FLEET_TOKEN" \
  --sandbox \
  --allowed-roots /home/dev/repos,/tmp/work
```

### 4. MCP Integration

- **Streamable HTTP:** Point your MCP client to `http://<host>:8420/mcp` with `Authorization: Bearer <mcp_or_admin_token>`.
- **Stdio Bridge:** Configure your local desktop/CLI orchestrator to run:
  ```bash
  konsert mcp --url http://127.0.0.1:8420 --token "$ADMIN_TOKEN"
  ```

Available MCP tools: `fleet_status`, `dispatch_task`, `list_tasks`, `get_task_result`, `steer_task`, `cancel_task`.

### 5. Built-in Orchestrator Chat Configuration

Set environment variables (or `orchestrator:` in controller YAML):

```bash
# OpenAI / OpenRouter / xAI / Ollama / vLLM
export KONSERT_LLM_PROVIDER=openai
export KONSERT_LLM_MODEL=gpt-4o
export KONSERT_LLM_API_KEY=...
# Optional for OpenRouter / Ollama / vLLM:
# export KONSERT_LLM_BASE_URL=http://127.0.0.1:11434/v1

# Or Anthropic:
# export KONSERT_LLM_PROVIDER=anthropic
# export KONSERT_LLM_MODEL=claude-sonnet-4-20250514
# export ANTHROPIC_API_KEY=...

# Or Google Gemini:
# export KONSERT_LLM_PROVIDER=gemini
# export KONSERT_LLM_MODEL=gemini-2.5-pro
# export GEMINI_API_KEY=...
```

---

## Deployment Templates

Production service templates are provided under `deploy/`:
- `deploy/systemd/konsert.service` & `deploy/systemd/konsertd.service`
- `deploy/quadlet/konsertd.container` (Podman Quadlet)
- `deploy/launchd/dev.konsert.konsertd.plist` (macOS launchd)

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
