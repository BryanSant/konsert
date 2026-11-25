# Deploying Syre

This guide covers a production-style homelab deployment:

- the **controller** (`syre`) runs as a service on one always-on host, listening on loopback only;
- **Tailscale Serve** (or another HTTPS reverse proxy) puts it on your tailnet at a real
  `https://<host>.<tailnet>.ts.net` URL, so browsers and workers use HTTPS/WSS;
- **workers** (`syred`) run on every machine that should execute agents and connect
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

- **First browser.** `syre serve` prints one line (and a QR code when it runs in a
  terminal): `Sign in: https://hub:8420/pair/7KQ2-M9TX (10 min)`. Open it or scan it. Codes
  are single use, expire after 10 minutes, are rate limited, and appear in the audit log only
  by their short id (`pc-…`). The link uses `public_url` when set, otherwise this machine's
  LAN address (a loopback-only listener is marked "loopback").
- **More browsers and phones.** In a signed-in browser, **Settings → Devices → Add a device**
  (also in ⌘K) shows a QR code, the link and the code, and the new device appears in the list
  as soon as it signs in. On the hub's machine, `syre pair` prints a fresh link any time
  (for a systemd service: `sudo -u syre syre pair --url http://127.0.0.1:8420
  --tokens-file /var/lib/syre/syre-tokens.yaml`; with `journalctl -u syre` you also
  see the startup link).
- **The `syre` CLI.** On the hub's machine it needs nothing: it finds the local hub and
  its tokens file on its own. Elsewhere, `syre login https://hub:8420` prints a link and a
  code; approve it in a signed-in browser and the CLI saves its own credential (mode 0600) in
  your user config directory (`~/.config/syre/credentials.yaml` on Linux). `syre
  logout` revokes it. `--token` / `SYRE_TOKEN` and `--url` / `SYRE_URL` override.
- **MCP clients.** **Settings → Devices → Connect an MCP client** creates a credential that
  only works on `/mcp` (it cannot change where the LLM API key goes) and shows a ready-to-run
  `claude mcp add --transport http …` command and a JSON snippet for other clients.
- **Recovery key.** For emergencies and automation the hub keeps a recovery key in
  `syre-tokens.yaml` next to the database (mode 0600, `--tokens-file` to move it). It is
  never printed. It works as `Authorization: Bearer <key>` and on the login page under
  *Use recovery key* (which signs that browser in as a new device). Replace it with
  `syre token rotate recovery` on the hub's machine; signed-in devices are unaffected.

Machines do not share a token either: each syred has its own credential (section 5),
which the hub issues when it installs syred itself or when the machine joins with a
one-time code.

The commander's LLM API key (optional; agent subscriptions don't need one) is stored
encrypted in the database. The master key is `syre-secret.key` next to the database
(mode 0600, created on first start; `secret_key_file` / `SYRE_SECRET_KEY_FILE` to move
it), or supply it directly as base64 in `SYRE_SECRET_KEY`. Back the key file up together
with the database: without it a saved API key cannot be decrypted and must be re-entered.

Authentication is never silently off: `--no-auth` is for isolated local development and
binds to `127.0.0.1` by default.

## 2. Controller as a systemd service (Linux)

```bash
sudo useradd --system --home-dir /var/lib/syre --create-home --shell /usr/sbin/nologin syre
sudo install -m 0755 syre /usr/local/bin/syre
sudo install -d -m 0750 -o root -g syre /etc/syre
```

`/etc/syre/syre.yaml`:

```yaml
listen_addr: 127.0.0.1:8420        # loopback only; Tailscale Serve / the proxy fronts it
db_path: /var/lib/syre/syre.db
public_url: https://syre-hub.example-tailnet.ts.net
trust_tailscale_headers: true      # only with Tailscale Serve (section 3)
commander:
  provider: anthropic              # openai | anthropic | gemini
```

`/etc/syre/syre.env` (mode 0640, group `syre`) holds secrets:

```bash
ANTHROPIC_API_KEY=...
```

The recovery key is generated into `/var/lib/syre/syre-tokens.yaml` on first start.
Sign in with the link the service logs (`journalctl -u syre | grep 'Sign in'`).

