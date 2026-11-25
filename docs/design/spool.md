# Surviving hub sleep: the outbound spool (SYR-VZR9F)

Laptop hubs sleep, and every machine link drops with them. Before the spool, syred
discarded what it produced while disconnected (except final task statuses), so the
transcript, task events, usage and vitals of the gap were lost, and the hub's stale-task
timeout could mark tasks lost right after it woke, before any link re-attached. Now:

* syred **appends every hub-bound message that matters to an on-disk log before it is
  sent**, and replays what the hub has not applied on the next connection, in order;
* the hub **applies each record at most once** and acknowledges what it applied;
* after a **hub wake**, nothing is treated as lost until the links had a chance to come back.

Code: `internal/worker/spool` (the log), `internal/worker/daemon_spool.go` (what is spooled,
the sender), `internal/controller/hub_spool.go` (ingestion), `internal/controller/hub_wake.go`
(wake grace), `internal/store/sqlite/spool.go` (applied seqs, late vitals).

## The log

One spool per daemon identity: `<state dir>/spool/<worker id>/` (`~/.syre` or
`SYRE_STATE_DIR`).

* **Segments** `NNNNNNNNNNNNNNNNNNNN.seg` (named after their first seq), append-only, sealed at
  4 MB. A record is `len u32 | crc32c u32 | seq u64 | written-at i64 (unix ns) | class u8 |
  keylen u16 | key | data`, where data is the encoded envelope and the CRC covers everything
  after it. On open every segment is scanned: a torn or corrupt record ends the segment and the
  file is truncated there (a crash mid-write loses at most the record being written).
* **`spool.json`**: the spool's **epoch** (random; a wiped state directory gets a new one), the
  **hub id** (`hub_id`) its unacked records were produced for (empty until the daemon learns a
  hub's id), the highest seq the hub acknowledged, and a **seq reservation**. Seqs are reserved
  4096 at a time and the reservation is fsynced before any of them is used, so after a crash the
  daemon never reuses a seq it may already have sent (the next seq jumps past the reservation).
* **fsync**: batched every 100 ms; task statuses and agent questions (elicitations) are
  fsynced as soon as they are written.
* **Acks** are cumulative. Segments whose records are all acknowledged are deleted (the active
  segment once it holds 1 MB, so steady traffic does not create a file per second).

### What is spooled

| Spooled (class) | Not spooled |
|---|---|
| agent chat events: the transcript, usage, plan, tool calls (`chat`); elicitations (`question`) | raw PTY bytes: each task's output also goes to a bounded on-disk scrollback instead (below) |
| task events, including usage and quota reports (`task_event`) | request results (`git.*.result`, `fs.list.result`, ports replies...): meaningless on a new connection |
| task statuses; terminal ones fsynced at once (`status`, `final_status`) — this replaced the old status outbox | agent capabilities, quotas, machine facts, wake lock state: current state, re-sent on every connection |
| external-session reports and terminal operator commands (`report`) | per-chat MCP relay traffic (request/response) |
| session activity, shells and sub-agents included (`activity`, collapse key = the session) | |
| heartbeat vitals, with their sample time (`vitals`) | |

Vitals are sampled for the daemon's whole lifetime, connected or not (every 10 s while a
session runs, otherwise every minute), so the gap has vitals too.

**Terminal scrollback.** Each PTY task's output is written to
`<state dir>/scrollback/<worker id>/<task id>.log` (two halves of 512 KiB, rotated), next to the
in-memory ring buffer. A `pty.subscribe` for a task that is no longer running (it ended while
the hub slept, or syred restarted) is answered with a snapshot from that file, so a
re-attached terminal redraws. Files are pruned after 14 days and removed with the machine.

**Activity collapses.** Activity records carry their session as collapse key: a record
superseded by a newer one with the same key is skipped when sent, so after an outage the hub
gets each session's latest state, not every thinking/working tick (the hub keeps activity in
memory only). The activity snapshot syred sends after every hello goes through the spool too,
behind the replay.

## Protocol

```
syred                                       hub
  worker.hello {…, spool: {epoch, first_seq, last_seq}, active_chats}  →
                              ←  worker.spool.resume {hub_id, epoch, applied_seq}
  {seq: applied+1, type: …}  …  {seq: last_seq, …}   (replay)          →
  worker.spool.caught_up {seq: last_seq at resume, epoch?}             →
  {seq: …} …                                          (live)            →
                                  ←  worker.spool.ack {seq}   (≥ 1/s while records arrive)
  worker.spool.status {bytes, records, oldest_unsent_at, replay…}  (not spooled, for Fleet)
```

* Spooled messages carry `seq` in the envelope. syred's writer sends control messages
  that are not spooled first, then streams the spool from the resume point, then live PTY data.
  Live records are appended to the spool and reached by the same cursor, so the replay and live
  traffic are **one ordered stream**: nothing produced during a replay overtakes it.
* **Hub identity binding (SYR-3R7J7):** the hub includes its persistent `hub_id` (from the
  `settings` table; a fresh or reset database generates a new UUIDv7) in `worker.spool.resume`
  on every transport (outbound, ssh, local socket). When syred learns a `hub_id` that differs
  from the one in `spool.json` it does not replay: it closes its open segment files, renames the spool directory to
  `spool-orphaned-<old hub id>-<timestamp>` (keeping at most 2 archives and
  deleting older ones), starts a fresh epoch bound to the new `hub_id`, sends
  `worker.spool.caught_up` with the new `epoch` (so the hub adopts it before any live records
  arrive), and continues live. Records produced while disconnected from a known hub remain
  tagged with that hub's id. A spool never bound to any hub (a fresh daemon, whose first facts
  and vitals are written before it learns the hub's id) adopts the first hub it meets and
  replays those records to it.
