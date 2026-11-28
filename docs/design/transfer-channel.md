# Fleet transfer channel (design)

Status: proposal for review (SYR-5H78D, under SYR-PT1ZS). Nothing here is built yet.

## Problem

Moving a repo bundle or a worktree between machines (task migration, SYR-M7HZ5) goes
through the hub today. When the hub is a laptop on hotel Wi-Fi that hop is slow or
impossible. Control stays hub-and-spoke; bulk data should be able to go **directly between
two daemons**, arranged by the hub, with keys Syre manages, and through the hub when the
two can't reach each other.

## Decision

**Use the system sshd with a forced command, the way the hub already reaches machines**
(`internal/hubkey`): each daemon has its own SSH key; a receiving machine's
authorized_keys holds one line per peer daemon, restricted to
`syred receive --from=<peer worker id>`; every transfer is authorized by a **one-time grant
signed by the hub**. Machines whose sshd can't be reached (no Remote Login on macOS, no
OpenSSH Server on Windows, outbound-only joins) **fall back to the hub relay**.

A built-in `x/crypto/ssh` endpoint is not built now. It stays the option if relays turn out
to be common in practice (it would work without sshd, at the cost of a listening port and
our own SSH server code).

### Options compared

| | System sshd + forced command | Built-in Go SSH endpoint (on demand) |
|---|---|---|
| Needs on the receiver | sshd running (Linux usually; macOS Remote Login off by default; Windows OpenSSH Server optional) | nothing |
| Network | port 22 as today | a new listening port, opened per transfer (firewalls, NAT) |
| Code we own on the attack surface | the forced command (`syred receive`), small | an SSH server, auth, channel handling |
| Already proven in Syre | yes: the hub's attach key (`hubkey`, `sshutil.AttachArgs`) | no |
| Key placement | authorized_keys line per peer (like the hub's) | in-process allow list |
| Works for machines joined by code | only if sshd is reachable | yes, if the port is reachable |

The forced-command route reuses hardened, audited infrastructure and adds no listener;
the machines that most need direct transfer (Linux boxes, servers added over SSH) already
run sshd. The relay covers the rest.

## Keys

- **Daemon keys.** Each syred makes an ed25519 key pair at install (or on first need) in
  its state dir. The private key never leaves the machine. The public key goes to the hub
  in `worker.hello` (fact `transfer_pubkey`).
- **Peer lines.** When the hub first arranges a transfer from A to B, it asks B's syred to
  add A's line, in the same managed block as the hub's:
  `command="<syred> receive --from=<A id>",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding ssh-ed25519 … syre-peer:<A id>@<hub id>`.
  Removing A from the fleet removes its lines everywhere (and B's syred drops lines for
  peers the hub no longer lists, on hello).
- **Hub signing key.** The hub signs grants with its own ed25519 key; daemons pin the hub's
  public key at join (they already pin the hub's identity) and verify every grant.

## A transfer

1. The hub decides a direct transfer (A to B), records it, and issues a **grant**:
   `{transfer_id, from, to, kind (bundle | worktree), dest (a staging name, not a path),
   max_bytes, not_after (≤ 10 min), hub signature}`.
2. B is told to expect it (`transfer.expect` with the grant); A gets the grant and B's
   address (its SSH target, or Tailscale/LAN address from facts).
3. **Probe and send.** A runs ssh (the same hardened options as the hub's attach:
   `IdentitiesOnly`, no agent, no forwarding, pinned host keys) with its daemon key to B.
   The forced command `syred receive --from=A` reads the grant from stdin first, checks the
   signature, `from`, `to`, expiry and that the transfer id is unused, then streams at most
   `max_bytes` into B's staging dir under its state dir. Anything else is refused.
4. If the connection fails within a few seconds, or the grant is refused, A **relays through
   the hub** (the bytes ride the worker link, chunked, with the same transfer id).
5. Both ends report the result; the hub audits issuance, path (direct / relay), size, time
   and outcome. B moves the staged data into place only when the hub says the transfer
   completed and only into the destination B itself owns (a worktree it provisions).

## Threat model

| Threat | Mitigation |
|---|---|
| A compromised daemon pushes data to a peer on its own | B accepts only with a fresh hub grant naming A and B; the forced command does nothing else (no shell, no pty, no forwarding) |
| A stolen daemon key | Useful only with a valid grant, which only the hub issues; one-time ids; the key is revoked by removing the machine |
| Replay of a grant | Single use (B records transfer ids), short expiry |
| Path traversal / overwriting files | B writes only into its own staging dir under a name B derives; never a path from the sender |
| Disk filling | `max_bytes` in the grant, B's free-space check before accepting |
| A compromised hub | Can already run work on every machine; grants add no new power. Out of scope, as for the attach key |
| Eavesdropping | SSH encryption; relay rides the TLS worker link |
| Host impersonation | Host keys pinned as for the hub's attach; first use takes them from the hub's record of each machine |

## Reachability

The hub picks direct when it has an address for B that A should reach: the SSH target it
installed B over, or a Tailscale/LAN address in B's facts on the same tailnet/subnet as A.
It does not probe ahead of time; the transfer's own connection is the probe, with a short
timeout and the relay as the fallback. Results feed placement later (prefer pairs that
connect directly).

## Questions for review

1. Is sshd-only for direct transfers (relay otherwise) acceptable for the first cut, with
   the built-in endpoint deferred?
2. Should peer lines be added on first transfer (as above) or for every pair at join?
3. Is a 10-minute grant expiry and a staging-then-move flow right for worktrees, or should
   B unpack straight into a fresh worktree?

## Follow-up tickets

SYR-DRQAZ-fleet-ssh-keys (daemon keys, peer lines, grants) and SYR-M32P9-transfer-path
(probe, direct / relay, audit) are the implementation once this is agreed.
