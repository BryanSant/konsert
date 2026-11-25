# Data retention (KON-CSCWP)

Konsert keeps what people come back for and drops the bulk. Live and recent work keeps
every detail; older finished work is summarised rather than deleted; time series are
rolled up. The hub enforces this itself, in small steps, and shows it in
**Settings → Storage** (database size, a breakdown by kind, the policy in plain words,
editable periods and **Compact now**).

## Policy per table

| Table | Policy |
| --- | --- |
| `tasks`, `chat_sessions` | Kept until the user deletes them (deleting a project, chat or task cascades to its history). |
| `chat_messages` (API-key orchestrator transcripts) | Kept with the chat. |
| `chat_events` (agent chat stream, also the agent chat transcript) | Turns that ended more than **30 days** ago (`retention_detail_days`) keep only the transcript kinds (`store.ChatEventKeepKinds`: `user_message`, `konsert_note`, `message`, `session_ready`, `turn_end`, `error`, `elicitation`, `elicitation_resolved`). Tool calls, reasoning, plans, usage, rate limits, sub-agent activity and status events are dropped. |
| `task_events` | Tasks that finished more than **30 days** ago keep only milestones (`store.TaskEventKeepKinds`: `milestone`, `summary`, `exit`, `error`, `waiting_input`, `control`, `steer`, `review`, `delivery`, `command`, `konsert_worktree`, `git_worktree`). Tool calls, file edits, usage, quota and other telemetry are dropped. |
| `fleet_events` (fleet timeline) | Everything for **90 days** (`retention_fleet_days`), then only human actions and failures (`store.FleetEventKeepSQL`: actor `user`, vetoes, `to_status = failed`) until **365 days** (`retention_fleet_summary_days`), then deleted. |
| `usage_events` | Per call for **30 days** (`retention_usage_days`), then folded into `usage_daily`. |
| `usage_daily` | Daily totals per provider, model and account, kept **365 days** (`retention_usage_daily_days`). Usage views read both tables. |
| `machine_vitals`, `machine_vitals_rollup` | Raw 15-second samples 12 hours, hourly roll-ups 30 days, daily roll-ups 365 days (KON-WY5F2, `RollupVitals`). |
| `audit_log` | 90 days, at most 100,000 rows (`AuditRetention`, pruned inline). |
| `devices`, `pairing_codes`, `machine_join_codes` | Expired codes and revoked devices pruned after a day (`PruneAuth`, join-code creation). |
| Everything else (questions, holds, requests, overrides, vetoes, reviews, settings, memory) | Small; kept until the related work or machine is deleted. |

Periods are edited in Settings → Storage within sane bounds (`store.RetentionBounds`;
summaries never shorter than the detail they summarise) and stored in `settings`.

## What is never compacted

- Anything belonging to a task that is not finished (`completed`, `failed`, `canceled`).
  A task that runs again loses its `compacted_at` and is compacted again later.
- Chats that are busy (`starting`, `working`, `needs_you`) or suspended, held, or waiting
  on an open question or user request.
- **Pinned work.** A Keep toggle (chat header, Command session strip, Work review panel;
  `PUT /api/{tasks,chats}/{id}/keep`) sets `keep`; pinned tasks and chats, and their
  fleet timeline events, are never compacted.

## Markers and the UI

- `tasks.compacted_at`: when the task's verbose events were dropped. The review panel says
  so.
- `chat_sessions.compacted_at` / `compacted_before`: when retention last dropped detail
  and the age cutoff it used. The transcript shows "Detailed events (tool calls,
  reasoning, plans) from before *date* were compacted on *date*. The conversation itself
  is kept." instead of an error or an empty pane.
- `chat_sessions.compacted_seq` / `compact_next_at`: internal cursor. Retention walks a
  chat from `compacted_seq` and finds candidates by an index on `compact_next_at` (the
  oldest event it has not looked at), so it never rescans what it already compacted.

## How it runs

The hub runs a pass three minutes after start and then hourly (`retentionLoop`). A pass
walks the steps `tasks`, `chats`, `fleet`, `usage` (`store.CompactSteps`), each in
batches of at most 500 rows, one short transaction per batch (`CompactHistory`), and
sleeps 20 ms between batches so other writers get the hub's single SQLite connection. A
periodic pass stops after 1,000 batches; the next pass continues. Every batch is atomic
and the next one starts from what the database holds, so stopping anywhere (restart,
shutdown) is safe. The usage roll-up adds a batch to `usage_daily` and deletes it from
`usage_events` in the same transaction, so a call is never counted twice.

Then it returns free pages to the file system with `PRAGMA incremental_vacuum` in steps
of 1,024 pages (databases are created with `auto_vacuum = INCREMENTAL`) and truncates the
WAL. **Compact now** runs an unbounded pass in the background; its progress is at
`GET /api/storage/compact` and it is audit-logged (`storage.compact`; retention changes
are `storage.retention`).

The storage breakdown uses SQLite's `dbstat` (exact page usage, cached five minutes;
`?fresh=1` re-measures), or row counts times typical row sizes when the SQLite build
lacks it.

The breakdown follows what compaction does rather than table names:

- **Conversations**: `chat_messages` (orchestrator transcripts) plus the `chat_events` of
  the transcript kinds compaction keeps (`store.ChatEventKeepKinds`). Agent-chat
  conversations live in `chat_events`, so counting only `chat_messages` would make them
  look tiny.
- **Session detail**: every other `chat_events` row (tool calls with their output,
  reasoning, plans, usage, status and other streamed events): the part compaction drops.

`dbstat` measures pages per table, not per kind, so the `chat_events` space is split
between the two in proportion to `SUM(length(json))` of each kind group (one grouped
scan, done with the cached measurement); row counts are exact.

## Scale

A synthetic database of 2,000 finished agent chats dated 60 days ago (12 turns each, six
tool calls with ~1.4 KB of output per turn: 360,000 events, 415 MB) compacts to 36 MB:
288,000 rows dropped in 584 batches, 95 vacuum steps; about 2 s of database work (plus
the 20 ms yields when the hub paces it), and the longest single write transaction was
19 ms (a vacuum step). Run it with
`KONSERT_RETENTION_SCALE_DIR=/var/tmp go test ./internal/store/sqlite -run TestRetentionScale -v`.
