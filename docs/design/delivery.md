# Review and Delivery Modes (KON-H50YW)

The Work board is private between the user and their agents:

- **Review** means the agent finished work with git changes and is asking for sign-off.
- **Request changes** sends the note back to the agent (steering a running session or starting a follow-up session in the same checkout); the agent keeps working on the same branch.
- **Approve** accepts the work and runs the repository's configured delivery mode.

## Delivery Modes

| Mode | Value | Behavior |
|---|---|---|
| **Direct** | `direct` | Approve merges the task branch into the default branch on the owning daemon (using the configured merge style) and pushes if a remote exists (local-only merge when there is no remote). Merge conflicts abort cleanly and send the task back to the agent with a Request-changes note listing the conflicting files. |
| **PR/MR + merge on approve** | `pr_merge` | When the task enters Review, the hub pushes the branch and opens a PR/MR (title from the task, body with the agent's summary and acceptance list) so CI/bots can run before sign-off. Approve merges the PR/MR through the forge with `WhenChecksPass=true` (falling back to an allowed merge method if the chosen one is disallowed by the repository). |
| **Approve opens PR/MR** | `pr_open` | Nothing is pushed before Approve. Approve pushes the branch (to the user's fork when upstream `CanPush` is false) and opens the PR/MR; the task shows **Delivered: PR open** and the hub tracks PR state periodically (`GetPR`) until merged or closed. |
| **Keep branch only** | `keep_branch` | Approve marks the work accepted and keeps the task branch as-is. |

For modes with an open PR/MR (`pr_merge`, `pr_open`):
- Without a forge adapter or credential for the host, the hub pushes the branch and shows a Compare URL so the user can open the PR/MR by hand.
- The hub polls `ReviewComments` on open PRs/MRs and delivers new change-request comments to the agent as a Request-changes note, deduplicated persistently by comment ID (`review_seen_comments`).

## Defaults & Overrides

- **Default delivery mode**: `pr_merge` when the repository has a remote; `direct` (local-only) when there is no remote.
- **Merge style**: `squash` (default), `merge`, or `rebase`.
- **Override hierarchy**: Repo override (`PATCH /api/repos/{id}`, in Projects & repos) → Project override (`PATCH /api/projects/{id}`, in Projects & repos) → Global default (`PUT /api/settings`, in Settings → Delivery Defaults) → Automatic default.

## Review Panel & Auditability

- `GET /api/tasks/{id}/delivery` computes the delivery preview so the Work review panel always states what Approve will do (for example, `Approve → squash-merge into main and push`, `Approve → merge PR #12 (squash) when checks pass`, or `Approve → push to fork and open a PR against upstream`).
- All review decisions and delivery outcomes are recorded on the task timeline (`review` and `delivery` events, preserved by retention) and in the fleet audit log (`task.review`, `task.deliver`).
