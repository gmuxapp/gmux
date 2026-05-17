# ADR 0005: CLI routes session actions through gmuxd, not session sockets

**Status:** Accepted
**Date:** 2026-05-17
**Related:** gmuxapp/gmux#221 (peer --send/--tail), gmuxapp/gmux#222 (tail on dead sessions)

## Context

Until this change, four of the CLI's session subcommands split cleanly
between "go through gmuxd" and "shortcut to the session's unix socket":

| Subcommand | Transport before | Local live | Local dead | Peer live | Peer dead |
|---|---|:-:|:-:|:-:|:-:|
| `--list`   | gmuxd HTTP            | ✓ | ✓ | ✓ | ✓ |
| `--kill`   | gmuxd HTTP            | ✓ | error | ✓ via `peer.Forward` | n/a |
| `--attach` | gmuxd WS              | ✓ | error | ✓ via `ProxyWS` | error |
| `--send`   | **session socket**    | ✓ | n/a | rejected client-side | n/a |
| `--tail`   | **session socket**    | ✓ | dial fails (#222) | rejected client-side (#221) | error |
| `--wait`   | gmuxd HTTP            | ✓ | n/a | rejected server-side | n/a |

The shortcut path looked appealing for `--send` / `--tail`: the data is
already at the session's socket; one extra hop through gmuxd is pure
overhead. But every "exception" to "talk to gmuxd" duplicates the
question "where does this session's data actually live?" in a new
place. The duplications surfaced as the two issues this ADR resolves:

- **#222**: `--tail` against a dead session dials a socket that no
  longer exists and returns `connect: connection refused`, even though
  the persisted scrollback is sitting on disk in a directory gmuxd
  manages.
- **#221**: `--send` against a peer session errors out at the CLI
  ("only supported for local sessions") because the CLI can't reach
  the peer's session socket directly. gmuxd already has the routing
  (`peer.Forward`, `peer.ProxyWS`) that the web UI uses for the same
  case; the CLI just doesn't use it.

## Decision

Every session-targeted CLI subcommand talks to the **local gmuxd
HTTP API** and never to a per-session unix socket. Concretely:

- `--tail N <id>` → `GET /v1/sessions/<id>/scrollback?tail=N`.
- `--send <id> 'text'` → `POST /v1/sessions/<id>/input`.
- `--kill <id>` → `POST /v1/sessions/<id>/kill` (unchanged).
- `--attach <id>` → `GET /v1/sessions/<id>/attach` + WS to `/ws/<id>` (unchanged).
- `--wait <id>` → `GET /v1/sessions/<id>/wait` (unchanged; peer support
  deferred until peer-side Status streaming exists).

gmuxd handles the "where does the data live?" question once, in the
session-action dispatcher in `services/gmuxd/cmd/gmuxd/main.go`:

1. If the session ID belongs to a peer, `peer.Forward` (or
   `peer.ProxyWS` for WS) handles the call. The peer's gmuxd then
   answers the question for its own local state.
2. Otherwise, the local handler decides between the runner socket
   (`/v1/sessions/<id>/input` proxies to runner `/input`) and the
   persisted state directory (`/v1/sessions/<id>/scrollback` reads
   from `<state>/sessions/<id>/{scrollback,scrollback.0}`).

The CLI never participates in this routing. It speaks one HTTP API.

## Consequences

**Positive.**

- Both issues collapse into the same change. #222 stops being a "the
  CLI needs to know about state directories" problem; it's just
  "gmuxd's scrollback endpoint already reads from disk, and we
  exposed `?tail=N` to it."
- Peer support for `--send` and `--tail` is automatic: the dispatcher
  already routes peer IDs through `peer.Forward`. Adding a new action
  case is the entire integration.
- The CLI loses ~50 lines (`sessionSocketClient`, the `SocketPath ==
  ""` guard logic that lived in `cmdTail` and `cmdSend`). Future
  action subcommands are simpler to add: one gmuxd endpoint and one
  CLI HTTP call.
- The runner's socket API becomes a private implementation detail of
  gmuxd. We're free to change it without thinking about CLI version
  skew.

**Negative.**

- One extra HTTP hop on every local session action. Sub-millisecond
  on a unix socket; not measurable against typing latency.
- gmuxd is now a hard dependency for `--tail` / `--send` (in
  addition to `--kill` / `--attach` already). The CLI's
  `ensureGmuxd()` already runs at the start of every action
  subcommand, so this changes nothing observable.
- `--tail` now returns raw PTY bytes (CRLF, ANSI escapes) instead of
  the rendered plain text the runner used to emit. The dead-session
  case had no choice (no live screen state to render from), so
  unifying on raw bytes was the consistent option. Users who want
  plain text pipe through `sed`. Pre-1.0; called out as breaking.

**Neutral, worth flagging.**

- The runner cap on `/input` (`maxInputBytes = 1 << 20`) is now
  duplicated as a constant in gmuxd (`services/gmuxd/cmd/gmuxd/main.go`).
  This is intentional: gmuxd validates at its edge so a 413 surfaces
  to the user instead of silent truncation inside the runner. If
  these ever diverge, the gmuxd one should be the smaller (rejects
  earlier).

## Alternatives considered

**Fall-back to on-disk scrollback inside the CLI (the original #222
fix).** Smaller diff; the CLI dials the session socket as today,
falls back to reading `<state>/sessions/<id>/scrollback` when the
socket is gone. Rejected: leaks gmuxd's state-directory layout into
the CLI, doesn't help #221, and the next "where does session data
live?" question (devcontainer, sandbox, etc.) duplicates the leak.

**Clear `SocketPath` from the gmuxd session record on death.** That
would make the existing `case !sess.Alive` arm in `cmdTail` fire
with a friendly "session is not running" message. Rejected: the
user wants the data, not the error. This option fixes the error
message but not the underlying capability.

**Always render scrollback to plain text inside gmuxd.** Symmetric
output between live and dead sessions. Rejected for now: dead
sessions would need a terminal-emulator replay inside gmuxd, which
pulls a new dependency into the daemon for a feature that didn't
warrant it. The raw-bytes format is what `cat ~/.local/state/.../scrollback`
gave users as the workaround; we're standardizing on what they were
already doing.
