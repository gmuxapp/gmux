# ADR-0004: Integrated PTY + WebSocket transport (replace abduco + ttyd)

- Status: Accepted
- Date: 2026-03-13

## Context

Current session lifecycle requires two external C dependencies:
- **abduco**: allocates PTY, daemonizes, handles Unix socket attach/detach
- **ttyd**: bridges abduco's PTY to browser via WebSocket, serves xterm.js

Both are maintenance liabilities:
- abduco is effectively unmaintained; we carry two patches (no-alternate-screen, kitty keyboard protocol)
- ttyd adds per-session port management, token auth, and a custom binary protocol on top of WebSocket
- The interaction between the two creates debugging complexity (no scrollback in either, buffer quirks)

Meanwhile, the core problem is simple:
1. Hold a PTY with a running command
2. Let one or more WebSocket clients see output and send input
3. Forward window resize
4. Detect command exit

## Decision

**gmuxr holds the PTY directly and serves a WebSocket endpoint.** This replaces both abduco and ttyd with a single Go binary.

### Architecture

```
┌─────────────────────────────────────┐
│ gmuxr (per session)              │
│  ├── PTY (creack/pty + exec)        │
│  ├── WebSocket on Unix socket       │
│  ├── Metadata writer                │
│  └── Optional scrollback ring       │
└──────────┬──────────────────────────┘
           │ Unix socket (WebSocket)
┌──────────┴──────────────────────────┐
│ gmuxd (per machine)                 │
│  ├── Session discovery              │
│  ├── REST API + SSE events          │
│  └── WebSocket reverse proxy        │
│       /ws/{session_id} → gmuxr   │
└──────────┬──────────────────────────┘
           │ TCP (HTTP/WS)
┌──────────┴──────────────────────────┐
│ gmux-web (browser)                  │
│  ├── Session list (tRPC via API)    │
│  ├── xterm.js + AttachAddon         │
│  │    connects to gmuxd /ws/{id}    │
│  └── UI chrome                      │
└─────────────────────────────────────┘
```

### WebSocket protocol

Use xterm.js AttachAddon's native protocol: **raw bytes, no envelope.**

- **Server → client**: raw PTY output bytes (binary WebSocket frames)
- **Client → server**: raw keyboard input bytes (binary or text frames)
- **Resize**: single JSON text message `{"type":"resize","cols":N,"rows":N}`
  - Distinguished from terminal input by being a valid JSON object with `type` field
  - AttachAddon sends raw keystrokes, never structured JSON

This is simpler than ttyd's protocol (no type-byte prefix) and directly compatible with xterm.js AttachAddon.

### Session persistence model

- gmuxr IS the session. Process alive = session alive.
- No double-fork daemonization needed: gmuxr itself is the long-lived process.
- Detach = close WebSocket. Reattach = new WebSocket connection.
- Multiple concurrent viewers supported (fan-out PTY output to all connected clients; only first non-readonly client sends input).

### Socket path convention

```
/tmp/gmux-sessions/{session_id}.sock
```

gmuxd discovers these alongside metadata files, proxies WebSocket connections to them.

### Scrollback (future)

gmuxr can maintain a ring buffer of recent PTY output. On new WebSocket connection, replay the buffer before switching to live — solving the "blank screen on reattach" problem that neither abduco nor ttyd could solve.

## Implementation plan

### Phase 1: PTY server in gmuxr
- [ ] Add `internal/ptyserver` package using `github.com/creack/pty`
- [ ] Fork+exec command in PTY
- [ ] Serve WebSocket on Unix socket
- [ ] Handle raw I/O: PTY output → WS broadcast, WS input → PTY write
- [ ] Handle resize JSON messages
- [ ] Detect child exit, clean up socket

### Phase 2: gmuxd WebSocket proxy
- [ ] Add `/ws/{session_id}` endpoint that proxies to gmuxr's Unix socket
- [ ] Pass through binary frames transparently

### Phase 3: xterm.js integration in gmux-web
- [ ] Add xterm.js + AttachAddon
- [ ] Connect to `ws://gmuxd-host/ws/{session_id}`
- [ ] Handle terminal resize → send JSON resize message
- [ ] Terminal appears in detail panel on session select

### Phase 4: Scrollback replay (future)
- [ ] Ring buffer in gmuxr (configurable size, default 100KB)
- [ ] Replay on new connection before switching to live stream
- [ ] Optional: persist scrollback to disk for crash recovery

## Consequences

### Positive
- **Zero external dependencies**: single Go binary replaces abduco + ttyd
- **Simpler debugging**: one process per session, standard WebSocket
- **Scrollback possible**: we own the PTY buffer
- **Cross-platform**: `creack/pty` supports Linux, macOS, BSD
- **Protocol simplicity**: raw bytes, compatible with xterm.js out of the box
- **No port management**: Unix sockets, gmuxd proxies

### Negative
- Must implement PTY management ourselves (~200-300 lines of Go)
- Must handle edge cases (signal forwarding, terminal cleanup, zombie processes)
- Lose abduco's CLI attach capability (can add a thin `gmux attach` CLI client later)

### Neutral
- ttyd's auth token mechanism not needed (gmuxd is the auth boundary)
- abduco's detach-key not needed (WebSocket close = detach)
