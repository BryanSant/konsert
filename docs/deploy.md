# Deploying Konsert

This guide covers a production-style homelab deployment:

- the **controller** (`konsert`) runs as a service on one always-on host, listening on loopback only;
- **Tailscale Serve** (or another HTTPS reverse proxy) puts it on your tailnet at a real
  `https://<host>.<tailnet>.ts.net` URL, so browsers and workers use HTTPS/WSS;
- **workers** (`konsertd`) run on every machine that should execute agents and connect
  *outbound* to the controller over WSS. No inbound ports are needed on worker hosts.

Templates referenced below live in [`deploy/`](../deploy).

> **Releases:** every release tag requires the manual
> [pre-release walkthrough](walkthrough.md) against real agent CLIs with no API key (see
> "Releasing" in the [README](../README.md#releasing)). It doubles as an acceptance check
> after upgrading your own deployment.

## 1. Signing in

Nobody copies a token by hand. Every browser, phone, CLI and MCP client signs in as its own
**device** with its own credential, and **Settings → Devices** lists them (name, kind, last
used, where from) with Rename and Revoke. Revoking takes effect at once: the device's
requests fail and its open sessions are closed. Expiry slides: a device that is used at
least every 90 days (Settings → *Keep devices signed in while used every N days*) never has
to sign in again; one left unused that long is signed out.

- **First browser.** `konsert serve` prints one line (and a QR code when it runs in a
  terminal): `Sign in: https://hub:8420/pair/7KQ2-M9TX (10 min)`. Open it or scan it. Codes
  are single use, expire after 10 minutes, are rate limited, and appear in the audit log only
  by their short id (`pc-…`). The link uses `public_url` when set, otherwise this machine's
  LAN address (a loopback-only listener is marked "loopback").
- **More browsers and phones.** In a signed-in browser, **Settings → Devices → Add a device**
  (also in ⌘K) shows a QR code, the link and the code, and the new device appears in the list
  as soon as it signs in. On the hub's machine, `konsert pair` prints a fresh link any time
  (for a systemd service: `sudo -u konsert konsert pair --url http://127.0.0.1:8420
  --tokens-file /var/lib/konsert/konsert-tokens.yaml`; with `journalctl -u konsert` you also
  see the startup link).
- **The `konsert` CLI.** On the hub's machine it needs nothing: it finds the local hub and
  its tokens file on its own. Elsewhere, `konsert login https://hub:8420` prints a link and a
  code; approve it in a signed-in browser and the CLI saves its own credential (mode 0600) in
  your user config directory (`~/.config/konsert/credentials.yaml` on Linux). `konsert
  logout` revokes it. `--token` / `KONSERT_TOKEN` and `--url` / `KONSERT_URL` override.
- **MCP clients.** **Settings → Devices → Connect an MCP client** creates a credential that
  only works on `/mcp` (it cannot change where the LLM API key goes) and shows a ready-to-run
  `claude mcp add --transport http …` command and a JSON snippet for other clients.
- **Recovery key.** For emergencies and automation the hub keeps a recovery key in
  `konsert-tokens.yaml` next to the database (mode 0600, `--tokens-file` to move it). It is
  never printed. It works as `Authorization: Bearer <key>` and on the login page under
  *Use recovery key* (which signs that browser in as a new device). Replace it with
  `konsert token rotate recovery` on the hub's machine; signed-in devices are unaffected.

Machines do not share a token either: each konsertd has its own credential (section 5),
which the hub issues when it installs konsertd itself or when the machine joins with a
one-time code.

The orchestrator's LLM API key (optional; agent subscriptions don't need one) is stored
encrypted in the database. The master key is `konsert-secret.key` next to the database
(mode 0600, created on first start; `secret_key_file` / `KONSERT_SECRET_KEY_FILE` to move
it), or supply it directly as base64 in `KONSERT_SECRET_KEY`. Back the key file up together
with the database: without it a saved API key cannot be decrypted and must be re-entered.

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
ANTHROPIC_API_KEY=...
```

The recovery key is generated into `/var/lib/konsert/konsert-tokens.yaml` on first start.
Sign in with the link the service logs (`journalctl -u konsert | grep 'Sign in'`).

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
URL: it is the allowed browser origin, the URL `konsertd join` commands point at, and the
URL agents on other machines use for the hub's per-chat MCP tools. Machines added over SSH
never need to reach it: the hub connects to them.

### Identity headers (`trust_tailscale_headers`)

Tailscale Serve adds `Tailscale-User-Login` (plus `-Name` / `-Profile-Pic`) to every request
from a tailnet user. With `trust_tailscale_headers: true` the controller treats that header
as an authenticated browser session, so tailnet users open the PWA without pairing.

The header is honored **only from loopback peers**, which is where Tailscale Serve connects
from. Keep `listen_addr` on `127.0.0.1`; on a non-loopback listener anyone who can reach the
port directly could otherwise forge the header, and the controller logs a warning at startup.

Never enable `trust_tailscale_headers` behind a proxy other than Tailscale Serve.

### Workers over WSS

Machines added over SSH use the SSH transport (section 5) and need no route to the hub.
Machines the hub cannot SSH into join with a code and connect out:

```bash
konsertd join https://<host>.<tailnet>.ts.net ABCD-EFGH   # code from Add machine → Manual
```

They authenticate with their own machine credential (the identity header is not used
for `/ws/worker`) and reconnect with backoff if the controller restarts.

### Verification checklist

```bash
URL=https://<host>.<tailnet>.ts.net
# From another tailnet node: the identity header authenticates the browser API.
curl -s $URL/api/auth/me                 # {"authenticated":true,"user":"you@example.com",...}
# The header is ignored when it doesn't come from Tailscale Serve.
curl -s -H 'Tailscale-User-Login: x' http://<host-ip>:8420/api/auth/me   # unreachable (loopback bind)
# Fleet status sees your workers.
konsert login $URL && konsert status
```

In a browser, open the URL: the PWA loads without a login prompt, the footer shows the
connected hosts, and DevTools → Application → Service Workers shows `ngsw-worker.js` active.

## 4. Other reverse proxies (Caddy, Nginx)

Any HTTPS proxy works if it forwards WebSocket upgrades and the original `Host` header. The
controller checks a browser's `Origin` against the request `Host` and `public_url`, so either
pass `Host` through unchanged or set `public_url` to the external URL. Leave
`trust_tailscale_headers` **off**; browsers sign in with a pairing link, and the session
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

A worker reaches the hub one of three ways (Fleet shows it as the machine's transport):

- **ssh**: **Add machine → SSH** installs (or upgrades) konsertd over your SSH access and
  starts it without a controller URL. konsertd then serves the hub on a local socket,
  `~/.konsert/run/<worker id>.sock` (AF_UNIX on every OS, in a directory only you can open),
  and the hub attaches to it over SSH with **its own key**, never yours:
  `konsert-hub-ssh-key` next to the database (0600). Bootstrap adds that key to the
  machine's `~/.ssh/authorized_keys` (for members of Administrators on Windows,
  `%ProgramData%\ssh\administrators_authorized_keys`, with the ACL sshd requires),
  restricted to a forced command:

  ```text
  command="/home/me/.local/bin/konsertd attach --allow=<worker id>",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding ssh-ed25519 AAAA... konsert-hub:<hub id>
  ```

  `konsertd attach` only bridges the SSH session to the socket. SSH is the auth: no machine
  credential, and the hub's port never has to be reachable from the machine (keep it on
  127.0.0.1 behind Tailscale Serve). The hub keeps one `ssh` per machine (keepalives,
  pinned host keys from `known_hosts`, `BatchMode`), re-dials with backoff when the link
  drops, and the daemon re-sends its live sessions, so agents survive dropped links and
  hub restarts. Removing the machine drops its id from that line (the line goes with the
  last id); konsertd stops.
- **local**: **Add machine → This machine** runs `konsertd install` on the hub's machine;
  the hub attaches to the same local socket directly, without SSH or a credential.
- **outbound**: any other machine joins with a one-time code and connects out to the hub
  with its own machine credential. **Add machine → Manual** shows one command,
  e.g. `konsertd join https://konsert-hub.example-tailnet.ts.net ABCD-EFGH`. The code is
  single use, expires after 15 minutes and is rate-limited. `join` trades it for the
  machine's credential, saves it in `~/.konsert/credentials/` (readable only by you) and
  runs `konsertd install` with the same flags (`--foreground` runs it in the terminal
  instead). The tab shows the machine appear once it connects.

A credential authenticates only its own machine. **Fleet → Details → Revoke credential**
disconnects a machine at once; it can join again with a new code. `konsert token rotate
machines` (or **Settings → Security**) revokes the credentials of every machine that joined
by hand (`--machine <sid>` for one) and lists them as needing a new code. konsertd without
a credential exits with a pointer to `konsertd join`, unless the hub runs `--no-auth`.

```yaml

```yaml
# /etc/konsert/konsertd.yaml
controller_url: wss://konsert-hub.example-tailnet.ts.net/ws/worker
name: bocabox
max_concurrent: 2
# sandbox: true          # opt-in: no YOLO/skip-permissions flags, shell driver refused
# allowed_roots: [/home/me/src]
```

Check a host with `konsertd doctor`.

`controller_url` (or `--controller`, `KONSERT_CONTROLLER_URL`) is only for outbound
machines; without it konsertd serves the local socket.

konsertd keeps its stable worker ID, machine ID, credential and status outbox in
`~/.konsert` (override with `KONSERT_STATE_DIR`), so a re-run never registers a duplicate
machine.

Agents run as the user konsertd runs as, with that user's CLI logins (`claude`, `grok`,
...), so run workers as your normal user rather than a system account.

### `konsertd install` (recommended)

`konsertd install` registers konsertd to start automatically **as the current user** and
starts it now. It takes the same flags as `konsertd run`:

```bash
konsertd join https://konsert-hub.example-tailnet.ts.net ABCD-EFGH   # join + install
konsertd install --controller wss://konsert-hub.example-tailnet.ts.net/ws/worker   # reinstall
konsertd status        # installed? running? where are the config and logs?
konsertd uninstall     # stop it and remove the unit/agent/task and its config
```

The flags (`--controller`, `--name`, `--sandbox`, `--tag`, `--allowed-roots`, ...) are
written to a per-user config file that only you can read (mode 0600; on Windows an ACL
granting only you and SYSTEM), and the daemon is started with `--config <that file>`. The
machine credential stays in `~/.konsert`, never in the config, unit, plist or task
definition. `--config file.yaml` merges an existing config into the installed one.
`--dry-run` prints what would be installed. Re-running `install` updates the instance in
place.

A `--name` other than the hostname installs a separate instance (`konsertd-<name>`), so
several daemons (say, a normal and a `--sandbox` one) can run side by side; pass the same
`--name` to `status` and `uninstall`.

| OS | What `install` creates | Config | Logs |
|---|---|---|---|
| Linux | systemd user unit `~/.config/systemd/user/konsertd.service`, `systemctl --user enable` + restart; `loginctl enable-linger` so it runs while you are logged out (printed as a `sudo` command if polkit refuses; skip with `--no-linger`) | `~/.config/konsert/konsertd.yaml` | `journalctl --user -u konsertd` |
| macOS | LaunchAgent `~/Library/LaunchAgents/dev.konsert.konsertd.plist` (`RunAtLoad`, `KeepAlive`), loaded with `launchctl bootstrap gui/$UID` | `~/Library/Application Support/konsert/konsertd.yaml` | `~/Library/Logs/konsert/konsertd.log` |
| Windows | Scheduled Task `\konsertd`: at your logon, not elevated, no console window, no time limit, restarted within a minute if it exits | `%LOCALAPPDATA%\konsert\konsertd.yaml` | `%LOCALAPPDATA%\konsert\logs\konsertd.log` |

The unit and the LaunchAgent record your current `PATH`, so agent CLIs installed under
Homebrew, `~/.local/bin`, nvm and so on are found; re-run `install` after adding a new
location. Install from the binary's permanent location: the definition runs that exact
file. On Linux the user's systemd instance is required; in containers without one,
`install` says so (use the Quadlet below or your own supervisor).

**Windows details.** The task runs `conhost.exe --headless konsertd.exe run ...`: conhost
is a GUI-subsystem program, so starting it shows nothing, and in headless mode it gives
konsertd a console without a window. git and agent CLIs started by konsertd inherit that
hidden console instead of opening windows of their own. konsertd stops cleanly when the
task is ended. A logon task only runs while you are logged on to Windows (it starts at
your next logon if you install it over SSH with nobody logged on).

To run konsertd at boot without a logon, as a dedicated account, install a native
service from an elevated ("Run as administrator") terminal:

```powershell
konsertd.exe install --service --user .\builder --controller wss://hub/ws/worker
# prompts for builder's password (or pipe it with --password-stdin), grants builder
# "Log on as a service", sets restart-on-failure and starts the service. Join as builder
# first (konsertd join ... --foreground, then Ctrl-C) so its ~/.konsert has a credential.
konsertd.exe status --service
konsertd.exe uninstall --service   # also revokes "Log on as a service" if install granted it
```

The service's config and logs are in `%ProgramData%\konsert` (config readable only by
SYSTEM, Administrators and the service account). Agents use that account's CLI logins, so
log in to `claude`, `codex`, ... as that account first. konsertd answers the service
manager's Stop and Shutdown requests by stopping its agent sessions and releasing the
wake lock.

### Manual setup

The templates below are an alternative to `konsertd install`, e.g. for a system-wide unit
or a custom setup.

#### Linux: systemd

Install [`deploy/systemd/konsertd.service`](../deploy/systemd/konsertd.service). To run it as
your user, either add `User=you` to the unit or install it as a user unit:

```bash
mkdir -p ~/.config/systemd/user && cp deploy/systemd/konsertd.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now konsertd
loginctl enable-linger "$USER"     # keep it running while logged out
```

#### macOS: launchd

Copy [`deploy/launchd/dev.konsert.konsertd.plist`](../deploy/launchd/dev.konsert.konsertd.plist)
to `~/Library/LaunchAgents/`, adjust the paths (point `--config` at a file in your home
directory; join first so `~/.konsert` holds the machine credential), then:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.konsert.konsertd.plist
launchctl kickstart -k gui/$(id -u)/dev.konsert.konsertd
```

#### Windows

Run `konsertd.exe join https://... <code> --foreground` from a terminal (later
`konsertd.exe run --controller wss://...`; add `--log-file` to log to a file). ConPTY and Job Objects are used automatically.

### Containers: Podman Quadlet

[`deploy/quadlet/konsertd.container`](../deploy/quadlet/konsertd.container) runs the
`ghcr.io/bryansant/konsertd` image with `~/src` mounted at `/workspace`:

```bash
mkdir -p ~/.config/containers/systemd && cp deploy/quadlet/konsertd.container ~/.config/containers/systemd/
systemctl --user daemon-reload && systemctl --user start konsertd
```

The image has no agent CLIs logged in; install and authenticate them in a derived image or
mount their config directories.

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
| Worker exits with "no machine credential" | It never joined: run `konsertd join <hub-url> <code>` with a code from Add machine → Manual. |
| Worker logs "credential rejected" or "revoked" | Its credential was revoked or rotated; join again with a new code (the running daemon picks it up). |
| Worker logs "retry later" (`1013`) | Another konsertd with the same identity is connected; it takes over when that one stops. |
| Worker stops with "removed from fleet" | The machine was removed in the UI; re-run konsertd to add it back. |
| `trust_tailscale_headers` has no effect | The controller isn't on a loopback listener, or the request didn't come through Tailscale Serve. |
| Keep Awake shows "idle only" on a Linux host | polkit refused the sleep inhibitor; see the polkit rule in section 6. |
| Version badge on a machine | That konsertd's version differs from the controller's; upgrade the worker. |
