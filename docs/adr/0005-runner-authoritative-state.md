# ADR-0005: Runner-authoritative state, adapters, and socket-based discovery

- Status: Proposed
- Date: 2026-03-14

## Context

gmuxr is a full server: it holds the PTY, serves WebSocket, maintains a scrollback buffer, and knows everything about its session. Yet gmuxd discovers sessions by polling JSON metadata files that gmuxr writes to `/tmp/gmux-meta/`. This creates:

1. **Redundant state** — metadata files duplicate what the runner already knows
2. **Polling latency** — gmuxd scans every 2s; new sessions appear with delay
3. **Stale file cleanup** — if gmuxr crashes, metadata files linger
4. **No reverse channel** — gmuxd cannot query or subscribe to runner state changes
5. **File I/O as IPC** — fragile, hard to test, hard to extend

Meanwhile, gmuxr has a unique privilege that no other tool in this space has: **it sits between the user and the child process.** It owns the PTY. It sees all output. It knows the command. It can modify the launch. cmux is a terminal (sees output, doesn't own the process). Codex/Claude Desktop are the process (can't generalize). gmuxr is the **launcher** — it can recognize, adapt, and monitor any command.

## Decision

Three changes:

1. **The runner is the source of truth.** gmuxd queries runners via their Unix sockets, not files.
2. **Adapters make the runner intelligent.** An adapter library teaches gmuxr how to launch, monitor, and discover sessions for specific tools — without any configuration from the user or cooperation from the child.
3. **Session schema v2** (see `docs/protocol/session-schema-v2.md`) defines a two-layer state model: process state (owned by gmux) and application status (reported by adapters).

### Part 1: Adapters

An adapter tells gmuxr how to work with a specific kind of child process. Adapters are Go interfaces compiled into gmuxr, with a path to external adapters later.

```go
type Adapter interface {
    // Name returns the adapter identifier (e.g. "pi", "pytest", "generic").
    Name() string

    // Match returns true if this adapter handles the given command.
    // Adapters are tried in priority order; first match wins.
    Match(command []string) bool

    // Prepare modifies the command and environment before launch.
    // Can inject flags, set env vars, wrap the command.
    // Returns the (possibly modified) command and extra env vars.
    Prepare(ctx PrepareContext) (command []string, env []string)

    // Monitor receives PTY output and produces Status events.
    // Called on every PTY read with the raw bytes.
    // Returns nil when there's no status change.
    Monitor(output []byte) *Status

    // Sidecar starts an optional background goroutine that watches
    // external state (files, sockets, APIs) and sends Status events.
    // Receives the session context; sends events on the channel.
    // Returns nil if no sidecar is needed.
    Sidecar(ctx SidecarContext) <-chan Status

    // Resumable returns sessions that can be resumed.
    // Called by gmuxd, not gmuxr. Returns nil if this adapter
    // doesn't support resumable sessions.
    Resumable() []ResumableSession
}

type PrepareContext struct {
    Command    []string
    Cwd        string
    SessionID  string
    SocketPath string
    // Note: the runner automatically sets GMUX=1, GMUX_SOCKET,
    // GMUX_SESSION_ID, GMUX_ADAPTER, and GMUX_VERSION in the
    // child's environment. Prepare() returns additional env vars
    // specific to this adapter.
}

type SidecarContext struct {
    SessionID   string
    Cwd         string
    Command     []string
    Env         []string
    Done        <-chan struct{} // closed when child exits
}

type ResumableSession struct {
    ID            string   // application-specific session ID
    Title         string
    Subtitle      string
    Cwd           string
    ResumeCommand []string // command to resume (e.g. ["pi", "--resume", "abc"])
    LastActive    time.Time
}
```

#### Example: `pi` adapter

