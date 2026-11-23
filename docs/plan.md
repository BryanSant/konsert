# Konsert Technical Implementation Plan

This document is the engineering blueprint for **Konsert** as specified in [prd.md](./prd.md).

> **Status (2026-11-23).** Sections 1–7 are the original MVP blueprint. Phases 1–4 are built, but the code has moved on: subscription-first agent chat ([design/agent-chat.md](./design/agent-chat.md)), UUIDv7 `id` + short `sid` on every record, a single squashed schema (`internal/store/sqlite/migrations/001_schema.sql`), `encoding/json/v2`, hub-managed daemons. Where sections 1–7 disagree with the code, the code wins. **Section 8 is the forward roadmap** for the two pillars in the PRD: total visibility and intelligent scheduling.

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

## 8. Roadmap: Visibility and Scheduling

Phases 1–4 of the original roadmap (daemon and PTY core; hub, store, scheduler and MCP; PWA; telemetry and packaging) are done, as are the subscription-first chat work and first-run setup. The phases below close the gaps marked **[gap]** in the PRD. They are ordered by dependency: scheduling decisions need visibility data, migration needs attempts and history, and the orchestrator skill needs the tools from every earlier phase. Each phase is one `ka` epic.

### Phase 5: Observed sessions (Visibility I)

Goal: every agent session on a machine shows up, whoever started it.

