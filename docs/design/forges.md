# Forge adapters (SYR-QHGWZ)

Syre talks to code forges (GitHub, GitLab, Forgejo/Gitea) directly from the hub over
their HTTP APIs with no CLI dependency. Plain git operations (fetch, branch, commit, push,
local merge) stay on the worker daemon (`syred`) that owns the checkout; the hub only
performs forge API calls (pull/merge requests, checks, auto-merge, review comments, fork
lookup).

## Supported forges

| Forge | Package | API root | Notes |
| --- | --- | --- | --- |
| **GitHub** (`github.com` and GitHub Enterprise Server) | `internal/forge/github` | `https://api.github.com` or `https://<host>/api/v3` | REST v3 for all operations; GraphQL (`/graphql` or `/api/graphql`) only for `enablePullRequestAutoMerge`. |
| **GitLab** (`gitlab.com` and self-hosted) | `internal/forge/gitlab` | `https://<host>/api/v4` | REST v4. Project paths (including nested groups) are URL-encoded (`group%2Fsub%2Frepo`). |
| **Forgejo / Gitea** (including `codeberg.org`) | `internal/forge/forgejo` | `https://<host>/api/v1` | REST v1. Supports Codeberg and self-hosted Forgejo/Gitea instances. |

## Operations (`forge.Client`)

All adapters implement the shared `forge.Client` contract in `internal/forge/forge.go`:

- **`RepoInfo`**: default branch, allowed merge methods (`squash`, `merge`, `rebase`),
  whether the authenticated user can push directly (`CanPush`), and the user's fork if one
  exists (`Fork`).
- **`OpenPR`**: opens a pull/merge request, including cross-repository fork PRs/MRs
  (`owner:branch` on GitHub and Forgejo; source-project MR with `target_project_id` on
  GitLab) and draft status.
- **`FindPR`**: finds an open PR/MR for `head -> base` (or returns `ErrNotFound`).
- **`GetPR`**: reads PR/MR state (`open`, `merged`, `closed`), draft flag, auto-merge
  state, and summarized CI checks (`none`, `pending`, `passed`, `failed`) combining commit
  statuses and check runs (GitHub), head pipeline status (GitLab), or commit statuses
  (Forgejo).
- **`MergePR`**: merges using the requested method. When `WhenChecksPass` is true and
  checks are `pending`, enables the forge's auto-merge (`enablePullRequestAutoMerge` on
  GitHub, `merge_when_pipeline_succeeds` on GitLab, `merge_when_checks_succeed` on
  Forgejo). Maps repository merge-method restrictions to `ErrMethodNotAllowed` and
  conflicts/failing required checks to `ErrNotMergeable`.
- **`ReviewComments`**: returns review feedback newer than `since` (reviews, inline diff
  comments, and general PR/MR comments), marking change requests (`CHANGES_REQUESTED` on
  GitHub, unresolved discussions on GitLab, `REQUEST_CHANGES` on Forgejo). Paginates
  results across pages.
- **`CompareURL`**: builds the web URL to open a PR/MR manually when no credential is
  available.

## Forge detection (`internal/forge/detect.go`)

`Detect(ctx, remoteURL)` parses `https://`, `http://`, `ssh://`, `git://`, and SCP-style
(`git@host:owner/repo.git`) remotes (preserving nested GitLab group paths in `Repo.Owner`)
and resolves the forge kind in order:

1. **Per-host override** (`forges.hosts` in controller YAML config).
2. **Well-known hosts**: `github.com` → GitHub, `gitlab.com` → GitLab, `codeberg.org` →
   Forgejo.
3. **Version-endpoint probing** (with a short timeout, default 3s) for unknown hosts:
   - `GET https://<host>/api/v4/version` → GitLab (HTTP 200 with `"version"` or HTTP 401
     with GitLab headers)
   - `GET https://<host>/api/v1/version` → Forgejo/Gitea (HTTP 200 with `"version"`)
   - `GET https://<host>/api/v3/meta` → GitHub Enterprise (GitHub response headers)
4. Otherwise returns `ErrUnknownForge`.