```go
func (a *PiAdapter) Match(cmd []string) bool {
    return len(cmd) > 0 && filepath.Base(cmd[0]) == "pi"
}

func (a *PiAdapter) Prepare(ctx PrepareContext) ([]string, []string) {
    // Inject session tracking flag that pi understands
    cmd := append(ctx.Command, "--session-id", ctx.SessionID)
    // Note: GMUX, GMUX_SOCKET, GMUX_SESSION_ID, GMUX_ADAPTER, GMUX_VERSION
    // are set automatically by the runner for all adapters.
    // Adapter-specific env vars go here:
    env := []string{
        "PI_GMUX_INTEGRATION=1",
    }
    return cmd, env
}

func (a *PiAdapter) Sidecar(ctx SidecarContext) <-chan Status {
    ch := make(chan Status, 8)
    // Watch ~/.pi/sessions/<id>/ for JSONL state changes
    go watchPiSessionFile(ctx, ch)
    return ch
}

func (a *PiAdapter) Resumable() []ResumableSession {
    // Scan ~/.pi/sessions/ for resumable sessions
    // Return entries with resume command: ["pi", "--resume", "<id>"]
}
```

**What this gives the user:** `gmuxr pi` just works. The adapter injects the right flags, watches pi's session file, maps pi's internal states to gmux Status events (agent thinking → `active`, waiting for user → `attention`, task complete → `success`). No configuration.

#### Example: `pytest` adapter

```go
func (a *PytestAdapter) Match(cmd []string) bool {
    for _, arg := range cmd {
        if strings.Contains(arg, "pytest") { return true }
    }
    return false
}

func (a *PytestAdapter) Prepare(ctx PrepareContext) ([]string, []string) {
    return ctx.Command, nil // no modification needed
}

func (a *PytestAdapter) Monitor(output []byte) *Status {
    line := string(output)
    if match := passedRe.FindString(line); match != "" {
        return &Status{Label: match, State: "active"}
    }
    if strings.Contains(line, "FAILED") {
        return &Status{Label: "tests failing", State: "error"}
    }
    if strings.Contains(line, "passed") && !strings.Contains(line, "failed") {
        return &Status{Label: "all passed", State: "success"}
    }
    return nil
}
```

**What this gives the user:** `gmuxr pytest tests/` shows live test progress in the sidebar. The dot pulses green while tests run, shows count, goes red on failure, green check on all-pass.

#### Example: `generic` adapter (fallback)

```go
func (a *GenericAdapter) Match(cmd []string) bool { return true }

func (a *GenericAdapter) Prepare(ctx PrepareContext) ([]string, []string) {
    return ctx.Command, nil
}

func (a *GenericAdapter) Monitor(output []byte) *Status {
    // Any output = activity
    return &Status{Label: "running", State: "active"}
}
```

Always matches. Provides baseline "it's alive and producing output" status.

#### Adapter selection

On `gmuxr <command>`:

1. **Explicit override**: if `GMUX_ADAPTER=<name>` is set in the environment, use that adapter directly. Skips matching entirely. This is the escape hatch for weird wrappers, aliases, or nix/corepack indirection where binary name matching can't work.
   ```bash
   GMUX_ADAPTER=pi gmuxr my-custom-pi-wrapper --flags
   ```
2. **Auto-match**: walk the adapter list in priority order, call `Match(command)` on each. First match wins.
3. **Fallback**: `generic` is always last (catch-all).

#### Matching strategy

Matching should be **cheap and fast** — called once at launch, never shells out. The cost of a false negative is low (generic adapter still works, you just lose rich status), so we optimize for the common case.

Adapters match on binary name + argument scanning:

```go
func (a *PiAdapter) Match(cmd []string) bool {
    for _, arg := range cmd {
        if filepath.Base(arg) == "pi" || filepath.Base(arg) == "pi-coding-agent" {
            return true
        }
        if arg == "--" { break } // stop at arg separator
    }
    return false
}
```

This handles:
- Direct: `pi`, `pi-coding-agent`
- Wrappers: `npx pi`, `nix run .#pi`, `env pi`
- Full paths: `/home/user/.local/bin/pi`

What this deliberately **skips**: `which` resolution, `--help` probing, shebang inspection. All too slow or fragile, and `GMUX_ADAPTER` covers every edge case they would.

### Part 2: Runner serves state on its Unix socket

