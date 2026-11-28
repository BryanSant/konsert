# Syre Technical Implementation Plan

This document is the engineering blueprint for **Syre** as specified in [prd.md](./prd.md).

> **Status (2026-11-25).** Sections 1–7 are the original MVP blueprint. Phases 1–4 are built, but the code has moved on: subscription-first agent chat ([design/agent-chat.md](./design/agent-chat.md)), UUIDv7 `id` + short `sid` on every record, a single squashed schema (`internal/store/sqlite/migrations/001_schema.sql`), `encoding/json/v2`, hub-managed daemons. Where sections 1–7 disagree with the code, the code wins. **Section 8 is the forward roadmap** for the three pillars in the PRD: Autopilot, total visibility, and intelligent scheduling.

---

## 1. Toolchain & Dependency Matrix

| Layer | Technology / Library | Notes |
| :--- | :--- | :--- |
| **Language (Backend/Daemon)** | Go 1.24+ (verified on Go 1.27) | Static binaries via `CGO_ENABLED=0` |
| **Database** | `modernc.org/sqlite` | Pure-Go SQLite (no CGO); behind `store.Store` interface for future PostgreSQL 18 |
| **WebSocket** | `github.com/coder/websocket` | Context-native, clean binary/text framing, concurrent-safe |
| **PTY (Unix)** | `github.com/creack/pty` | Linux & macOS PTY allocation + `TIOCSWINSZ` + process groups (`Setpgid: true`) |
| **PTY (Windows)** | `github.com/UserExistsError/conpty` | Windows 10/11 ConPTY + `Resize` + Job Object cleanup |
| **MCP SDK** | `github.com/modelcontextprotocol/go-sdk` | Official Go MCP SDK (stdio + Streamable HTTP) |
| **CLI / Config** | `github.com/spf13/cobra` + `gopkg.in/yaml.v3` | Subcommands (`serve`, `mcp`, `run`, `doctor`, `token`) + YAML/env config |
| **Frontend** | Angular 22+ (Zoneless Signals, `@angular/pwa`) | Standalone components, Signals, `provideZonelessChangeDetection()` |
| **Styling** | Tailwind CSS v4 + CSS Custom Properties | VS Code / Herdr dark theme design tokens |
| **Terminal UI** | `@xterm/xterm`, `@xterm/addon-fit`, `@xterm/addon-webgl`, `@xterm/addon-web-links` | Hardware-accelerated terminal with automatic canvas fallback |
| **Packaging** | `Makefile` + `.goreleaser.yaml` | Cross-compile `linux`, `darwin`, `windows` + embed `web/dist` |

---

## 2. Repository Structure

```text
syre/
├── cmd/
│   ├── syre/                  # Controller CLI & HTTP/WS/MCP server (embeds web/dist)
│   └── syred/                 # Cross-platform worker daemon CLI
├── docs/
│   ├── prd.md                    # Product Requirements Document
│   └── plan.md                   # This implementation plan
├── internal/
│   ├── auth/                     # Fleet bearer token, browser session cookie, Tailscale headers
│   ├── config/                   # Controller & worker YAML + env + flag configuration
│   ├── gitutil/                  # Worktree create/cleanup, base SHA, diff --stat & commit summary
│   ├── protocol/                 # Shared WS JSON control envelopes & binary PTY frame codec
│   ├── store/
│   │   ├── store.go              # Swappable Store interfaces & domain models
│   │   └── sqlite/
│   │       ├── sqlite.go         # modernc.org/sqlite implementation + PRAGMA setup
│   │       ├── migrate.go        # Embedded SQL migration runner
│   │       └── migrations/
│   │           └── 001_init.sql
│   ├── controller/
│   │   ├── hub.go                # Worker registry, browser subscriber fan-out, ref-counted PTY streams
│   │   ├── scheduler.go          # Capability/selector task routing & concurrency accounting
│   │   ├── service.go            # Shared application service (used by REST, MCP, Commander)
│   │   ├── mcp.go                # MCP server (stdio + Streamable HTTP/SSE)
│   │   ├── api.go                # REST + Browser WS + Worker WS HTTP handlers
│   │   └── embed.go              # //go:embed static PWA assets & SPA fallback handler
│   ├── commander/
│   │   ├── backend.go            # CommanderBackend interface & tool-calling loop
│   │   ├── openai.go             # OpenAI-compatible provider (OpenAI, OpenRouter, xAI, Ollama)
│   │   ├── anthropic.go          # Anthropic Messages API provider
│   │   └── gemini.go             # Google Gemini API provider
│   └── worker/
│       ├── daemon.go             # Outbound WS client, reconnect backoff, heartbeat, task re-adoption
│       ├── runner.go             # Task execution lifecycle, worktree hook, telemetry & exit reporting
│       ├── pty.go                # Cross-platform PtyProcess interface
│       ├── pty_unix.go           # creack/pty + PGID signal escalation (build: !windows)
│       ├── pty_windows.go        # conpty + Windows Job Object cleanup (build: windows)
│       ├── ringbuf.go            # Thread-safe 512KB circular buffer
│       └── drivers/
│           ├── driver.go         # AgentDriver interface, PermissionMode, Registry
│           ├── claude.go         # claude CLI (--dangerously-skip-permissions default) + JSONL watcher
│           ├── agy.go            # agy CLI driver + structured status parser
│           ├── grok.go           # grok CLI driver + summary extractor
│           └── mock.go           # Deterministic test driver for CI & UI development
├── web/                          # Angular 22+ Zoneless Signals PWA
│   ├── angular.json
│   ├── package.json
│   └── src/
│       ├── manifest.webmanifest
│       ├── styles.css            # Tailwind v4 + dark theme CSS tokens
│       └── app/
│           ├── core/
│           │   ├── models/       # TypeScript mirrors of protocol/store DTOs
│           │   ├── services/     # AuthService, ApiClient, SyreSocketService
│           │   └── state/        # FleetStore, TabsStore, TelemetryStore, CommanderStore (Signals)
│           ├── layout/           # AppShell, FleetSidebar, TabBar, TelemetryDrawer, MobileHeader
│           └── features/
│               ├── auth/         # LoginViewComponent
│               ├── terminal/     # XtermPaneComponent (Read-Only / Interactive Takeover)
│               └── commander/ # CommanderChatComponent (streaming markdown + tool cards)
├── deploy/
│   ├── systemd/                  # syre.service, syred.service
│   ├── quadlet/                  # syred.container (Podman)
│   └── launchd/                  # dev.syre.syred.plist
├── Makefile
└── .goreleaser.yaml
```