* `worker.spool.caught_up` goes out in stream order once the cursor passes the last seq at
  resume time; records up to it that were never sent (superseded activity, compacted vitals)
  count as applied.
* A resume for another epoch counts as `applied_seq` 0. The same protocol runs on every
  transport (outbound, ssh, local socket) and across daemon restarts: the resume point always
  comes from the hub, never from what was sent.

## Idempotent ingestion (hub)

Key: **(worker id, daemon seq)** within the spool's epoch. Per machine the hub keeps the
highest seq it applied in memory and in SQLite (`worker_spool`), and skips any record at or
below it (a replay of something applied). Records of one machine are applied one at a time,
under a per-machine lock, so an old connection still draining cannot race a new one.

How the applied seq is made durable, per record type:

| Record | Durability of "applied" |
|---|---|
| chat events (transcript, usage, questions) | same SQLite transaction as the `chat_events` row (`store.SpoolMark` guard: a record already applied is refused) |
| task events, task usage reports | same transaction as the `task_events` / `usage_events` row |
| replayed vitals | same transaction as the `machine_vitals` / roll-up rows |
| live vitals | with the 15-second bucket that holds them: the bucket's highest seq is stored in the transaction that writes it (once a minute) |
| task statuses, external sessions, operator commands | stored right after they are applied (a replay must not roll a status back) |
| activity | with the next ack (≤ 1 s); activity is in-memory state, so re-applying it is harmless |

So a replay after a reconnect, or after a hub crash, never duplicates chat events, usage
totals, task events, fleet timeline entries or vitals. The hub acks only seqs it has stored.
One message may append several rows: only its first guarded write claims the seq.

**Late vitals.** A heartbeat sampled more than 30 s ago, or one that is part of the replay, is
stored at its own time and does not touch live heat or the active-task list: in the raw tier
(15 s buckets) while the raw tier still covers that hour (12 h, pruned on hour boundaries, so
it is rolled up with the rest of its hour later); older samples are merged straight into the
hourly roll-up (min / sample-weighted mean / max) and the complete day is recomputed; past the
hourly window (30 days) into the daily roll-up.

**Chats.** worker.hello lists the chats open on the daemon. A chat whose turn still runs there
is no longer failed on reconnect; one whose turn did not survive (syred restarted) is failed
only after the replay delivered every event it had.

## Bounds and compaction

Budget: `spool.max_mb` (default 256) and `spool.max_age_days` (default 14) in syred's YAML.
When an append finds the spool over budget (at most every 5 s), compaction rewrites sealed
segments, oldest first, one level at a time, stopping as soon as it is within budget:

1. vitals → 1-minute means (sample-weighted, `samples` set), stamped with the minute;
2. vitals → 1-hour means;
3. superseded activity dropped;
4. vitals and activity older than the age budget dropped.

Compaction only touches records **never handed to a hub** (newer than the hub's last applied
seq and not sent since), so a merged record can never overlap one the hub already applied.
Transcripts, task events, statuses and questions are **never dropped**: if the spool is still
over budget it reports itself **full** (syred logs it, `syred status` prints a
`SPOOL FULL` line, Fleet shows it on the machine) and stops keeping new vitals and activity only
(counted as dropped). It recovers by itself once the hub acknowledges records.

If the spool cannot be opened or written (disk full, permissions), syred logs it once and
sends messages live without keeping them, as before.

## Hub wake grace

The hub watches its clock (every 5 s, and on every path that acts on silence: the reaper, a
dead link, a lost-task check): a jump of more than 30 s by the wall or the monotonic clock is a
**wake** (sleep, or a stopped process). For **2 minutes, or until a machine that was linked at
the wake re-attaches**, whichever comes first:

* the stale-machine reaper does not mark anything offline (every machine looks stale after a
  sleep);
* lost-task reconciliation and the dead-link orphan policies wait (and a lost-task check on a
  re-attached machine also waits for its replay to finish, so a final status replayed late
  wins);
* real-time deadlines still apply: override expiry and veto escalation run on schedule.

Fleet shows such machines as **RECONNECTING** / "reconnecting after sleep…"
(`reconnecting_after_sleep` in the fleet view).

## Status

* `syred status`: spool size of budget, records not yet acknowledged, oldest unsent record,
  replay progress, `SPOOL FULL` (and the last write error).
* Fleet: the machine's `spool` (as last reported with `worker.spool.status`: every 2 s during a
  replay, otherwise when it changes, at most every 10 s, and every 30 s) shows in the row when
  something is notable (replay in progress, a backlog older than 30 s, full) and in machine
  details.

## Guarantees and limits

* Nothing spooled is lost while the hub sleeps or is unreachable, across daemon restarts, up to
  the budget; beyond it only vitals and activity are compacted or dropped.
* Each record is applied at most once by the hub, and at least once as long as the spool keeps
  it.
* A hub crash (not a sleep) can still lose up to a minute of **live** vitals (the in-memory
  bucket not yet written), as before; it does not duplicate them.
* A daemon machine that crashes (power loss) can lose the records of the last 100 ms that were
  not yet fsynced (statuses and questions excepted).
* Activity is in-memory state on the hub: after an outage the hub gets each session's latest
  state, not its history.
