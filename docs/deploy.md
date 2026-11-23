# Deploying Konsert

This guide covers a production-style homelab deployment:

- the **controller** (`konsert`) runs as a service on one always-on host, listening on loopback only;
- **Tailscale Serve** (or another HTTPS reverse proxy) puts it on your tailnet at a real
  `https://<host>.<tailnet>.ts.net` URL, so browsers and workers use HTTPS/WSS;
- **workers** (`konsertd`) run on every machine that should execute agents and connect
  *outbound* to the controller over WSS. No inbound ports are needed on worker hosts.

Templates referenced below live in [`deploy/`](../deploy).

## 1. Tokens

The controller uses three bearer tokens:

| Token | Used by | Flag / env / YAML |
|---|---|---|
| Admin | PWA login, REST API, `konsert` CLI, `konsertd mcp` | `--admin-token` / `KONSERT_ADMIN_TOKEN` / `admin_token` |
| Fleet | `konsertd` workers on `/ws/worker` | `--fleet-token` / `KONSERT_FLEET_TOKEN` / `fleet_token` |
| MCP | Streamable HTTP clients on `/mcp` (defaults to the admin token) | `--mcp-token` / `KONSERT_MCP_TOKEN` / `mcp_token` |

If no tokens are configured, `konsert serve` generates the admin and fleet tokens on first
start, prints them once, and saves them (mode 0600) to `konsert-tokens.yaml` next to the
database (`--tokens-file` to change). To choose your own:

```bash
konsert token generate   # prints a random token; run once per token
```

Authentication is never silently off: `--no-auth` is for isolated local development and
binds to `127.0.0.1` by default.

## 2. Controller as a systemd service (Linux)

```bash
sudo useradd --system --home-dir /var/lib/konsert --create-home --shell /usr/sbin/nologin konsert
sudo install -m 0755 konsert /usr/local/bin/konsert
sudo install -d -m 0750 -o root -g konsert /etc/konsert
```

`/etc/konsert/konsert.yaml`:

```yaml
listen_addr: 127.0.0.1:8420        # loopback only; Tailscale Serve / the proxy fronts it
db_path: /var/lib/konsert/konsert.db
public_url: https://konsert-hub.example-tailnet.ts.net
trust_tailscale_headers: true      # only with Tailscale Serve (section 3)
orchestrator:
  provider: anthropic              # openai | anthropic | gemini
```

`/etc/konsert/konsert.env` (mode 0640, group `konsert`) holds secrets:

```bash
KONSERT_ADMIN_TOKEN=...
KONSERT_FLEET_TOKEN=...
ANTHROPIC_API_KEY=...
```

Install [`deploy/systemd/konsert.service`](../deploy/systemd/konsert.service) to
`/etc/systemd/system/`, then:

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now konsert
journalctl -u konsert -f
```

The unit is sandboxed (`ProtectSystem=strict`, `NoNewPrivileges`, ...) and may only write to
`/var/lib/konsert`. SSH machine bootstrap reads and updates the service user's `~/.ssh`, so
keep the `konsert` user's home at `/var/lib/konsert` (as created above).

## 3. HTTPS with Tailscale Serve (recommended)

One-time tailnet setup (admin console): **DNS → MagicDNS** on, and **DNS → HTTPS
Certificates → Enable**. The first `tailscale serve` on a node prints an approval link if
Serve isn't enabled yet. Note that HTTPS certificates are issued by Let's Encrypt, so your
machine names appear in public Certificate Transparency logs.

On the controller host:

```bash
sudo tailscale serve --bg --https=443 http://127.0.0.1:8420
tailscale serve status
```

The controller is now at `https://<host>.<tailnet>.ts.net`. Set `public_url` to exactly that
URL: it is the allowed browser origin and the controller URL handed to SSH-bootstrapped
workers.

### Identity headers (`trust_tailscale_headers`)

Tailscale Serve adds `Tailscale-User-Login` (plus `-Name` / `-Profile-Pic`) to every request
from a tailnet user. With `trust_tailscale_headers: true` the controller treats that header
as an authenticated browser session, so tailnet users open the PWA without the admin-token
login screen.

The header is honored **only from loopback peers**, which is where Tailscale Serve connects
from. Keep `listen_addr` on `127.0.0.1`; on a non-loopback listener anyone who can reach the
port directly could otherwise forge the header, and the controller logs a warning at startup.

Never enable `trust_tailscale_headers` behind a proxy other than Tailscale Serve.

### Workers over WSS

```bash
konsertd run --controller wss://<host>.<tailnet>.ts.net/ws/worker --token "$KONSERT_FLEET_TOKEN"
```

Workers authenticate with the fleet token (the identity header is not used for
`/ws/worker`) and reconnect with backoff if the controller restarts.

### Verification checklist

```bash
URL=https://<host>.<tailnet>.ts.net
# From another tailnet node: the identity header authenticates the browser API.
curl -s $URL/api/auth/me                 # {"authenticated":true,"user":"you@example.com",...}
# The header is ignored when it doesn't come from Tailscale Serve.
curl -s -H 'Tailscale-User-Login: x' http://<host-ip>:8420/api/auth/me   # unreachable (loopback bind)
# Fleet status sees your workers.
konsert status --url $URL --token "$KONSERT_ADMIN_TOKEN"
```

In a browser, open the URL: the PWA loads without a login prompt, the footer shows the
connected hosts, and DevTools → Application → Service Workers shows `ngsw-worker.js` active.

## 4. Other reverse proxies (Caddy, Nginx)