---

## 3. Data Model & SQLite Schema

> **Taxonomy replacement ([design/work.md](./design/work.md), [design/autopilot.md](./design/autopilot.md)).** The Space / Workspace / Repo / Worktree taxonomy is replaced (no migration pre-1.0). The old `spaces`, `workspaces`, and taxonomy `repos`/`worktrees` tables go away (per-machine checkouts and worktrees become daemon-reported facts). In Phases A1–A6, the hub SQLite schema adds: `projects`, `repos` (per-project repo registry), `tickets`, `ticket_edges` (`blocked_by` DAG), `leases`, `attempts`, `artifacts` (index of daemon-held artifacts), `directives`, and `external_refs`. The MVP SQL below is kept for historical reference.

### 3.1 Swappable Storage Interface (`internal/store/store.go`)

```go
package store

import (
	"context"
	"time"
)

type PermissionMode string

const (
	PermissionFullAccess PermissionMode = "full_access" // Default ("YOLO")
	PermissionSandboxed  PermissionMode = "sandboxed"   // Opt-in restriction
)

type Worker struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	HostGroup      string            `json:"host_group"`
	OS             string            `json:"os"`
	Arch           string            `json:"arch"`
	Isolation      string            `json:"isolation"`       // "vm", "container", "host"
	PermissionMode PermissionMode    `json:"permission_mode"` // "full_access" | "sandboxed"
	Agents         []string          `json:"agents"`
	Tags           map[string]string `json:"tags"`
	MaxConcurrent  int               `json:"max_concurrent"`
	Status         string            `json:"status"` // "online", "offline"
	LastSeenAt     time.Time         `json:"last_seen_at"`
}

type Task struct {
	ID             string         `json:"id"`
	WorkerID       string         `json:"worker_id"`
	Agent          string         `json:"agent"`
	Workdir        string         `json:"workdir"`
	EffectiveDir   string         `json:"effective_dir"`
	Branch         string         `json:"branch"`
	Mode           string         `json:"mode"`            // "oneshot" | "interactive"
	PermissionMode PermissionMode `json:"permission_mode"` // "full_access" | "sandboxed"
	OrphanPolicy   string         `json:"orphan_policy"`   // "fail" | "requeue"
	Prompt         string         `json:"prompt"`
	Status         string         `json:"status"` // "queued","assigned","running","waiting_input","completed","failed","canceled"
	Summary        string         `json:"summary"`
	GitSummaryJSON string         `json:"git_summary_json,omitempty"`
	ExitCode       *int           `json:"exit_code,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	StartedAt      *time.Time     `json:"started_at,omitempty"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
}

type TaskEvent struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	Kind      string    `json:"kind"` // "milestone","tool_call","file_edit","control","error","exit"
	Message   string    `json:"message"`
	MetaJSON  string    `json:"meta_json,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ChatSession struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ChatMessage struct {
	ID         int64     `json:"id"`
	SessionID  string    `json:"session_id"`
	Role       string    `json:"role"` // "user","assistant","tool"
	Content    string    `json:"content"`
	ToolJSON   string    `json:"tool_json,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Store interface {
	UpsertWorker(ctx context.Context, w Worker) error
	GetWorker(ctx context.Context, id string) (Worker, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	MarkStaleWorkersOffline(ctx context.Context, cutoff time.Time) ([]string, error)

	CreateTask(ctx context.Context, t Task) error
	UpdateTaskAssignment(ctx context.Context, id, workerID, effectiveDir string) error
	UpdateTaskStatus(ctx context.Context, id, status, summary, gitSummaryJSON string, exitCode *int) error
	GetTask(ctx context.Context, id string) (Task, error)
	ListTasks(ctx context.Context, status, workerID string, limit int) ([]Task, error)

	AppendEvent(ctx context.Context, e TaskEvent) (int64, error)
	ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]TaskEvent, error)

	CreateChatSession(ctx context.Context, s ChatSession) error
	ListChatSessions(ctx context.Context, limit int) ([]ChatSession, error)
	AppendChatMessage(ctx context.Context, m ChatMessage) (int64, error)
	ListChatMessages(ctx context.Context, sessionID string) ([]ChatMessage, error)

	Close() error
}
```

### 3.2 Initial Migration (`internal/store/sqlite/migrations/001_init.sql`)

SQLite startup PRAGMAs:
```sql
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
```
With `db.SetMaxOpenConns(1)` for serialized writes (or split read/write pools if needed later).

```sql
CREATE TABLE IF NOT EXISTS workers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    host_group      TEXT NOT NULL,
    os              TEXT NOT NULL,
    arch            TEXT NOT NULL,
    isolation       TEXT NOT NULL DEFAULT 'host',
    permission_mode TEXT NOT NULL DEFAULT 'full_access',
    agents_json     TEXT NOT NULL,
    tags_json       TEXT NOT NULL,
    max_concurrent  INTEGER NOT NULL DEFAULT 2,
    status          TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id               TEXT PRIMARY KEY,
    worker_id        TEXT REFERENCES workers(id),
    agent            TEXT NOT NULL,
    workdir          TEXT NOT NULL,
    effective_dir    TEXT NOT NULL DEFAULT '',
    branch           TEXT NOT NULL DEFAULT '',
    mode             TEXT NOT NULL DEFAULT 'oneshot',
    permission_mode  TEXT NOT NULL DEFAULT 'full_access',
    orphan_policy    TEXT NOT NULL DEFAULT 'fail',
    prompt           TEXT NOT NULL,
    status           TEXT NOT NULL,
    summary          TEXT NOT NULL DEFAULT '',
    git_summary_json TEXT NOT NULL DEFAULT '',
    exit_code        INTEGER,
    created_at       TEXT NOT NULL,
    started_at       TEXT,
    completed_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_worker ON tasks(worker_id, status);

CREATE TABLE IF NOT EXISTS task_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    message    TEXT NOT NULL,
    meta_json  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, id ASC);

