# Konsert Technical Implementation Plan

This document is the engineering blueprint for implementing **Konsert** as specified in [prd.md](./prd.md). It is structured so coding agents or human engineers can execute Phase 1 through Phase 4 incrementally with clear interfaces, wire protocols, database migrations, and verification gates.

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
konsert/
├── cmd/
│   ├── konsert/                  # Controller CLI & HTTP/WS/MCP server (embeds web/dist)
│   └── konsertd/                 # Cross-platform worker daemon CLI
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
│   │   ├── service.go            # Shared application service (used by REST, MCP, Orchestrator)
│   │   ├── mcp.go                # MCP server (stdio + Streamable HTTP/SSE)
│   │   ├── api.go                # REST + Browser WS + Worker WS HTTP handlers
│   │   └── embed.go              # //go:embed static PWA assets & SPA fallback handler
│   ├── orchestrator/
│   │   ├── backend.go            # OrchestratorBackend interface & tool-calling loop
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
│           │   ├── services/     # AuthService, ApiClient, KonsertSocketService
│           │   └── state/        # FleetStore, TabsStore, TelemetryStore, OrchestratorStore (Signals)
│           ├── layout/           # AppShell, FleetSidebar, TabBar, TelemetryDrawer, MobileHeader
│           └── features/
│               ├── auth/         # LoginViewComponent
│               ├── terminal/     # XtermPaneComponent (Read-Only / Interactive Takeover)
│               └── orchestrator/ # OrchestratorChatComponent (streaming markdown + tool cards)
├── deploy/
│   ├── systemd/                  # konsert.service, konsertd.service
│   ├── quadlet/                  # konsertd.container (Podman)
│   └── launchd/                  # dev.konsert.konsertd.plist
├── Makefile
└── .goreleaser.yaml
```

---

## 3. Data Model & SQLite Schema

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

Both Worker (`konsertd` ↔ `konsert` at `/ws/worker`) and Browser (PWA ↔ `konsert` at `/ws/client`) use a **dual-frame WebSocket protocol**:
- **Text Frames (JSON):** Control plane messages, telemetry events, task dispatch/lifecycle, chat streaming.
- **Binary Frames:** Raw PTY byte streams to eliminate base64 inflation and minimize GC/CPU overhead during noisy builds.

### 4.1 Binary PTY Frame Format

```text
+---------------+----------------------------------+-------------------------+
| FrameType (1B)| TaskID (36B UTF-8 UUID, padded)  | Raw PTY Bytes (N bytes) |
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
| Controller → Browser | `chat.delta` / `chat.tool` / `chat.done` | Streaming orchestrator tokens and tool-call execution cards |
| Controller → Browser | `fleet.update` | Incremental worker/task state changes pushed to all connected UI clients |

### 4.3 On-Demand PTY Streaming Sequence