`DefaultResolver` caches detected `(Kind, APIBase)` per host so unknown hosts are probed
only once, and caches authenticated `Client` instances per host (keyed by token digest so
rotated tokens take effect immediately).

## Credential discovery (`internal/forge/creds.go`)

Most users need not paste a token, while an explicitly stored token always takes
precedence over ambient machine credentials. Credentials for a host are resolved in this
order:

1. **Hub encrypted token store** (`device` from OAuth device-code sign-in, or `pasted`
   personal access token) — wins over discovered credentials because it represents the
   user's deliberate choice for that host.
2. **`git credential fill`** for `protocol=https` and `host=<host>`, invoked read-only
   (Syre never calls `git credential approve` or `reject`) with
   `-c credential.interactive=false`, `GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=never`,
   `GIT_ASKPASS=`, `SSH_ASKPASS=`, and a 5s timeout so headless hub runs never pop up GUI
   prompts, browser sign-ins, or block waiting for interactive input. Uses whatever helper
   the host machine already has (macOS Keychain, Git Credential Manager, libsecret).
3. **Existing CLI login** (read-only lookup; the CLI is not used for API calls; stdin is
   bound to an empty reader so nothing can block on a terminal):
   - GitHub: `gh auth token --hostname <host>` (`GH_PROMPT_DISABLED=1`).
   - GitLab: `glab config get token --host <host>` (`NO_PROMPT=1`), falling back to
     `~/.config/glab-cli/config.yml` (or `$XDG_CONFIG_HOME/glab-cli/config.yml`).

If none of these yield a token, `Client` returns `ErrNoCredential` and delivery falls back
to `CompareURL`.

## OAuth device sign-in (`internal/forge/device.go`)

`DeviceFlowClient` implements RFC 8628 (OAuth 2.0 Device Authorization Grant) for GitHub
(`/login/device/code`, `/login/oauth/access_token`) and GitLab (`/oauth/authorize_device`,
`/oauth/token`). Syre ships a built-in GitHub OAuth App client ID
(`forge.DefaultGitHubClientID`) used for github.com only; client IDs are public, not
secrets. Controller config (`forges.github_client_id`, `forges.gitlab_client_id`) or
environment variables (`SYRE_GITHUB_CLIENT_ID`, `SYRE_GITLAB_CLIENT_ID`) override it and
are required for GitHub Enterprise Server and GitLab. When no client ID applies (or for
Forgejo), device flow reports `ErrDeviceFlowUnavailable` (HTTP 501 on the REST API).

## Storage and security

- **Encryption at rest**: stored tokens (device sign-in or pasted) are serialized and
  sealed with AES-256-GCM via `internal/secrets.Box` (`enc:v1:...`) before being written
  to the hub's `settings` table under keys `forge_token:<host>`.
- **No token exposure**: tokens are never returned by `GET /api/forges`, never sent back
  by device-flow polling, never written to audit logs, and never included in error
  messages or logs. `device_code` stays server-side during device sign-in.
- **Auth and auditing**: all `/api/forges*` endpoints require browser/device auth (not
  MCP-only credentials) and pass through the server's origin/CSRF guard. Storing a token
  (`forge.token.set`), deleting a token (`forge.token.delete`), and completing device
  sign-in (`forge.device.auth`) are recorded in the fleet audit log with host and source
  only.

## Hub REST API

- `GET /api/forges` — lists known/seen/configured forge hosts, their kind, active
  credential source (`git-credential`, `gh`, `glab`, `device`, `pasted`, or `none`), and
  whether device sign-in is configured.
- `PUT /api/forges/{host}/token` — stores a pasted token (`{"token": "...", "kind": "..."}`)
  encrypted at rest.
- `DELETE /api/forges/{host}/token` — removes a stored token.
- `POST /api/forges/{host}/device` — starts an OAuth device flow and returns `id`,
  `user_code`, `verification_uri`, `interval`, and `expires_at`.
- `GET /api/forges/{host}/device/{id}` — polls the flow; on authorization stores the
  encrypted token and returns `{"status": "authorized", "credential_source": "device"}`.