CREATE TABLE IF NOT EXISTS chat_sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    tool_json  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id ASC);
```

---

## 4. Wire Protocol Specification (`internal/protocol`)

Both Worker (`syred` ↔ `syre` at `/ws/worker`) and Browser (PWA ↔ `syre` at `/ws/client`) use a **dual-frame WebSocket protocol**:
- **Text Frames (JSON):** Control plane messages, telemetry events, task dispatch/lifecycle, chat streaming.
- **Binary Frames:** Raw PTY byte streams to eliminate base64 inflation and minimize GC/CPU overhead during noisy builds.

### 4.1 Binary PTY Frame Format

```text
+---------------+----------------------------------+-------------------------+
| FrameType (1B)| TaskID (16B UTF-8, zero-padded) | Raw PTY Bytes (N bytes) |
+---------------+----------------------------------+-------------------------+
  0x01 = pty.snapshot (ring buffer replay on subscribe)
  0x02 = pty.data     (live stdout/stderr chunk)
  0x03 = pty.stdin    (interactive input from browser -> controller -> worker)
```

### 4.2 Control Plane JSON Messages

Envelope: `{"type": "<msg_type>", "id": "<optional_req_id>", "ts": "<rfc3339>", "payload": {...}}`

| Direction | `type` | Key Payload Fields |
| :--- | :--- | :--- |
| Worker → Controller | `worker.hello` | `worker` metadata + `active_tasks: [{task_id, status, started_at}]` (enables restart re-adoption) |
| Worker → Controller | `worker.heartbeat` | `active_task_ids`, `load_avg` |
| Controller → Worker | `task.start` | `task_id`, `agent`, `workdir`, `branch`, `use_worktree`, `prompt`, `mode`, `permission_mode`, `timeout_seconds`, `cols`, `rows` |
| Controller → Worker | `task.cancel` | `task_id` |
| Controller → Worker | `task.steer` | `task_id`, `input_text`, `append_newline` |
| Worker → Controller | `task.status` | `task_id`, `status`, `effective_dir`, `exit_code?`, `summary?`, `git_summary?` |
| Worker → Controller | `task.event` | `task_id`, `kind`, `message`, `meta?` |
| Browser ↔ Controller ↔ Worker | `pty.subscribe` | `task_id` (Controller ref-counts; forwards to Worker on `0 -> 1` transition) |
| Browser ↔ Controller ↔ Worker | `pty.unsubscribe` | `task_id` (Controller forwards to Worker on `1 -> 0` transition) |
| Browser → Controller → Worker | `pty.resize` | `task_id`, `cols`, `rows` |
| Browser → Controller | `chat.send` | `session_id`, `content` |
| Controller → Browser | `chat.delta` / `chat.tool` / `chat.done` | Streaming commander tokens and tool-call execution cards |
| Controller → Browser | `fleet.update` | Incremental worker/task state changes pushed to all connected UI clients |

### 4.3 On-Demand PTY Streaming Sequence

```mermaid
sequenceDiagram
    participant UI as Angular PWA
    participant Hub as syre (Controller)
    participant W as syred (Worker)

    Note over W: Task running; PTY writes to 512KB RingBuffer (stream_active=false)
    UI->>Hub: JSON pty.subscribe {task_id}
    Hub->>Hub: Ref-count task subscribers (0 -> 1)
    Hub->>W: JSON pty.subscribe {task_id}
    W->>W: Snapshot 512KB RingBuffer & set stream_active=true
    W-->>Hub: BINARY [0x01][task_id][snapshot_bytes]
    Hub-->>UI: BINARY [0x01][task_id][snapshot_bytes]
    loop While stream_active == true
        W-->>Hub: BINARY [0x02][task_id][live_chunk]
        Hub-->>UI: BINARY [0x02][task_id][live_chunk]
    end
    Note over UI: User enables "Take Control"
    UI->>Hub: BINARY [0x03][task_id][keystrokes]
    Hub->>W: BINARY [0x03][task_id][keystrokes]
    Note over UI: User closes tab
    UI->>Hub: JSON pty.unsubscribe {task_id}
    Hub->>Hub: Ref-count task subscribers (1 -> 0)
    Hub->>W: JSON pty.unsubscribe {task_id}
    W->>W: Set stream_active=false (zero network traffic)
```

### 4.4 Backpressure & Slow Consumers
- Each browser WebSocket client gets a bounded outbound channel (e.g., 256 frames).
- If a browser's queue fills (e.g., backgrounded mobile Safari), `Hub` drops the slow subscriber and closes its socket rather than blocking `syred` or other viewers; the PWA auto-reconnects and re-subscribes cleanly.

---

## 5. Worker Daemon (`syred`) Architecture

### 5.1 Cross-Platform PTY Abstraction (`internal/worker/pty.go`)
```go
type PtyProcess interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Interrupt() error // SIGINT to PGID (Unix) or Ctrl+C / CTRL_BREAK_EVENT (Windows)
	Terminate() error // SIGTERM -> SIGKILL (Unix) or Job Object Terminate (Windows)
	Wait() (int, error)
	Close() error
}
```
- **Unix (`pty_unix.go`):** Starts command with `SysProcAttr{Setpgid: true}` via `creack/pty.StartWithSize`. Signals target `-pgid` so child build/test processes are cleanly terminated.
- **Windows (`pty_windows.go`):** Allocates ConPTY via `UserExistsError/conpty`, attaches the spawned process to a Windows Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so child processes cannot leak.

### 5.2 Ring Buffer (`internal/worker/ringbuf.go`)
- Fixed 512 KB (`524288` bytes) circular byte buffer protected by `sync.Mutex`.
- `Write(p []byte)` overwrites oldest bytes when full.
- `Snapshot() []byte` returns a contiguous chronological copy for replay on `pty.subscribe`.

### 5.3 Multi-Agent Drivers (`internal/worker/drivers/`)
```go
type LaunchSpec struct {
	Binary string
	Args   []string
	Env    []string
}

type TelemetryEvent struct {
	Kind    string // "milestone", "tool_call", "file_edit", "waiting_input", "summary", "error"
	Message string
	Meta    map[string]any
}