- `internal/worker/observe/`: a `Source` interface per agent (`Discover`, `Tail(session) <-chan protocol.AgentChatEvent`). Sources read the session stores agents already write:
  - claude: `~/.claude/projects/<encoded cwd>/*.jsonl` (and every `CLAUDE_CONFIG_DIR` account, e.g. `~/.claude2`);
  - codex: `~/.codex/sessions/**/rollout-*.jsonl`;
  - grok, cursor-agent, copilot, agy: their session/state stores (see [research/agent-cli-capabilities.md](./research/agent-cli-capabilities.md); add what's missing).
  Each source maps records to the normalized `AgentChatEvent` stream the chat engines already use, so the UI renders observed sessions with the same components.
- Hooks stay as a low-latency status signal; the process scan becomes a fallback and gains cursor-agent, copilot and the desktop apps.
- Status from session records (turn started/ended, waiting for permission), not CPU.
- Hub: observed sessions are `tasks` rows with `origin=external` (exists); add `agent_session_id`, account, model, summary, files touched and counts. Transcripts are fetched from the daemon only when a user opens one, cached on the hub for 30 minutes (configurable) and purged; nothing transcript-shaped is persisted.
- PWA: read-only session view; an "All activity" list per machine.
- Exit: start a session by hand in each of the six CLIs on macOS, Linux and Windows; each appears within 5 s with correct status transitions and tool calls. Daemon CPU stays under 1% idle.

### Phase 6: Processes and load (Visibility II)

- Daemon process sampler (per OS: `/proc` on Linux, `libproc`/`ps` on macOS, Toolhelp/`NtQuerySystemInformation` on Windows): per-process CPU and RSS every 5 s while work runs, 30 s idle.
- Attribution: managed sessions by process group / job object; observed sessions by descendant tree of the agent process, keeping orphaned descendants attributed by start-time lineage until they exit.
- Machine load split into agent load and other load, with top non-agent processes; input-idle time on interactive machines (macOS `IOHIDSystem` idle, Windows `GetLastInputInfo`, Linux logind `IdleHint`).
- Store: `metric_samples` (machine and session, downsampled 1 min / 1 h, retention settings), session peaks on the task row.
- Exit: a build started by an agent is attributed to it; a build started by hand counts as other load; history survives hub restarts.

### Phase 7: Facts, work keys and outcomes

- Extensible capability probes: built-in list plus probes declared in daemon config (command + version regex, or path exists), GPU detection (NVIDIA `nvidia-smi`, Apple Silicon GPU cores and unified memory, AMD), architecture in the selector, operator labels per machine, machine role (`dedicated` / `interactive` / `control-only`).
- `work_key` on tasks (derived from project, repo, branch/worktree, kind; overridable by the dispatcher) and requirement fields on dispatch.
- `task_attempts` table: one row per attempt with machine, account, queue wait, duration, exit, failure category, resource peaks, tokens/cost; requeue creates a new attempt.
- Failure classifier on the daemon (OOM: exit 137/SIGKILL with kernel OOM evidence, Windows job memory limit; disk full; timeout; missing tool) feeding the attempt's failure category; agent-reported categories (auth, usage limit) from the chat engines.
- `work_profiles` maintained from attempts per (work_key, machine class).
- Exit: profiles fill in after a few runs; `get_work_profile` returns them.

### Phase 8: Policy and enforcement

- Machine policy (hub-owned, pushed to the daemon): max managed sessions, max load average, min free RAM, reserved CPU/RAM, quiet hours, allowed work classes, role. Hub admission check plus daemon-side refusal.
- Resource envelopes per OS: Linux systemd user scopes (cgroup v2) with rlimit fallback; Windows Job Object limits on the existing job; macOS priority plus monitored kill. Envelope size from the work profile or a default class.
- Optional rootless Podman tier for Linux work (`isolation: container` per project): image, mounts for repo and agent login state, envelope via Podman flags.
- Policy read-only over MCP; edits in UI/CLI, audited.
- Break glass: `request_override` (rules, scope, duration, justification) with user approval by default (approval card in the orchestrator chat + notification), optional orchestrator self-approval, automatic expiry. `policy_events` log of every block and override with the stated intent; a report of how often rules blocked work and whether overrides followed.
- Container awareness: detect Podman/Docker; per-project opt-in container execution; never required for resource protection.
- Exit: a memory-hungry test job is capped on Linux and Windows and killed (classified OOM) when it exceeds its envelope; an interactive machine with an active user receives no new heavy work.

### Phase 9: Placement

- Capacity model: headroom = capacity − reservations − running envelopes − observed other load.
- Scorer: hard filters (requirements, policy, logins) then a weighted score (headroom fit for the expected profile, locality, account quota, placement rules, spread). Unknown work rotates across eligible machines weighted by headroom.
- Repo locality: daemons report checkouts (repo remote URL + path + branch + last fetch) found under configured roots; `checkouts` table (many machines per repo).
- Subscription-aware choice of machine and account pair; spread across same-vendor accounts.
- `placement_rules` (scope, prefer/require/avoid, target machine or class, provenance, evidence, expiry) and `placement_log` (candidates, rejections, scores, decision, decider).
- MCP/REST: `explain_placement` (dry run), placement explanation on every task.
- Exit: scenario tests: Windows-only work never lands elsewhere; repo work prefers machines with a checkout until they're saturated; exhausted account is avoided until reset; rules override scores but never policy.

### Phase 10: Retry and migration

- Retry policy by failure category (bigger machine for OOM/disk/timeout; another account for usage limit; never for auth).
- Migration: stop on A; transfer via push to the shared remote when available, otherwise a `git bundle` sent directly A → B over the fleet transfer channel (Phase 10a), falling back to a hub relay; start on B with a handoff note (previous summary, failure, files changed).
- Learned rules: a migration that succeeds after a classified failure writes a `learned` placement rule for the work key with the evidence.
- Exit: an ML-style job that OOMs on a small machine is migrated to a larger one, succeeds, and the next dispatch of that work key goes straight to the larger class.

### Phase 10a: Fleet transfer channel

- Per-daemon fleet SSH keypair (generated locally, public key registered with the hub, private key never leaves the machine).
- Receiver: konsertd runs a minimal SSH endpoint (Go `x/crypto/ssh`) on demand, or uses the system sshd with a forced-command `authorized_keys` entry; the hub authorizes one sender key for one transfer with an expiry. Design spike decides between the two (Windows has OpenSSH server optional; a built-in endpoint avoids depending on it).
- Reachability probe (Tailscale/LAN addresses reported in facts); transfer path selection git remote → direct → hub relay; audit of each transfer.
- Exit: a repo bundle moves bocabox → bocamacmini directly, and via the hub when direct isn't reachable; keys are scoped and expire.

### Phase 11: Orchestrator scheduling

- MCP tools: `fleet_snapshot`, `explain_placement`, `get_work_profile`, `list_attempts`, `set_placement_rule` / `clear_placement_rule`, `requeue_task`, `migrate_task`, `request_override`, plus event notifications to the orchestrator chat for failure, stall, needs-input and completion of work it dispatched.
- Ship the Konsert scheduler skill (installed for the orchestrator agent): reading the snapshot, trusting vs overriding the built-in scheduler, responding to failure categories, migrating, recording rules.
- Evaluation suite: scripted fleets (mock engine with synthetic resource profiles) and scenarios the orchestrator must handle; track decisions over versions.
- Exit: the orchestrator handles the OOM → migrate → pin scenario and a usage-limit → switch-account scenario end to end, and its learned rules survive a hub restart and a fresh orchestrator context.

### Cross-cutting

- Deep links in the PWA by sid (`/tasks/t-…`, `/machines/m-…`, `/chats/c-…`).
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
   - Spins up in-process `konsert` + `konsertd` over loopback WebSocket with `mock` driver to test full lifecycle: dispatch, subscribe/replay/unsubscribe ref-counting, interactive stdin, cancel PGID cleanup, and controller restart re-adoption.
3. **Frontend Tests & E2E:**
   - Angular unit tests for Signal stores (`FleetStore`, `TabsStore`).
   - Playwright smoke test against a running `konsert` + `konsertd --enable-mock-driver` verifying login, fleet tree rendering, xterm snapshot replay, Read-Only guard vs Interactive Takeover, and mobile drawer layout.