gmuxr already serves WebSocket at `/` on its Unix socket. We add HTTP endpoints:

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/` | GET (Upgrade) | WebSocket terminal attach (unchanged) |
| `/meta` | GET | Session metadata per schema v2 |
| `/events` | GET | SSE stream of state + status transitions |
| `/status` | PUT | Child process can self-report status (escape hatch) |

The adapter's `Monitor()` and `Sidecar()` feed into the same Status that `/meta` serves and `/events` streams. The `/status` endpoint is an escape hatch for children that want to report directly (via `GMUX_SOCKET` env var) without an adapter.

Priority: adapter sidecar > adapter monitor > child self-report > process-level defaults.

`GET /meta` response (per session-schema-v2):
```json
{
  "id": "sess-abc123",
  "created_at": "2026-03-14T10:00:00Z",
  "command": ["pi"],
  "cwd": "/home/user/dev/gmux",
  "kind": "pi",
  "alive": true,
  "pid": 12345,
  "exit_code": null,
  "started_at": "2026-03-14T10:00:01Z",
  "exited_at": null,
  "title": "gmux bootstrap",
  "subtitle": "~/dev/gmux",
  "status": {
    "label": "thinking",
    "state": "active"
  },
  "unread": false
}
```

`GET /events` SSE stream:
```
event: status
data: {"label":"thinking","state":"active"}

event: status
data: {"label":"waiting for approval","state":"attention"}

event: meta
data: {"title":"fix auth bug","subtitle":"iteration 3/10"}

event: exit
data: {"exit_code":0}
```

### Part 2b: Child awareness protocol

The child process needs to know it's running inside gmuxr, and how to talk back. This is the contract between the runner and any child — used by adapters' `Prepare()`, by tools that want native gmux integration, and by the `PUT /status` escape hatch.

#### Environment variables

gmuxr sets these in every child's environment:

| Variable | Value | Purpose |
|----------|-------|---------|
| `GMUX` | `1` | **Detection flag.** Simplest possible check: `if [ -n "$GMUX" ]`. Analogous to `CI=1`, `TERM_PROGRAM=ghostty`, `CMUX_WORKSPACE_ID`. |
| `GMUX_SOCKET` | `/tmp/gmux-sessions/sess-abc123.sock` | **Communication channel.** Unix socket path for HTTP requests back to the runner. |
| `GMUX_SESSION_ID` | `sess-abc123` | **Identity.** The session's unique ID. Useful for logging, correlation, file naming. |
| `GMUX_ADAPTER` | `pi` | **Which adapter matched.** Lets the child know how it was recognized. A child can adjust behavior if it knows it's being monitored (e.g. emit richer output). |
| `GMUX_VERSION` | `0.1.0` | **Protocol version.** So children can feature-detect. |

**Design rationale:**
- `GMUX=1` is the cheapest detection — a single env var check, no socket probing needed.
- `GMUX_SOCKET` is the communication channel — same socket that serves WebSocket for terminal attach. No second socket, no port allocation.
- `GMUX_ADAPTER` is informational — the child can ignore it, or use it to skip its own status reporting if it knows the adapter already handles it.
- All variables use the `GMUX_` prefix to avoid collisions. Follows the conventions of `CMUX_WORKSPACE_ID`, `TERM_PROGRAM`, `CI`, etc.

#### Child-to-runner HTTP API

The child can make HTTP requests to `GMUX_SOCKET` using standard Unix socket HTTP. Every endpoint is optional — children that don't know about gmux simply ignore the env vars.

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/status` | PUT | Set application status |
| `/meta` | PATCH | Update title, subtitle |
| `/caps` | GET | Discover runner capabilities |

**`PUT /status`** — Set or clear the application status:

```bash
# From a shell script or hook:
curl --unix-socket "$GMUX_SOCKET" http://localhost/status \
  -X PUT -H 'Content-Type: application/json' \
  -d '{"label":"thinking","state":"active"}'

# Clear status (revert to adapter/default):
curl --unix-socket "$GMUX_SOCKET" http://localhost/status \
  -X PUT -H 'Content-Type: application/json' \
  -d 'null'
```

Request body:
```json
{
  "label": "waiting for approval",  // short human-readable text
  "state": "attention",             // active|attention|success|error|paused|info
  "icon": "⏳"                      // optional icon hint
}
```

**`PATCH /meta`** — Update display metadata:

```bash
curl --unix-socket "$GMUX_SOCKET" http://localhost/meta \
  -X PATCH -H 'Content-Type: application/json' \
  -d '{"title":"fix auth bug","subtitle":"iteration 3 of 10"}'
```

Only the fields present in the request body are updated. Omitted fields are left unchanged.

**`GET /caps`** — Discover what the runner supports:

```json
{
  "gmux_version": "0.1.0",
  "adapter": "pi",
  "session_id": "sess-abc123",
  "endpoints": ["/status", "/meta", "/caps", "/events"],
  "features": ["scrollback", "resize"]
}
```

This lets a child gracefully degrade: check `/caps` first, only use endpoints that exist. Or skip the check entirely and just fire-and-forget `PUT /status` — a 404 is harmless.

#### Usage patterns

**Pattern 1: Unaware child** (most common)
The child doesn't know about gmux. The adapter's `Monitor()` and `Sidecar()` do all the work. Env vars are set but ignored.

**Pattern 2: gmux-aware hook** (e.g. pi's cockpit-session-map extension)
A hook or extension checks `GMUX=1`, then uses `GMUX_SOCKET` to report status at key moments:
```bash
# In a pi hook that fires on agent state change:
if [ -n "$GMUX" ]; then
  curl -s --unix-socket "$GMUX_SOCKET" http://localhost/status \
    -X PUT -d '{"label":"'"$STATE"'","state":"active"}'
fi
```

**Pattern 3: Native integration** (future)
A tool adds first-class gmux support: checks `GMUX=1`, reads `GMUX_ADAPTER` to see if it's already monitored, uses `/caps` to discover features, reports rich status via `/status` and updates title/subtitle via `PATCH /meta`.

#### Priority model

Multiple sources can set status. The runner resolves conflicts with a simple priority:

1. **Child self-report** (`PUT /status`) — highest. If the child is talking, it knows best.
2. **Adapter sidecar** — watches external state (files, APIs). Rich but indirect.
3. **Adapter monitor** — parses PTY output. Broad but noisy.
4. **Process-level defaults** — alive = dim green dot, exited = grey.

A higher-priority source overrides a lower one. If the child stops reporting (no `PUT /status` for a while), the adapter's signals take over again. The timeout for "child stopped reporting" is adapter-configurable, defaulting to 30 seconds.

### Part 3: gmuxd discovery

#### Live sessions: socket scan + registration

On startup (and periodically as fallback), gmuxd scans `/tmp/gmux-sessions/*.sock`:

1. For each `.sock` file, attempt `GET /meta` via Unix socket HTTP
2. If reachable → add session to cache, subscribe to `/events`
3. If unreachable → stale socket, clean up

As a fast path, gmuxr tries `POST /v1/register` on gmuxd (`localhost:8790`) at startup. Best-effort: if gmuxd isn't running, the runner continues normally.

#### Resumable sessions: adapter discovery

gmuxd also holds an adapter registry (same adapter code, or a subset). Periodically (or on demand), it calls `Resumable()` on each adapter and merges results into the session list.

Resumable sessions appear in the sidebar with `alive: false` and a distinct visual treatment. Clicking one triggers gmuxd to call `gmuxr` with the adapter's `ResumeCommand`.

```
Sidebar:
┌─────────────────────────────────┐
│ ● gmux bootstrap                │  ← live, active
│   ~/dev/gmux · thinking         │
├─────────────────────────────────┤
│ ◐ fix auth bug                  │  ← live, attention (needs user)
│   ~/dev/myapp · waiting         │
├─────────────────────────────────┤
│ ○ docs cleanup                  │  ← resumable (not running)
│   ~/dev/docs · 2h ago           │
├─────────────────────────────────┤
│ ○ refactor models               │  ← resumable
│   ~/dev/api · yesterday         │
└─────────────────────────────────┘
```

#### gmuxd store becomes a cache

The store caches what runners report, not a source of truth:

- Populated by registration + discovery scan
- Updated by SSE subscriptions to each runner's `/events`
- Entries removed when runner socket becomes unreachable
- Augmented with resumable sessions from adapters
- gmuxd can restart at any time — re-scan rebuilds full state

### What's removed

- `/tmp/gmux-meta/` directory and all metadata file I/O
- `metadata` package in gmuxr (replaced by in-memory state + adapters)
- File polling in gmuxd discovery (replaced by socket scan + SSE subscription)
- `AbducoName` field throughout (legacy from abduco era)
- `kind` flag on gmuxr (replaced by adapter auto-detection via `Match`)

## Architecture

```
                         gmuxr (per session)
┌────────────────────────────────────────────────────────────┐
│                                                            │
│  ┌──────────┐    ┌──────────┐    ┌──────────────────────┐  │
│  │ Adapter   │    │ PTY      │    │ Unix socket HTTP/WS  │  │
│  │           │    │          │    │                      │  │
│  │ Prepare() ├───→│ fork/    │    │  GET  /meta          │  │
│  │           │    │ exec     │    │  GET  /events (SSE)  │  │
│  │ Monitor() │←───┤ output   │    │  PUT  /status        │  │
│  │           │    │          │    │  WS   / (terminal)   │  │
│  │ Sidecar() ├──→ status ──→├───→│                      │  │
│  └──────────┘    │ ring buf │    └──────────┬───────────┘  │
│                  └──────────┘               │              │
└─────────────────────────────────────────────┼──────────────┘
                                              │ Unix socket
                         gmuxd (per machine)  │
┌─────────────────────────────────────────────┼──────────────┐
│                                             │              │
│  ┌──────────────┐    ┌──────────────────────┴───────────┐  │
│  │ Discovery     │    │ Session cache                    │  │
│  │               │    │                                  │  │
│  │ scan *.sock ──┼───→│ live: from runner /meta + /events│  │
│  │ /v1/register ─┼───→│ resumable: from adapter.Resume() │  │
│  │               │    │                                  │  │
│  │ Adapter       │    └──────────────────────┬───────────┘  │
│  │ Resumable() ──┼───→                       │              │
│  └──────────────┘                            │              │
│                                              │              │
│  Browser-facing:                             │              │
│    GET  /v1/sessions  ←──────────────────────┘              │
│    GET  /v1/events (SSE) ←── cache events                   │
│    WS   /ws/{id}     ←── proxy to runner socket             │
│    POST /v1/sessions/{id}/resume ←── launch gmuxr        │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## Implementation Plan

### Phase 1: Adapter interface + built-in adapters
- Define `Adapter` interface in `cli/gmuxr/internal/adapter/`
- Implement `generic` adapter (fallback: output activity → `active`)
- Implement `pi` adapter (match `pi` command, sidecar watches session file)
- Adapter registry with priority-ordered matching
- Wire `Prepare()` into launch, `Monitor()` into PTY read loop

### Phase 2: Runner HTTP endpoints
- Add `/meta`, `/events`, `/status` to existing Unix socket HTTP server
- In-memory session state struct fed by adapter Monitor/Sidecar
- SSE fan-out for `/events` (same pattern as gmuxd store subscribers)
- `PUT /status` for child self-reporting (escape hatch)

### Phase 3: gmuxd socket-based discovery
- Replace file-polling discovery with socket scan (`*.sock` → `GET /meta`)
- Add `/v1/register` + `/v1/deregister` endpoints
- Subscribe to runner `/events` SSE, update cache
- Remove `/tmp/gmux-meta`, `metadata` package, `AbducoName`

### Phase 4: Resumable sessions
- Add `Resumable()` to pi adapter (scan `~/.pi/sessions/`)
- gmuxd merges resumable sessions into cache with `alive: false`
- Add `POST /v1/sessions/{id}/resume` → spawns `gmuxr` with resume command
- Sidebar shows resumable sessions with distinct visual treatment

### Phase 5: Community adapter path (future)
- External adapter protocol (stdin/stdout JSON-lines or Unix socket)
- Adapter discovery from `~/.config/gmux/adapters/`
- Documentation and examples for writing adapters

## Consequences

### Positive
- **Zero-config intelligence** — `gmuxr pi` just works; adapter handles everything
- **Zero stale state** — socket reachable = alive; no files to clean up
- **Zero-latency discovery** — registration is immediate
- **Lossless gmuxd restart** — re-scan sockets, full state recovered
- **Community extensible** — anyone can write an adapter for their tool
- **Two-layer state model** — clean separation of process lifecycle from app status
- **Resumable sessions** — adapters discover what can be resumed; gmux launches it

### Negative
- Adapter `Monitor()` is called on every PTY read — must be cheap (no regex compilation per call)
- Built-in adapters couple gmuxr to knowledge of specific tools (but generic fallback always works)
- Sidecar goroutines add per-session overhead (lightweight, but nonzero)

### Neutral
- Socket scan replaces file scan (same directory, different mechanism)
- gmuxd address is well-known (`localhost:8790`) — acceptable for single-machine v1
- `kind` is inferred from adapter match, not user-specified (less explicit, more magical)