type AgentDriver interface {
	Name() string
	Detect() (path string, ok bool)
	BuildCommand(opts LaunchOptions) LaunchSpec
	WatchTelemetry(ctx context.Context, opts WatchOptions, emit func(TelemetryEvent))
	ExtractSummary(ctx context.Context, opts SummaryOptions) string
}
```
- **`claude` (`claude.go`):**
  - Default (`PermissionFullAccess`): passes `--dangerously-skip-permissions`. Opt-in (`PermissionSandboxed`): omits flag.
  - `WatchTelemetry`: watches `~/.claude/projects/<encoded-effective-dir>/*.jsonl` for newly appended JSONL lines, emitting `tool_call` (e.g., `Bash: go test ./...`), `file_edit` (e.g., `Edit: internal/hub.go`), and capturing the final assistant text block for `ExtractSummary`.
- **`agy` (`agy.go`):**
  - Default (`PermissionFullAccess`): passes full-access/yolo execution flags.
  - `WatchTelemetry`: inspects structured stdout/status markers (ANSI-stripped tee from the PTY stream) to emit milestones and final summary.
- **`grok` (`grok.go`):**
  - Default (`PermissionFullAccess`): passes auto-approve/yolo flags.
  - `WatchTelemetry`: parses ANSI-stripped output transitions for tool execution milestones and extracts the concluding response block.
- **`mock` (`mock.go`):**
  - Enabled in tests or via `--enable-mock-driver`; emits predictable colored ANSI frames, waits for optional stdin in `interactive` mode, and emits structured milestones + summary.

### 5.4 Workdir & Git Worktree Lifecycle (`internal/gitutil`)
1. If worker has `--allowed-roots` configured (opt-in sandboxing), verify `filepath.EvalSymlinks(workdir)` is contained within an allowed root; otherwise allow any existing path.
2. If `branch != ""` and `use_worktree == true` and `workdir` is inside a git repo:
   - Record `base_sha = git rev-parse HEAD`.
   - Create worktree at `<repo_root>/.syre-worktrees/<task_id>` on `branch` (`git worktree add -B <branch>` or checkout existing).
   - Run agent with `effective_dir` set to the worktree path.
3. On completion: compute `git status --short`, `git diff --stat <base_sha>`, and `git log --oneline <base_sha>..HEAD`, serialize into `GitSummaryJSON`, and emit in `task.status`.

---

## 6. Controller (`syre`) Architecture

### 6.1 Shared Application Service (`internal/controller/service.go`)
All three entrypoints—**REST API**, **MCP Server**, and **Built-in Commander Chat**—call the same `FleetService` methods:
- `FleetStatus(ctx)`
- `DispatchTask(ctx, req)`
- `ListTasks(ctx, filter)`
- `GetTaskResult(ctx, taskID, wait, timeout)` (uses an in-memory per-task broadcast channel in `Hub` to unblock immediately on completion without DB polling)
- `SteerTask(ctx, taskID, input, appendNewline)`
- `CancelTask(ctx, taskID)`

### 6.2 Capability-Based Scheduler (`internal/controller/scheduler.go`)
Matches `worker_selector` against online workers where `active_tasks < max_concurrent`:
1. Filter by `worker_id` (if specified), `host_group`, `os`, required `agent` in `worker.Agents`, all requested `tags`, and compatible `permission_mode` (a `sandboxed` worker cannot run `full_access` tasks).
2. Rank matching workers by lowest utilization ratio (`active_tasks / max_concurrent`), tie-breaking by most recent heartbeat.
3. If no worker currently has capacity, leave task in `queued` and re-evaluate whenever a task completes or a worker connects.

Under Autopilot (Phases A1–A7), the scheduler extends this loop to claim dispatchable tickets from the `blocked_by` DAG with atomic SQLite leases, enforce active policy directives (pause, throttle, drain, provider disable), and push debounced board pulses to the Commander on actionable transitions rather than having the Commander poll or block on `GetTaskResult`.

### 6.3 REST & WebSocket Endpoints (`internal/controller/api.go`)
- `GET /api/health` — Public health check.
- `POST /api/auth/login` / `POST /api/auth/logout` / `GET /api/auth/me` — Session auth.
- `GET /api/fleet` — Workers + active tasks snapshot.
- `GET /api/tasks`, `POST /api/tasks`, `GET /api/tasks/{id}`, `POST /api/tasks/{id}/steer`, `POST /api/tasks/{id}/cancel`.
- `GET /api/tasks/{id}/events` — Historical structured telemetry events.
- `GET /api/chat/sessions`, `POST /api/chat/sessions`, `GET /api/chat/sessions/{id}/messages`.
- `GET /ws/worker` — Authenticated worker daemon WebSocket.
- `GET /ws/client` — Authenticated browser PWA multiplexed WebSocket.
- `POST /mcp` / `GET /mcp` — Streamable HTTP/SSE MCP endpoint (bearer-token authenticated).
- `GET /*` — Embedded Angular 22 PWA static assets with SPA `index.html` fallback.

---

## 7. Angular 22+ Zoneless Signals PWA (`web/`)

### 7.1 State Architecture (Pure Signals)
- `provideZonelessChangeDetection()` enabled at bootstrap; no Zone.js overhead.
- **`FleetStore`:** `workers = signal<Worker[]>([])`, `tasks = signal<Task[]>([])`, `groupedFleet = computed(...)` (groups workers by `host_group` with nested active tasks and live status counts).
- **`TabsStore`:** `openTabs = signal<WorkspaceTab[]>([{ id: 'commander', kind: 'commander', pinned: true }])`, `activeTabId = signal<string>('commander')`. Tracks per-tab `interactiveMode: boolean` (always defaults to `false` / Read-Only).
- **`TelemetryStore`:** `eventsByTask = signal<Record<string, TaskEvent[]>>({})` fed by `task.event` WS messages and initial REST hydration.
- **`CommanderStore`:** Manages active chat session, streaming assistant delta buffer, and collapsible tool-call execution cards.

### 7.2 `XtermPaneComponent` Implementation Details
- Mounts `@xterm/xterm` inside `afterNextRender()`, loads `FitAddon`, `WebLinksAddon`, and attempts `WebglAddon` (catching context errors and falling back to default canvas renderer).
- Attaches a `ResizeObserver` on the container element; debounces resize events (`100ms`), calls `fitAddon.fit()`, and sends `pty.resize` to `syre`.
- Subscribes to `SyreSocketService.ptyStream(taskId)`:
  - On `0x01` (`pty.snapshot`): calls `term.reset()` then `term.write(bytes)`.
  - On `0x02` (`pty.data`): calls `term.write(bytes)`.
- `term.onData(data)` checks `interactiveMode()` signal—when `false` (Read-Only), keystrokes are ignored and a subtle toast/hint reminds the user to toggle **"Take Control"**. When `true`, sends binary `0x03` frame.

### 7.3 Responsive & PWA Strategy
- Tailwind CSS v4 breakpoints (`md:` = `768px`):
  - `>= 768px`: 3-pane IDE grid (collapsible left Fleet Explorer `260px`, center flex workspace, collapsible right Telemetry Drawer `320px`).
  - `< 768px`: Single-pane view with compact header (drawer toggle + tab selector + status pill), slide-over Fleet Explorer overlay, bottom-sheet Telemetry Drawer, and mobile quick-key toolbar (`Ctrl+C`, `Esc`, `Tab`, `Enter`) when Interactive mode is on.
- `@angular/service-worker` caches app shell and static assets (`prefetch`), while `/api/*`, `/ws/*`, and `/mcp` bypass cache (`network-only`).

---

## 8. Roadmap: Autopilot, Visibility and Scheduling

Phases 1–4 of the original roadmap (daemon and PTY core; hub, store, scheduler and MCP; PWA; telemetry and packaging) are done, as are the subscription-first chat work, first-run setup, and questions/steering. Autopilot (Phases A1–A7) is built first because it creates the attempt history that placement learns from; visibility and scheduling (Phases 5–11) follow. Each phase is one `ka` epic.

### Phase A1: Work model (tickets, repo registry, leases, DAG; Work board on tickets)

- Hub SQLite schema for `projects`, `repos` (per-project repo registry keyed by remote URL with verify/delivery/merge defaults), `tickets` (ka hierarchy `Project > Initiative > Epic > Ticket`, acceptance items, comments, questions, hints, autonomy settings, statuses), `ticket_edges` (`blocked_by` DAG with cycle rejection), and `leases`.
- Remove `Space` and `Workspace` taxonomy tables (pre-1.0, no migration); keep ad-hoc tasks allowed under a project without a lease or DAG.
- Atomic SQLite lease claim in the scheduler (`status=running`, TTL default 10m), lease renewal on daemon heartbeat, orphan policy on expiry, and attributed lease breaking by user or Commander.
- Per-ticket repo bindings (`1..n`) with one worktree provisioned per binding under `<workdir root>/.syre/tickets/<ticket sid>/<repo name>` (clones at `<workdir root>/.syre/clones/<repo name>`), cleaned up on `done` delivery or after failed retention.
- Agent-facing work tools served by local `syred` over the per-chat MCP relay (`ticket_get`, `ticket_check`, `ticket_comment`, `ticket_followup`, plus existing `ask_user` and `request_user_action`), scoped strictly to the session's own ticket.
- PWA Work board reading tickets (and ad-hoc tasks) with columns mapped from status categories, Initiative/Epic + Expedite swimlanes, and cards showing lease, attempt count, last digest line and blockers.

### Phase A2: Tracker sync (adapter interface; ka adapter via embedded ka library; ka-side engine extraction to pkg/ and external lease authority)

- Common `Tracker` adapter interface (`Kind`, `Pull`, `Push`, `Capabilities`) and `external_refs` table linking Syre tickets to external tracker items.
- Conflict resolution by field ownership (human-owned fields take tracker values, Syre-owned runtime fields take Syre's, `blocked_by` merges as a set) plus per-adapter capability mappings, interval/webhook pulls (default 30s) and debounced pushes.
- `ka`-side prerequisite: extract `internal/engine` (index, query, operations, sync, merge driver) into `pkg/`, and add `authority: <external system>` claims to the ka Store Spec so `ka` clients display Syre leases read-only instead of competing for them.
- `ka` adapter in the hub embedding ka's engine as a library so `syre` acts as one more replica of the ka store; projects with no tracker continue using Syre's tables directly.

### Phase A3: Packets, digests and artifacts (incl. attempts, failure classifier for tasks, test-output parsers, inspect, capping inspect_git_status)

- Dispatch packet compiler on the hub (target 150–300 tokens) combining ticket objective, acceptance items, repo bindings, `verify_cmd`, tool list, and `contracts` pulled from `interface_digest` of done `blocked_by` tickets.
- `attempts` table recording machine, account, agent, model, timings, exit, failure category, verify result, tokens and cost; daemon-side failure classifier for tasks (OOM, timeout, disk full, missing tool, verify, agent error, canceled, plus chat-engine auth/usage_limit/rate_limit).
- Post-run digest extractor on `syred` (target 100–250 tokens) with test-output parsers for common runners (`go test`, jest/vitest, pytest, cargo, gradle, maven, dotnet) and ANSI-stripped tails on failure or `needs_you`.
- Edge artifact store (`diff_stat`, `diff`, `stderr`, `log`, `transcript`, `file`) with hub `artifacts` index, fetched on demand with line/byte caps via `inspect`; replace `inspect_git_status`'s uncapped per-file diffs with capped `inspect`.
- PWA ticket detail view showing attempts and digests.

### Phase A4: Verification gate, self-repair and cross-agent review

- Daemon verification gate: when a worker finishes and the ticket `verify_cmd` is empty, `syred` runs each binding's `verify_cmd` (repo command, else the project command) in that repo's worktree. A ticket-level `verify_cmd` with one binding runs in that worktree; with several bindings it runs once in `<workdir root>/.syre/tickets/<ticket sid>`. All commands must pass. The digest records the failing repo's command and exit, with passed/failed summed and the failure tail labelled with the repo name.
- Self-repair loop: on verification failure with `attempt < retry_limit` (default 2), `syred` sends the compact failure tail back into the same worker session as the next prompt at zero Commander token cost; on exhausted retries, transition to `failed` (or `needs_you` when `escalate_on_blocker`) and push the digest.
- Cross-agent review: when `cross_review` is enabled and verification passes, dispatch a review run on a different agent or model to check the diff against acceptance items; `changes-requested` loops back to the author session like a verify failure, and `approve` proceeds to delivery ([design/delivery.md](./design/delivery.md)).

### Phase A5: Board pulse and Autopilot loop (Play/Pause, decomposition, expand_scope, fleet stop with grace)

- Board pulse emitter on the hub: debounced (default 20s) push on actionable transitions (completions, `needs_you`, exhausted retries, followups awaiting triage, `needs_decomposition`, expiring directives, board idle) via the fleet-event path for built-in Commanders and MCP notification + `pulse` resource for external Commanders.
- Autopilot Play/Pause loop (per project or global, distinct from emergency Pause everything): scheduler dispatches ready tickets in priority/age order, batches `needs_decomposition` or unbound tickets into a pulse for the Commander to decompose via `manage_work`, and turns Play off with a final pulse when idle.
- Blocker routing and scope handling: worker questions route to the Commander first when allowed (with receipts and override window) while destructive/ambiguous ones stay in Needs you; followups are triaged by the Commander and auto-created under the same epic when `expand_scope` is on.
- Graceful fleet/project/machine/session stop with configurable grace period before kill and process-group/job-object cleanup (`syre` + `syred`), plus Play/Pause controls on the PWA Work board.

### Phase A6: Policy directives (timed pause/throttle/drain, provider disable with fallback, usage-limit auto-directives, commander_policy_writes)

- `directives` table and hub expiry timers for typed, attributed directives: `machine.pause`, `machine.throttle` (concurrency, CPU ceiling, `heavy_ok`), `machine.drain`, `provider.disable` (with preferred `fallback`), and `autopilot.pause`.
- Scheduler and daemon enforcement of active directives, including rerouting away from disabled providers and respecting throttle caps.
- Tighten vs loosen rule: tightening directives apply immediately when set by the Commander (attributed "user via Commander" with receipt and one-click undo), governed by `commander_policy_writes` (`tighten` default | `confirm` | `none`); loosening operator policy still goes through `request_override` and approval.
- Automatic `provider.disable` directives (provenance `system`) created until reset time when daemons report usage-limit windows, plus directive badges on machines in the PWA.

### Phase A7: Lean MCP surface and token budgets

- Consolidate the 28 Commander MCP tools into the ~10-tool flat-schema surface (`fleet_status`, `backlog`, `manage_work`, `control`, `set_policy`, `reply`, `inspect`, `ask_user`, `answer_question`, `request_override`, `memory`) for both built-in and external `/mcp` Commanders.
- Group `fleet_status` summaries by capability by default; move settings, fan-out, narration and raw git inspection to the UI or existing tool actions.
- CI token-budget tests (fixed tokenizer approximation) failing the build on regressions: packet <= 300, digest <= 250, pulse <= 120, full Commander tool schema <= 2,500, and 20-machine `fleet_status` summary <= 400.

Existing Phases 5–11 follow; Phase 7 attempts and failure classifier move into Phase A3, and Phase 11 scheduling tools use the lean MCP surface from Phase A7.

### Phase 5: Observed sessions (Visibility I)

Goal: every agent session on a machine shows up, whoever started it.

- `internal/worker/observe/` (interface and registry done, SYR-9BQN9): a `Source` per agent with `Discover(since)` and `Read(session, offset)`, which returns `protocol.AgentChatEvent`s for complete records plus the next offset. A `Registry` polls every 2 s and re-reads a session only when its size, mtime or live status changed. It persists offsets in the state dir, so a restart resumes rather than replays, and skips sessions idle beyond 24 h. It never reports a managed session (a `ManagedSet` of chat/hook session ids, plus any predicate). New sessions wait a 3 s grace before their first report, and a late managed mark retracts. Sources read the session stores agents already write:
  - claude (done, SYR-YANWY): `<config dir>/projects/<encoded cwd>/<session id>.jsonl` for `~/.claude`, every account's `CLAUDE_CONFIG_DIR` and any `~/.claude<N>`. Live status (busy/shell, waiting with what it waits for, idle) and the PID come from the `sessions/<pid>.json` file each running claude writes. A session whose process is gone is done, and one under a syred-managed process is managed. Without such files (older versions), status comes from the records and ages: working goes idle after 10 min without writes, and idle goes done after 30 min. The daemon reports each session as an external session (`source: transcript`, keyed by the claude session id, the id hooks report too). Project dirs are listed every 10 s; between listings only recent transcripts and the ones live pid files point at are stat'ed. Steady-state cost was 0.2 to 0.3% CPU with busy sessions on macOS and Linux, and under 0.1% on Windows.
  - codex: `~/.codex/sessions/**/rollout-*.jsonl`;
  - grok, cursor-agent, copilot, agy: their session/state stores (see [research/agent-cli-capabilities.md](./research/agent-cli-capabilities.md); add what's missing).
  Each source maps records to the normalized `AgentChatEvent` stream the chat engines already use, so the UI renders observed sessions with the same components.
- Hooks stay as a low-latency status signal; the process scan becomes a fallback and gains cursor-agent, copilot and the desktop apps.
- Status from session records (turn started/ended, waiting for permission), not CPU.
- Hub: observed sessions are `tasks` rows with `origin=external` (exists); add `agent_session_id`, account, model, summary, files touched and counts. Transcripts are fetched from the daemon only when a user opens one, cached on the hub for 30 minutes (configurable) and purged; nothing transcript-shaped is persisted.
- PWA: read-only session view; an "All activity" list per machine.
- Exit: start a session by hand in each of the six CLIs on macOS, Linux and Windows; each appears within 5 s with correct status transitions and tool calls. Daemon CPU stays under 1% idle.

### Phase 5a: Human control and attribution

Built right after observed sessions and before placement: it shapes every interaction, and holds are rules placement must respect.
- Cause model on task/session status changes and audit rows (actor, confidence, correlation to request/command).
- Controls: suspend/resume (Unix SIGSTOP/SIGCONT of the process group; Windows thread suspension of the job's processes), machine drain/suspend/stop, fleet pause, veto with reason (stored as commander memory at the work key/project scope).
- `holds` table (scope, cause, created_by, released_at) enforced by the scheduler, retry logic and commander tools; `user_requests` (open requests from agents) linked to matching user actions.
- Shell integration for Syre terminals (OSC 133 prompt/command/exit marks; zsh, bash, PowerShell) → `worker.operator_command` events; command text kept on the daemon with a short TTL, redacted summary to the hub.
- Event delivery: fleet events to the commander chat; notes before a worker session's next prompt; `recent_events` MCP tool; standing operator-protocol instruction in managed sessions.
- PWA: intervention controls, Pause everything, holds and requests visible, "your actions" timeline.
- Later (needs Phase 6): process-death ↔ command correlation, SSH-outside-Syre attribution, per-process kill.
- Exit: user stops nginx in a Syre terminal on host-1 → hold on host-1/nginx, commander and the affected session are told and ask before restarting; when an agent asked the user to restart nginx, the same action resolves its request and the agent continues.

### Phase 5b: Questions and steering

Built alongside 5a (open requests are questions with an action attached).
- `ask_user` schema with required context/consequences/rationale and validation; question-writing guidance in managed sessions.
- `questions` table, queue API, dedupe of re-asks, generous MCP tool timeouts for managed sessions (`MCP_TOOL_TIMEOUT` for claude) with re-ask on timeout.
- Reference enrichment (ka tickets, PRs, files, sids).
- Side-conversation engine: fork the asking session read-only (claude `--resume <id> --fork-session` with plan mode; codex fork), else the commander with a context pack; answer drafts; summary delivered with the answer.
- PWA: question card, queue with keyboard flow, thread with pinned options; mobile inbox and side-conversation sheet with dictation.
- Delegation to the commander with receipts; question quality metrics.
- Exit: an under-specified question is rejected and re-asked properly; the user asks "what is JK-ABC12 and why shorten it?" in a side conversation answered by a fork, then answers from a phone; the agent receives the answer and the summary.

### Phase 6: Processes and load (Visibility II)

- Daemon process sampler (done, SYR-7AK2A: `internal/worker/procs`). It reads `/proc/<pid>/stat` on Linux, `kern.proc.all` plus `proc_pid_rusage` on macOS, and one `NtQuerySystemInformation(SystemProcessInformation)` call on Windows. For each process it records parent, name, start time, cumulative CPU and memory: RSS on Linux, physical footprint on macOS, working set on Windows. CPU% comes from the delta between samples, keyed by PID and start time. It samples every 5 s while work runs and every 30 s idle. On macOS, other users' processes come back without CPU and memory unless syred runs as root. Measured cost per sample at the 5 s interval: 0.1% of one core on macOS (768 processes), 0.22% on Linux (659), 0.016% on Windows (171). The daemon starts it with its first consumer (attribution, SYR-G1JPX).
- Attribution (done, SYR-G1JPX: `procs.Attributor`, `worker/load.go`). The daemon runs the sampler and splits each sample among sessions:
  - each session owns its root process, its descendants and, for managed sessions, what its guardrail container holds (`Guard.Members`: the cgroup scope's `cgroup.procs` on Linux, the job object plus adopted escapees on Windows, the tracked tree on macOS), which catches double forks that left the tree between samples;
  - managed sessions are listed before observed ones;
  - a process keeps its session (by PID and start time) after its parent dies or its session ends, until it exits, and is counted as an orphan.

  Per session: CPU%, RSS now and peak, process count and orphans. The result rides on the heartbeat as `WorkerMetrics.Load`.
- Load split (done, SYR-PSDFB: `procs.SplitLoad`, `worker/useridle_*.go`). Each sample is divided into agent load (attributed sessions, plus syred and its helpers) and other load (everything else), CPU and RSS each, with the five busiest other processes. Processes macOS won't report without root are counted, not guessed. User idle time:
  - macOS: `ioreg` HIDIdleTime;
  - Windows: `GetLastInputInfo`, only when syred runs in the desktop session (not as a session-0 service);
  - Linux: logind IdleHint of the user's seat sessions. This is coarse: 0 until the desktop's own idle timeout passes. Servers with no seat report none.

  Idle is read at most every 30 s. All of it rides on `WorkerMetrics.Load`.
- Store (done, SYR-PEEBE). `metric_samples` keeps per-subject series: `task:`, `chat:`, `observed:<machine>/`, `agent:<machine>`, `other:<machine>`. The hub folds each heartbeat's `WorkerMetrics.Load` into 1-minute buckets, written each minute and at shutdown (a restart merges into the same rows), and rolls them up hourly. Retention is in Settings → Storage: minutes 2 days, hours 30 days (at least 7). Tasks keep their peaks (`peak_rss_mb`, `peak_cpu_pct`). Queries: `GET /api/metrics?subject=…|worker=…&range=1h|12h|24h|3d|1w` and MCP `fleet_status detail=history`. Machine rows in `fleet_status` carry the live load and the policy.
- Exit: a build started by an agent is attributed to it; a build started by hand counts as other load; history survives hub restarts.

### Phase 7: Facts, work keys and outcomes

- Extensible capability probes: built-in list plus probes declared in daemon config (command + version regex, or path exists), GPU detection (NVIDIA `nvidia-smi`, Apple Silicon GPU cores and unified memory, AMD), architecture in the selector, operator labels per machine, machine role (`dedicated` / `interactive` / `control-only`).
- `work_key` on tickets/tasks (derived from project, epic/ticket, repo, kind; overridable by the dispatcher) and requirement fields on dispatch.
- Extend `attempts` and the daemon failure classifier (introduced in Phase A3) with resource peaks from Phase 6 and kernel OOM evidence. (Guardrail sheds are already tagged at the source as `memory_pressure_shed`, with peak and host memory in the digest: [design/guardrails.md](design/guardrails.md).)
- `work_profiles` maintained from attempts per `(work_key, machine class)`.
- Exit: profiles fill in after a few runs; `inspect` returns them.

### Phase 8: Policy and enforcement

- Machine policy (done, SYR-BJDY6). `protocol.MachinePolicy` is stored in `workers.policy_json` and pushed to the daemon in `worker.directives`. Fields:
  - role: `dedicated`, `interactive`, or `control_only` (runs nothing);
  - max sessions and max load average;
  - CPU cores reserved for the user;
  - min free RAM and RAM reserved for the user;
  - quiet hours, in an explicit IANA zone;
  - allowed work classes (`chat`, `task`, `ticket`).

  One pure checker (`internal/policy`) runs twice. The hub runs it at admission (scheduler, ticket runs, pinned dispatch, and opening a chat that isn't already running) and caps slots at max sessions. The daemon runs it again before `task.start` and `agent.chat.open`, using its own hosted count and last vitals. The RAM rules add to the survival reserve, never replace it: admission needs survival reserve + min free + reserved, and the guardrails' watchdog trips at that sum (`Guard.SetUserReserve`; facts report `policy_reserve_mb`). Edits go only through `PUT /api/workers/{id}/policy` (the CLI's `syre policy`) and are audited (`machine.policy`, before and after). MCP credentials never reach REST, so the Commander can read the policy (on each machine in `fleet_status`) but only tightens through `set_policy` directives on top of it, which the user can end. The `interactive` role does nothing yet: it waits for the user-active signal (SYR-PSDFB). A policy editor in the UI is the remaining piece.
- Resource guardrails, not envelopes: done in the guardrails epic (SYR-E6DZ9, [design/guardrails.md](design/guardrails.md)). Survival reserve, one shared worker pool, soft kernel watermarks, a pressure watchdog that sheds the newest session, requeue without spending a retry. No hard per-session caps on any OS.
- Machine policy's user RAM reservation raises the guardrails' floor above the survival reserve.
- Optional rootless Podman tier for Linux work (`hermetic_container` per project): image, mounts for repo and agent login state, soft limits via Podman flags (`--memory-reservation`, swap cap, CPU shares), shed as one session.
- Policy directives over MCP follow the tighten vs loosen rule from Phase A6 (`commander_policy_writes`); full policy edits remain in UI/CLI and are audited.
- Break glass (done, SYR-ADQZX). `request_override` gains scope `policy`, which suspends named machine policy rules on one machine for its duration. The hub's admission honours it, and the daemon gets the lift in `worker.directives` with its end time, so it ignores an expired lift even before the hub's update. Overrides need the user's approval: only a user cause can decide one, and MCP credentials can't reach REST. The exception is `override_self_approve` (Settings, REST only; the MCP settings path doesn't accept it), which records the decision as the Commander's own. Uses are counted when the work they admitted is placed. `policy_blocks` counts each piece of work a rule or directive kept off a machine, once an hour, kept 90 days. `GET /api/policy/report?days=7` sets blocks against the overrides that followed (requested, approved, denied, used, whether they came after the block), shown under Settings → Policy report.
- Container awareness: detect Podman/Docker; per-project opt-in container execution; never required for resource protection.
- Exit: an interactive machine with an active user receives no new heavy work; policy blocks and overrides are reported. (The anti-lockup exit — a memory runaway on bare metal is shed and requeued while ssh stays responsive — belongs to the guardrails epic.)

### Phase 9: Placement

- Capacity model: headroom = available RAM − survival reserve − policy reservation − expected peaks of running sessions (estimates, never caps) − observed other load; recent sheds down-weight a machine.
- Scorer: hard filters (requirements, policy, logins) then a weighted score (headroom fit for the expected profile, locality, account quota, placement rules, spread). Unknown work rotates across eligible machines weighted by headroom.
- Repo locality: daemons report checkouts (repo remote URL + path + branch + last fetch) found under configured roots; `checkouts` table (many machines per repo).
- Subscription-aware choice of machine and account pair; spread across same-vendor accounts.
- `placement_rules` (scope, prefer/require/avoid, target machine or class, provenance, evidence, expiry) and `placement_log` (candidates, rejections, scores, decision, decider).
- MCP/REST: `explain_placement` (dry run), placement explanation on every task.
- Exit: scenario tests: Windows-only work never lands elsewhere; repo work prefers machines with a checkout until they're saturated; exhausted account is avoided until reset; rules override scores but never policy.

### Phase 10: Retry and migration

- Retry policy by failure category (bigger machine for OOM/disk/timeout; another account for usage limit; never for auth). `memory_pressure_shed` is requeued without spending a retry; only repeated sheds of a work key suggest a larger class.
- Migration: stop on A; transfer via push to the shared remote when available, otherwise a `git bundle` sent directly A → B over the fleet transfer channel (Phase 10a), falling back to a hub relay; start on B with a handoff note (previous summary, failure, files changed).
- Learned rules: a migration that succeeds after a classified failure writes a `learned` placement rule for the work key with the evidence.
- Exit: an ML-style job that OOMs on a small machine is migrated to a larger one, succeeds, and the next dispatch of that work key goes straight to the larger class.

### Phase 10a: Fleet transfer channel

- Per-daemon fleet SSH keypair (generated locally, public key registered with the hub, private key never leaves the machine).
- Receiver: syred runs a minimal SSH endpoint (Go `x/crypto/ssh`) on demand, or uses the system sshd with a forced-command `authorized_keys` entry; the hub authorizes one sender key for one transfer with an expiry. Design spike decides between the two (Windows has OpenSSH server optional; a built-in endpoint avoids depending on it).
- Reachability probe (Tailscale/LAN addresses reported in facts); transfer path selection git remote → direct → hub relay; audit of each transfer.
- Exit: a repo bundle moves bocabox → bocamacmini directly, and via the hub when direct isn't reachable; keys are scoped and expire.

### Phase 11: Commander scheduling

- Expose scheduling capabilities through the lean MCP surface from Phase A7 rather than adding separate tools: `fleet_status` (headroom, managed/observed sessions, quotas, policies), `inspect` (explain placement, work profiles, attempts), `set_policy` (placement rules and directives), `manage_work` (requeue/migrate), `control`, and `request_override`; push actionable transitions via the board pulse (Phase A5).
- Ship the Syre scheduler skill (installed for the commander agent): reading the snapshot, trusting vs overriding the built-in scheduler, responding to failure categories, migrating, recording rules.
- Evaluation suite: scripted fleets (mock engine with synthetic resource profiles) and scenarios the commander must handle; track decisions over versions.
- Exit: the commander handles the OOM → migrate → pin scenario and a usage-limit → switch-account scenario end to end, and its learned rules survive a hub restart and a fresh commander context.

### Cross-cutting

- Deep links in the PWA by sid (`/<sid>` short links and `/<mode>/<sid>`) — done.
- Each phase adds a hands-on walkthrough to [walkthrough.md](./walkthrough.md) on macOS, Linux and Windows.

---

## 9. Testing & Verification Strategy

1. **Go Unit & Race Tests (`go test -race ./...`):**
   - `ringbuf`: wrap-around boundaries, concurrent write + snapshot.
   - `protocol`: binary frame encode/decode fuzz & edge cases.
   - `store/sqlite`: CRUD, WAL concurrency, stale worker offline marking, chat persistence.
   - `scheduler`: selector matching, `sandboxed` vs `full_access` isolation rules, queue draining.
   - `gitutil`: temporary git repo worktree creation, commit diff stat extraction.
2. **Integration Tests (`go test -race ./internal/controller/...`):**
   - Spins up in-process `syre` + `syred` over loopback WebSocket with `mock` driver to test full lifecycle: dispatch, subscribe/replay/unsubscribe ref-counting, interactive stdin, cancel PGID cleanup, and controller restart re-adoption.
3. **Frontend Tests & E2E:**
   - Angular unit tests for Signal stores (`FleetStore`, `TabsStore`).
   - Playwright smoke test against a running `syre` + `syred --enable-mock-driver` verifying login, fleet tree rendering, xterm snapshot replay, Read-Only guard vs Interactive Takeover, and mobile drawer layout.