```mermaid
sequenceDiagram
    participant UI as Angular PWA
    participant Hub as konsert (Controller)
    participant W as konsertd (Worker)

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
- If a browser's queue fills (e.g., backgrounded mobile Safari), `Hub` drops the slow subscriber and closes its socket rather than blocking `konsertd` or other viewers; the PWA auto-reconnects and re-subscribes cleanly.

---

## 5. Worker Daemon (`konsertd`) Architecture

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
   - Create worktree at `<repo_root>/.konsert-worktrees/<task_id>` on `branch` (`git worktree add -B <branch>` or checkout existing).
   - Run agent with `effective_dir` set to the worktree path.
3. On completion: compute `git status --short`, `git diff --stat <base_sha>`, and `git log --oneline <base_sha>..HEAD`, serialize into `GitSummaryJSON`, and emit in `task.status`.

---

## 6. Controller (`konsert`) Architecture

### 6.1 Shared Application Service (`internal/controller/service.go`)
All three entrypoints—**REST API**, **MCP Server**, and **Built-in Orchestrator Chat**—call the same `FleetService` methods:
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
- **`TabsStore`:** `openTabs = signal<WorkspaceTab[]>([{ id: 'orchestrator', kind: 'orchestrator', pinned: true }])`, `activeTabId = signal<string>('orchestrator')`. Tracks per-tab `interactiveMode: boolean` (always defaults to `false` / Read-Only).
- **`TelemetryStore`:** `eventsByTask = signal<Record<string, TaskEvent[]>>({})` fed by `task.event` WS messages and initial REST hydration.
- **`OrchestratorStore`:** Manages active chat session, streaming assistant delta buffer, and collapsible tool-call execution cards.

### 7.2 `XtermPaneComponent` Implementation Details
- Mounts `@xterm/xterm` inside `afterNextRender()`, loads `FitAddon`, `WebLinksAddon`, and attempts `WebglAddon` (catching context errors and falling back to default canvas renderer).
- Attaches a `ResizeObserver` on the container element; debounces resize events (`100ms`), calls `fitAddon.fit()`, and sends `pty.resize` to `konsert`.
- Subscribes to `KonsertSocketService.ptyStream(taskId)`:
  - On `0x01` (`pty.snapshot`): calls `term.reset()` then `term.write(bytes)`.
  - On `0x02` (`pty.data`): calls `term.write(bytes)`.
- `term.onData(data)` checks `interactiveMode()` signal—when `false` (Read-Only), keystrokes are ignored and a subtle toast/hint reminds the user to toggle **"Take Control"**. When `true`, sends binary `0x03` frame.

### 7.3 Responsive & PWA Strategy
- Tailwind CSS v4 breakpoints (`md:` = `768px`):
  - `>= 768px`: 3-pane IDE grid (collapsible left Fleet Explorer `260px`, center flex workspace, collapsible right Telemetry Drawer `320px`).
  - `< 768px`: Single-pane view with compact header (drawer toggle + tab selector + status pill), slide-over Fleet Explorer overlay, bottom-sheet Telemetry Drawer, and mobile quick-key toolbar (`Ctrl+C`, `Esc`, `Tab`, `Enter`) when Interactive mode is on.
- `@angular/service-worker` caches app shell and static assets (`prefetch`), while `/api/*`, `/ws/*`, and `/mcp` bypass cache (`network-only`).

---

## 8. Phased Delivery Roadmap & Acceptance Criteria

### Phase 1 — Daemon, Protocol & PTY Core
- **Deliverables:** `go.mod`, `internal/protocol`, `internal/worker/{pty_unix.go,pty_windows.go,ringbuf.go,daemon.go,runner.go}`, `internal/gitutil`, `internal/worker/drivers/{driver.go,mock.go,claude.go,agy.go,grok.go}`, `cmd/konsertd`.
- **Exit Criteria:**
  - `konsertd doctor` detects installed CLIs (`claude`, `agy`, `grok`) and prints permission posture (`full_access` default).
  - Unit tests pass for `RingBuffer` wrap-around/snapshot, binary frame codec, `gitutil` worktree lifecycle, and PTY spawn/resize/PGID-cancel using `mock` driver.
  - Cross-compilation succeeds for `linux/amd64`, `linux/arm64`, `darwin/arm64`, `windows/amd64`.

### Phase 2 — Controller, SQLite WAL, Scheduler, MCP & Orchestrator API
- **Deliverables:** `internal/store/{store.go,sqlite/*}`, `internal/auth`, `internal/controller/{hub.go,scheduler.go,service.go,api.go,mcp.go}`, `internal/orchestrator/{backend.go,openai.go,anthropic.go,gemini.go}`, `cmd/konsert`.
- **Exit Criteria:**
  - End-to-end Go integration test: start `konsert` + `konsertd` (with `mock` driver), dispatch task via MCP `dispatch_task`, await `get_task_result(wait=true)`, verify structured summary & git diff without PTY streaming.
  - Reconnect test: restart `konsert` while `konsertd` runs a task; verify task re-adoption via `worker.hello`.
  - Auth tests: verify fleet token enforcement, browser cookie auth, and opt-in `--allowed-roots` enforcement.

### Phase 3 — Angular 22 Zoneless Signals PWA
- **Deliverables:** `web/` Angular 22 app with Tailwind v4 dark theme, Fleet Explorer tree, multi-tab `@xterm/xterm` workspace (Read-Only default + Interactive Takeover toggle), Telemetry Drawer, Orchestrator Chat tab, mobile `< 768px` drawer & quick-key bar, `manifest.webmanifest`, `//go:embed` integration into `cmd/konsert`.
- **Exit Criteria:**
  - `npm run build` embeds cleanly into a single `konsert` binary.
  - Live demo with `mock` driver: open tab → instant 512 KB replay + live ANSI stream; close tab → worker stops streaming; toggle Take Control → stdin reaches PTY; resize window → PTY dimensions update.
  - Mobile viewport (`390x844`) verified usable for Fleet drawer, Orchestrator Chat, and terminal viewing.

### Phase 4 — Live Agent Telemetry Scrapers, Hardening & Packaging
- **Deliverables:** Full `claude` JSONL transcript watcher, `agy` & `grok` structured milestone/summary parsers, `deploy/` templates (systemd, Podman Quadlet, launchd), `.goreleaser.yaml`, `Makefile`, `README.md`.
- **Exit Criteria:**
  - Live smoke tests against real `claude`, `agy`, and `grok` CLIs verifying full-access default execution, structured telemetry milestones, and clean final summaries.
  - `goreleaser check` and `make build-all` produce clean release artifacts for Linux, macOS, and Windows.

---

## 9. Testing & Verification Strategy

1. **Go Unit & Race Tests (`go test -race ./...`):**
   - `ringbuf`: wrap-around boundaries, concurrent write + snapshot.
   - `protocol`: binary frame encode/decode fuzz & edge cases.
   - `store/sqlite`: CRUD, WAL concurrency, stale worker offline marking, chat persistence.
   - `scheduler`: selector matching, `sandboxed` vs `full_access` isolation rules, queue draining.
   - `gitutil`: temporary git repo worktree creation, commit diff stat extraction.
2. **Integration Tests (`go test -race ./internal/controller/...`):**
   - Spins up in-process `konsert` + `konsertd` over loopback WebSocket with `mock` driver to test full lifecycle: dispatch, subscribe/replay/unsubscribe ref-counting, interactive stdin, cancel PGID cleanup, and controller restart re-adoption.
3. **Frontend Tests & E2E:**
   - Angular unit tests for Signal stores (`FleetStore`, `TabsStore`).
   - Playwright smoke test against a running `konsert` + `konsertd --enable-mock-driver` verifying login, fleet tree rendering, xterm snapshot replay, Read-Only guard vs Interactive Takeover, and mobile drawer layout.