Install [`deploy/systemd/syre.service`](../deploy/systemd/syre.service) to
`/etc/systemd/system/`, then:

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now syre
journalctl -u syre -f
```

The unit is sandboxed (`ProtectSystem=strict`, `NoNewPrivileges`, ...) and may only write to
`/var/lib/syre`. SSH machine bootstrap reads and updates the service user's `~/.ssh`, so
keep the `syre` user's home at `/var/lib/syre` (as created above).

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
URL: it is the allowed browser origin and the URL `syred join` commands point at. Machines
added over SSH never need to reach it (the hub connects to them), and agents on every
machine reach their chat's Syre tools through their own syred (section 5).

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
syred join https://<host>.<tailnet>.ts.net ABCD-EFGH   # code from Add machine → Manual
```

They authenticate with their own machine credential (the identity header is not used
for `/ws/worker`) and reconnect on their own: with backoff (1 s doubling to 30 s, forever)
when the controller restarts or the network drops, at once after a wake from sleep or a
network change, and within ~45 s of a link that went silent (both sides ping when quiet).
`syred status` shows the link state, the last error and the next attempt.

### Verification checklist

```bash
URL=https://<host>.<tailnet>.ts.net
# From another tailnet node: the identity header authenticates the browser API.
curl -s $URL/api/auth/me                 # {"authenticated":true,"user":"you@example.com",...}
# The header is ignored when it doesn't come from Tailscale Serve.
curl -s -H 'Tailscale-User-Login: x' http://<host-ip>:8420/api/auth/me   # unreachable (loopback bind)
# Fleet status sees your workers.
syre login $URL && syre status
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
syre.example.com {
	reverse_proxy 127.0.0.1:8420
}
```

Nginx:

```nginx
server {
    listen 443 ssl;
    server_name syre.example.com;
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

- **ssh**: **Add machine → SSH** installs (or upgrades) syred over your SSH access and
  starts it without a controller URL. syred then serves the hub on a local socket,
  `~/.syre/run/<worker id>.sock` (AF_UNIX on every OS, in a directory only you can open),
  and the hub attaches to it over SSH with **its own key**, never yours:
  `syre-hub-ssh-key` next to the database (0600). Bootstrap adds that key to the
  machine's `~/.ssh/authorized_keys` (for members of Administrators on Windows,
  `%ProgramData%\ssh\administrators_authorized_keys`, with the ACL sshd requires),
  restricted to a forced command:

  ```text
  command="/home/me/.local/bin/syred attach --allow=<worker id>",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding ssh-ed25519 AAAA... syre-hub:<hub id>
  ```

  `syred attach` only bridges the SSH session to the socket. SSH is the auth: no machine
  credential, and the hub's port never has to be reachable from the machine (keep it on
  127.0.0.1 behind Tailscale Serve). The hub keeps one `ssh` per machine (keepalives,
  pinned host keys from `known_hosts`, `BatchMode`), re-dials with backoff when the link
  drops (at once after the hub's machine wakes or its network changes, unless the link
  already re-dialed after the change; a link silent for 45 s counts as dropped), and the daemon re-sends its live sessions, so agents survive
  dropped links and hub restarts. Removing the machine drops its id from that line (the line goes with the
  last id); syred stops.
- **local**: **Add machine → This machine** runs `syred install` on the hub's machine;
  the hub attaches to the same local socket directly, without SSH or a credential.
- **outbound**: any other machine joins with a one-time code and connects out to the hub
  with its own machine credential. **Add machine → Manual** shows one command,
  e.g. `syred join https://syre-hub.example-tailnet.ts.net ABCD-EFGH`. The code is
  single use, expires after 15 minutes and is rate-limited. `join` trades it for the
  machine's credential, saves it in `~/.syre/credentials/` (readable only by you) and
  runs `syred install` with the same flags (`--foreground` runs it in the terminal
  instead). The tab shows the machine appear once it connects.

