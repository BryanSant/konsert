# Review and Delivery Modes (SYR-H50YW)

The Work board is private between the user and their agents:

- **Review** means the agent finished work with git changes and is asking for sign-off.
- **Request changes** sends the note back to the agent (steering a running session or starting a follow-up session in the same checkout); the agent keeps working on the same branch.
- **Approve** accepts the work and runs the repository's configured delivery mode.

## Delivery Modes

| Mode | Value | Behavior |
|---|---|---|
| **Direct** | `direct` | Approve merges the task branch into the default branch on the owning daemon (using the configured merge style) inside an ephemeral detached worktree and pushes if a remote exists (local-only merge when there is no remote). Merge conflicts abort cleanly in the temp worktree, remove it, and send the task back to the agent with a Request-changes note listing the conflicting files. |
| **PR/MR + merge on approve** | `pr_merge` | When the task enters Review, the hub pushes the branch and opens a PR/MR (title from the task, body with the agent's summary and acceptance list) so CI/bots can run before sign-off. Approve merges the PR/MR through the forge with `WhenChecksPass=true` (falling back to an allowed merge method if the chosen one is disallowed by the repository). |
| **Approve opens PR/MR** | `pr_open` | Nothing is pushed before Approve. Approve pushes the branch (to the user's fork when upstream `CanPush` is false) and opens the PR/MR; the task shows **Delivered: PR open** and the hub tracks PR state periodically (`GetPR`) until merged or closed. |
| **Keep branch only** | `keep_branch` | Approve marks the work accepted and keeps the task branch as-is. |

For modes with an open PR/MR (`pr_merge`, `pr_open`):
- Without a forge adapter or credential for the host, the hub pushes the branch and shows a Compare URL so the user can open the PR/MR by hand.
- The hub polls `ReviewComments` on open PRs/MRs and delivers new change-request comments to the agent as a Request-changes note, deduplicated persistently by comment ID (`review_seen_comments`).

## Checkout Safety & Git Identity Guarantees

Direct delivery and plain-git helpers never disturb the user's own checkout:

- **Isolated temp worktree**: Every direct merge (`squash`, `merge`, or `rebase`) runs inside an ephemeral detached worktree (`git worktree add --detach`) under `/var/tmp` (or `os.TempDir()` on Windows) and always removes it (`git worktree remove --force`) on exit, including on conflict or error.
- **Direct merge with a remote**: Fetches `<remote>/<target>`, merges/rebases in the temp worktree, and pushes `HEAD:refs/heads/<target>` without `--force` (on a non-fast-forward rejection, re-fetches and retries once before reporting an error). Afterward, local `refs/heads/<target>` is updated with `git update-ref` (guarded by the old SHA) only if `<target>` is not checked out in any worktree; if it is checked out (for example, in the user's primary checkout), Syre leaves it untouched and reports `"pushed to <remote>/<target>; your local <target> is behind — pull when ready"`.
- **Direct merge without a remote (local-only)**: Merges in the temp worktree based on local `<target>`. If `<target>` is not checked out anywhere, advances `refs/heads/<target>` with `git update-ref <new> <old>`. If `<target>` is checked out in a clean worktree, fast-forwards it in place with `git merge --ff-only`. If that worktree has uncommitted changes, Syre does not touch it: it leaves the merged commit on `syre/merged/<task-sid>` and reports `"<target> is checked out with uncommitted changes; the merge is ready on branch syre/merged/<sid> — fast-forward <target> when you're ready"`.
- **No auto-commit outside Syre task worktrees**: Auto-committing dirty changes is permitted only inside a verified Syre-managed linked worktree (`<repoRoot>/.syre-worktrees/<taskID>`). If a task ran directly in the user's own checkout, Approve never commits or pushes automatically and reports that the changes must be committed manually.
- **No invented commit identity**: Commits use the repository/user git configuration (`user.name` and `user.email`). If either is unset on the owning machine, Syre refuses to commit and instructs the user to configure `user.name` and `user.email` on that machine.

## Defaults & Overrides

- **Default delivery mode**: `pr_merge` when the repository has a remote; `direct` (local-only) when there is no remote.
- **Merge style**: `squash` (default), `merge`, or `rebase`.
- **Override hierarchy**: Repo override (`PATCH /api/repos/{id}`, in Projects & repos) → Project override (`PATCH /api/projects/{id}`, in Projects & repos) → Global default (`PUT /api/settings`, in Settings → Delivery Defaults) → Automatic default.

## Review Panel & Auditability

- `GET /api/tasks/{id}/delivery` computes the delivery preview so the Work review panel always states what Approve will do (for example, `Approve → squash-merge into main and push`, `Approve → merge PR #12 (squash) when checks pass`, or `Approve → push to fork and open a PR against upstream`).
- All review decisions and delivery outcomes are recorded on the task timeline (`review` and `delivery` events, preserved by retention) and in the fleet audit log (`task.review`, `task.deliver`).