Any HTTPS proxy works if it forwards WebSocket upgrades and the original `Host` header. The
controller checks a browser's `Origin` against the request `Host` and `public_url`, so either
pass `Host` through unchanged or set `public_url` to the external URL. Leave
`trust_tailscale_headers` **off**; browsers log in with the admin token, and the session
cookie is marked `Secure` when the proxy sends `X-Forwarded-Proto: https`.

Caddy:

```caddy
konsert.example.com {
	reverse_proxy 127.0.0.1:8420
}
```

Nginx:

```nginx
server {
    listen 443 ssl;
    server_name konsert.example.com;
    # ssl_certificate / ssl_certificate_key ...

    client_max_body_size 20m;          # attachments are capped at ~17 MiB by the controller

    location / {
        proxy_pass http://127.0.0.1:8420;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 1h;         # long-lived /ws/client and /ws/worker sockets
        proxy_buffering off;           # SSE (SSH bootstrap progress) and streaming
    }
}
```

## 5. Workers

Each worker needs the controller URL and the fleet token, either as flags, environment
variables (`KONSERT_CONTROLLER_URL`, `KONSERT_FLEET_TOKEN`, `KONSERT_WORKER_NAME`, ...), or a
YAML file:

```yaml
# /etc/konsert/konsertd.yaml
controller_url: wss://konsert-hub.example-tailnet.ts.net/ws/worker
name: bocabox
max_concurrent: 2
# sandbox: true          # opt-in: no YOLO/skip-permissions flags, shell driver refused
# allowed_roots: [/home/me/src]
```

Put the token in `/etc/konsert/konsertd.env` as `KONSERT_FLEET_TOKEN=...`. Check a host with
`konsertd doctor`.

konsertd keeps its stable worker ID, machine ID and status outbox in `~/.konsert` (override
with `KONSERT_STATE_DIR`), so a re-run never registers a duplicate machine.

Agents run as the user konsertd runs as, with that user's CLI logins (`claude`, `grok`,
...), so run workers as your normal user rather than a system account.

### Linux: systemd

Install [`deploy/systemd/konsertd.service`](../deploy/systemd/konsertd.service). To run it as
your user, either add `User=you` to the unit or install it as a user unit:

```bash
mkdir -p ~/.config/systemd/user && cp deploy/systemd/konsertd.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now konsertd
loginctl enable-linger "$USER"     # keep it running while logged out
```

### macOS: launchd

Copy [`deploy/launchd/dev.konsert.konsertd.plist`](../deploy/launchd/dev.konsert.konsertd.plist)
to `~/Library/LaunchAgents/`, adjust the paths and add `KONSERT_FLEET_TOKEN` under
`EnvironmentVariables` (or point `--config` at a file in your home directory), then:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.konsert.konsertd.plist
launchctl kickstart -k gui/$(id -u)/dev.konsert.konsertd
```

### Containers: Podman Quadlet

[`deploy/quadlet/konsertd.container`](../deploy/quadlet/konsertd.container) runs the
`ghcr.io/bryansant/konsertd` image with `~/src` mounted at `/workspace`:

```bash
mkdir -p ~/.config/containers/systemd && cp deploy/quadlet/konsertd.container ~/.config/containers/systemd/
systemctl --user daemon-reload && systemctl --user start konsertd
```

The image has no agent CLIs logged in; install and authenticate them in a derived image or
mount their config directories.

### Windows

Run `konsertd.exe run --controller wss://... --token ...` from a terminal, or register it as a
Task Scheduler task that runs at logon as your user. ConPTY and Job Objects are used
automatically.

## 6. Host Keep Awake

The footer's Keep Awake control can also stop worker hosts from sleeping during long runs.
konsertd holds the lock with:

| OS | Mechanism |
|---|---|
| macOS | `caffeinate -i` (tied to the konsertd process) |
| Linux | `systemd-inhibit` (logind) |
| Windows | `SetThreadExecutionState` |

The lock is released when you turn it off, when konsertd stops, and when the controller has
been unreachable for 3 minutes.

**Linux and polkit:** logind lets any session block *idle*, but blocking *sleep* from a
non-console session (SSH, a systemd user service, which is how konsertd usually runs)
requires admin authorization. konsertd then falls back to an idle-only lock and the
footer shows "idle only". To allow full sleep blocking for the user konsertd runs as, add
`/etc/polkit-1/rules.d/50-konsert-inhibit.rules`:

```javascript
polkit.addRule(function (action, subject) {
  if (action.id == "org.freedesktop.login1.inhibit-block-sleep" &&
      subject.user == "YOUR_USER") {
    return polkit.Result.YES;
  }
});
```

## 7. Installing the PWA

Open the controller's HTTPS URL on a phone or desktop browser and use **Install app** /
**Add to Home Screen**. The service worker caches only the app shell; `/api`, `/ws` and
`/mcp` always go to the network, so a stale cache never serves fleet data. After a controller
upgrade the PWA shows a "new version available" prompt.

## 8. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Browser gets `403` or the live socket won't connect | The page's origin isn't the controller's `Host` or `public_url`. Set `public_url` to the URL you browse to, or add `--allowed-origin`. |
| Worker logs `401` and exits | Missing or wrong fleet token. |
| Worker logs "retry later" (`1013`) | Another konsertd with the same identity is connected; it takes over when that one stops. |
| Worker stops with "removed from fleet" | The machine was removed in the UI; re-run konsertd to add it back. |
| `trust_tailscale_headers` has no effect | The controller isn't on a loopback listener, or the request didn't come through Tailscale Serve. |
| Keep Awake shows "idle only" on a Linux host | polkit refused the sleep inhibitor; see the polkit rule in section 6. |
| Version badge on a machine | That konsertd's version differs from the controller's; upgrade the worker. |