**Agents' Syre tools.** Whatever the transport, agent chats reach their per-chat tools
(`ask_user`, and the fleet tools for the commander) through their own syred: it
serves them on `http://127.0.0.1:<port>/mcp/chat/<chat id>` (an ephemeral loopback port)
and relays each request to the hub over the link above. No machine needs a route to the
hub's port for its agents, and the hub can stay on 127.0.0.1. Each chat's token reaches
only its own chat, and only through the link of the machine the chat runs on. If the link
drops mid-call the agent gets an error and its retry goes through once the link is back
(a call made while it is down waits up to 20 s for it). Details:
[docs/design/agent-chat.md](design/agent-chat.md#per-chat-mcp-relay-syr-dtn92).

A credential authenticates only its own machine. **Fleet → Details → Revoke credential**
disconnects a machine at once; it can join again with a new code. `syre token rotate
machines` (or **Settings → Security**) revokes the credentials of every machine that joined
by hand (`--machine <sid>` for one) and lists them as needing a new code. syred without
a credential exits with a pointer to `syred join`, unless the hub runs `--no-auth`.

```yaml

```yaml
# /etc/syre/syred.yaml
controller_url: wss://syre-hub.example-tailnet.ts.net/ws/worker
name: bocabox
max_concurrent: 2
# sandbox: true          # opt-in: no YOLO/skip-permissions flags, shell driver refused
# allowed_roots: [/home/me/src]
# spool:                 # what syred keeps for the hub while it is unreachable
#   max_mb: 256          # default 256
#   max_age_days: 14     # default 14
```

Check a host with `syred doctor`.

`controller_url` (or `--controller`, `SYRE_CONTROLLER_URL`) is only for outbound
machines; without it syred serves the local socket.

syred keeps its stable worker ID, machine ID, credential, outbound spool and terminal
scrollback in `~/.syre` (override with `SYRE_STATE_DIR`), so a re-run never registers a
duplicate machine.

**When the hub sleeps or is unreachable** (a laptop hub, a network outage), syred keeps
working and keeps everything that matters on disk: agent transcripts and questions, task
events and statuses, usage, session reports and machine vitals go to its spool
(`~/.syre/spool/<worker id>/`) before they are sent, and are replayed in order when the
link comes back; the hub applies each record exactly once. Raw terminal output is not spooled;
each terminal's last 1 MB is kept on disk so it redraws when you re-open it. The spool is
bounded (`spool.max_mb`, default 256 MB, and `spool.max_age_days`, default 14): over budget,
vitals are compacted first (into 1-minute, then hourly averages), then activity; transcripts,
task events, statuses and questions are never dropped. If it is still full, `syred status`
prints `SPOOL FULL` and Fleet shows it on the machine. After the hub itself wakes up, it waits
up to 2 minutes for machines to re-attach before marking anything offline or lost (Fleet:
"reconnecting after sleep…"). Details: [docs/design/spool.md](design/spool.md). The worker ID file is keyed by name (`worker-id-<name>-<isolation>`, name
defaulting to the hostname), so hosts sharing a home directory (a container and its
host) each keep their own identity.

Agents run as the user syred runs as, with that user's CLI logins (`claude`, `grok`,
...), so run workers as your normal user rather than a system account.

### `syred install` (recommended)

`syred install` registers syred to start automatically **as the current user** and
starts it now. It takes the same flags as `syred run`:

```bash
syred join https://syre-hub.example-tailnet.ts.net ABCD-EFGH   # join + install
syred install --controller wss://syre-hub.example-tailnet.ts.net/ws/worker   # reinstall
syred status        # installed? running? linked to the hub? where are the config and logs?
syred uninstall     # stop it and remove the unit/agent/task and its config
```

**Removing a machine** in the hub uninstalls syred from it, so the OS service cannot
start it again. A connected syred gets close code 4001 and uninstalls itself (its
service, the hub's key grant, its credential); the hub also runs
`syred uninstall --worker <id>` itself, over your SSH access for a machine it added
over SSH (retried for 30 days while the host is unreachable) and directly for its own
machine, and records the result in the audit log ("Uninstall syred"). A machine
joined by hand that was offline uninstalls itself when it next connects. The agent hooks
in `~/.claude/settings.json` go only with the last syred installed on the account:
hosts sharing a home directory share them. If a removal is interrupted, syred finishes
it at its next start.

The flags (`--controller`, `--name`, `--sandbox`, `--tag`, `--allowed-roots`, ...) are
written to a per-user config file that only you can read (mode 0600; on Windows an ACL
granting only you and SYSTEM), and the daemon is started with `--config <that file>`. The
machine credential stays in `~/.syre`, never in the config, unit, plist or task
definition. `--config file.yaml` merges an existing config into the installed one.
`--dry-run` prints what would be installed. Re-running `install` updates the instance in
place.

A `--name` other than the hostname installs a separate instance (`syred-<name>`), so
several daemons (say, a normal and a `--sandbox` one) can run side by side; pass the same
`--name` to `status` and `uninstall`.

| OS | What `install` creates | Config | Logs |
|---|---|---|---|
| Linux | systemd user unit `~/.config/systemd/user/syred.service`, `systemctl --user enable` + restart; `loginctl enable-linger` so it runs while you are logged out (printed as a `sudo` command if polkit refuses; skip with `--no-linger`) | `~/.config/syre/syred.yaml` | `journalctl --user -u syred` |
| macOS | LaunchAgent `~/Library/LaunchAgents/dev.syre.syred.plist` (`RunAtLoad`, `KeepAlive`), loaded with `launchctl bootstrap gui/$UID` | `~/Library/Application Support/syre/syred.yaml` | `~/Library/Logs/syre/syred.log` |
| Windows | Scheduled Task `\syred`: at your logon, not elevated, no console window, no time limit, restarted within a minute if it exits | `%LOCALAPPDATA%\syre\syred.yaml` | `%LOCALAPPDATA%\syre\logs\syred.log` |

The unit and the LaunchAgent record your current `PATH`, so agent CLIs installed under
Homebrew, `~/.local/bin`, nvm and so on are found; re-run `install` after adding a new
location. Install from the binary's permanent location: the definition runs that exact
file. On Linux the user's systemd instance is required; in containers without one,
`install` says so (use the Quadlet below or your own supervisor).

**Windows details.** The task runs `conhost.exe --headless syred.exe run ...`: conhost
is a GUI-subsystem program, so starting it shows nothing, and in headless mode it gives
syred a console without a window. git and agent CLIs started by syred inherit that
hidden console instead of opening windows of their own. syred stops cleanly when the
task is ended. A logon task only runs while you are logged on to Windows (it starts at
your next logon if you install it over SSH with nobody logged on).

To run syred at boot without a logon, as a dedicated account, install a native
service. Installing needs administrator rights: in an ordinary terminal at the desktop,
syred asks for the account password, then for approval (UAC), and shows the elevated
install's output in the same terminal. Over SSH, an administrator account already has an
elevated session (OpenSSH), so it just works; without administrator rights, or with
`--no-elevate`, or with no desktop to show the prompt on (SSH, services, CI), syred
never waits for a prompt: it says what to run instead (`sudo syred ...` when Windows
sudo is enabled in inline mode, otherwise an elevated "Run as administrator" terminal).

```powershell
syred.exe install --service --user .\builder --controller wss://hub/ws/worker
# prompts for builder's password (or pipe it with --password-stdin), grants builder
# "Log on as a service", sets restart-on-failure and starts the service. Join as builder
# first (syred join ... --foreground, then Ctrl-C) so its ~/.syre has a credential.
syred.exe status --service
syred.exe uninstall --service   # no elevation needed; see below
```

Removing the service needs no elevation: install grants the service account stop and
delete on its own service (and nothing else: it cannot start, reconfigure or re-permission
it), so `syred uninstall --service` run as that account, the daemon removing itself
when the hub removes the machine, and the hub's uninstall over SSH all work without
administrator rights. Revoking "Log on as a service" (which install granted, if the account
lacked it) does need them: without them the right is kept and a later elevated
`syred uninstall --service` revokes it.

The service's config and logs are in `%ProgramData%\syre` (config readable only by
SYSTEM, Administrators and the service account, which may also delete it). Agents use that account's CLI logins, so
log in to `claude`, `codex`, ... as that account first. syred answers the service
manager's Stop and Shutdown requests by stopping its agent sessions and releasing the
wake lock.

### Manual setup

The templates below are an alternative to `syred install`, e.g. for a system-wide unit
or a custom setup.

#### Linux: systemd

Install [`deploy/systemd/syred.service`](../deploy/systemd/syred.service). To run it as
your user, either add `User=you` to the unit or install it as a user unit:

```bash
mkdir -p ~/.config/systemd/user && cp deploy/systemd/syred.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now syred
loginctl enable-linger "$USER"     # keep it running while logged out
```

#### macOS: launchd

Copy [`deploy/launchd/dev.syre.syred.plist`](../deploy/launchd/dev.syre.syred.plist)
to `~/Library/LaunchAgents/`, adjust the paths (point `--config` at a file in your home
directory; join first so `~/.syre` holds the machine credential), then:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.syre.syred.plist
launchctl kickstart -k gui/$(id -u)/dev.syre.syred
```

#### Windows

Run `syred.exe join https://... <code> --foreground` from a terminal (later
`syred.exe run --controller wss://...`; add `--log-file` to log to a file). ConPTY and Job Objects are used automatically.

### Containers: Podman Quadlet

[`deploy/quadlet/syred.container`](../deploy/quadlet/syred.container) runs the
`ghcr.io/bryansant/syred` image with `~/src` mounted at `/workspace`:

```bash
mkdir -p ~/.config/containers/systemd && cp deploy/quadlet/syred.container ~/.config/containers/systemd/
systemctl --user daemon-reload && systemctl --user start syred
```

The image has no agent CLIs logged in; install and authenticate them in a derived image or
mount their config directories.

## 6. Host Keep Awake

The footer's Keep Awake control can also stop worker hosts from sleeping during long runs.
syred holds the lock with:

| OS | Mechanism |
|---|---|
| macOS | `caffeinate -i` (tied to the syred process) |
| Linux | `systemd-inhibit` (logind) |
| Windows | `SetThreadExecutionState` |

The lock is released when you turn it off, when syred stops, and when the controller has
been unreachable for 3 minutes.

**Linux and polkit:** logind lets any session block *idle*, but blocking *sleep* from a
non-console session (SSH, a systemd user service, which is how syred usually runs)
requires admin authorization. syred then falls back to an idle-only lock and the
footer shows "idle only". To allow full sleep blocking for the user syred runs as, add
`/etc/polkit-1/rules.d/50-syre-inhibit.rules`:

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

## 8. Data retention

The hub bounds its own database; there is nothing to schedule. Running work and work
finished in the last 30 days keeps every detail. After that, finished sessions keep their
record, summary, outcome and transcript, and drop tool calls, reasoning and other
streamed events (tasks keep their milestones, errors and your interventions). The fleet
timeline keeps 90 days in full, then your actions and failures for a year. Token usage is
kept per call for 30 days, then as daily totals for a year. Machine vitals keep 12 hours
of samples, 30 days hourly and a year daily; the audit log keeps 90 days. Sessions marked
**Keep** are never compacted, and deleting a chat, task, project or machine deletes its
history.

Compaction runs in small batches a few minutes after start and hourly, then returns the
space to the file system (incremental vacuum). **Settings → Storage** shows the database
size and what uses it, lets you change the periods, and has **Compact now**. Per-table
details: [docs/design/retention.md](design/retention.md).

## 9. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Browser gets `403` or the live socket won't connect | The page's origin isn't the controller's `Host` or `public_url`. Set `public_url` to the URL you browse to, or add `--allowed-origin`. |
| Worker exits with "no machine credential" | It never joined: run `syred join <hub-url> <code>` with a code from Add machine → Manual. |
| Worker logs "credential rejected" or "revoked" | Its credential was revoked or rotated; join again with a new code (the running daemon picks it up). |
| Worker logs "retry later" (`1013`) | Another syred with the same identity is connected; it takes over when that one stops. |
| Worker stops with "removed from fleet" | The machine was removed in the UI; re-run syred to add it back. |
| Worker logs "hub link lost" then "still disconnected …" once a minute | The hub is unreachable (or went silent for 45 s). syred keeps retrying on its own; `syred status` shows the last error and the next attempt. It logs "reconnected" when the link is back. |
| A machine goes offline after ~45 s while its daemon still runs | Nothing crossed the link for 45 s in one direction (a half-open connection, a stopped hub, a dead network path). The daemon redials; its sessions keep running and are re-adopted. |
| `trust_tailscale_headers` has no effect | The controller isn't on a loopback listener, or the request didn't come through Tailscale Serve. |
| Keep Awake shows "idle only" on a Linux host | polkit refused the sleep inhibitor; see the polkit rule in section 6. |
| Version badge on a machine | That syred's version differs from the controller's; upgrade the worker. |
