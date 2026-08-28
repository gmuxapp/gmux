---
title: Interface stability
description: Interfaces covered by the gmux 2.x compatibility covenant.
---

This page defines the public interfaces that gmux 2.x keeps compatible. Covenanted interfaces evolve additively: existing variables, files, and keys keep their meaning throughout 2.x. Surfaces listed under [Experimental](#experimental) below are excluded from the covenant until they are moved into it, even where another page documents them.

## Covenanted for 2.x

### Environment variables

- `GMUX`: set to `1` inside a session.
- `GMUX_SESSION_ID`: identifies the current session.
- `GMUX_ADAPTER`: names the adapter selected for the session.
- `GMUX_SOCKET`: gives the current session runner's Unix socket path.
- `GMUX_SESSION_SOCK`: gives agent hooks the socket used for session and turn events.
- `GMUX_EDIT_FALLBACK`: controls the fallback command used by `gmux edit`.
- `GMUX_NO_AGENT_HOOK`: disables injection of the session's agent hook.
- `GMUX_SOCKET_DIR`: relocates the directory containing session sockets.

### Command-line interface

The machine-facing CLI documented in the [CLI reference](/reference/cli/) is covenanted, including:

- documented verbs, flags, argument grammar, and input and output payload channels;
- documented exit-code taxonomy and error codes explicitly identified as stable;
- stdout/stderr placement and exactly-once session-ID publication;
- ordering and delimiters that the documentation explicitly teaches scripts to consume, including argv-order results from multi-session `wait` and headers that appear only for multi-session output.

These contracts may grow additively during 2.x. They do not freeze every rendered byte of human-oriented output.

`gmux ls --json` is specifically covenanted as one top-level array (`[]`, never
`null`) whose rows follow the documented alive-first/newest-first ordering.
The required `ref`, `id`, `adapter`, and `alive` keys and every documented
existing key retain their JSON type, absence rules, owner scope, and meaning.
Optional keys are omitted rather than emitted as `null`; peer projections may
omit newer optional keys. New keys may be added, so consumers must ignore keys
they do not understand. `ref` remains the authoritative reusable session
argument; `alive` remains runner liveness only, never activity, success,
resumability, health, or capability support.

#### Diagnostic session fields

The session fields `pid`, `socket_path`, `runner_version`, and `binary_hash`
— wherever session JSON is served (`gmux ls --json`, the HTTP and SSE session
payloads, peer projections) — are origin-local diagnostics, not scripting
inputs. They keep their JSON type
while present, but they are best-effort: any of them may be absent on any row
(`binary_hash` never appears in `gmux ls --json` at all),
they carry no meaning outside the host that owns the session (a `pid` or
`socket_path` from another host is not usable where you read it), and gmux may
stop emitting them in a future release without a major version. Do not build
on them; use `ref`, `alive`, `status`, and the exit-code taxonomy instead.

### Configuration files

- `host.toml` contains host-local daemon configuration. Existing keys keep their meaning — except the keys listed under [Experimental](#experimental), which are documented for use but not yet covenanted — and the file is strictly validated: unknown keys are rejected to catch mistakes, except for the documented deprecated `tailscale.hostname`, `discovery.tailscale`, and `[[peers]]` shapes, which are ignored with warnings. See the [host.toml reference](/reference/host-toml/#strict-validation) for details. A `host.toml` using keys from a newer gmux release may therefore require the matching daemon version.
- `settings.jsonc` and `theme.jsonc` contain portable frontend preferences. Their keys evolve additively, and unknown fields are tolerated for forward compatibility.

## Experimental

These surfaces ship in current releases but are still settling. They may
change incompatibly in a minor release; anything not moved to the covenanted
list stays experimental rather than becoming stable by shipping.

- **`[notifications.ntfy]`** (`host.toml`): the key names, value grammar, and
  delivery semantics of best-effort phone notifications.
- **`agent.max_subagents_by_depth`** (`host.toml`): the depth-array grammar
  and budget semantics of the active-subagent limit.
- **`gmux agent prompt --new --model <M>`**: the flag is covenanted; the
  *value* grammar is owned by the launched adapter (today pi's model selector)
  and may change with it.
- **ACP drive mode** (`drive_mode: "acp"`): the enum value is covenanted so
  clients can recognize it, but ACP-driven sessions themselves — how they are
  launched, resumed, and rendered — are still under active design (ADR 0033,
  ADR 0034).
- **HTTP endpoints not documented in this site's references**, including
  `/v1/agent-launch-reservations`: internal daemon plumbing that happens to
  live under `/v1/`; not for external callers.

## Explicitly not covenanted

- Internal environment variables gmux uses as private process plumbing. They are undocumented on purpose and must not be read, set, or passed to session children.
- Runtime state files, including `state.db`, sockets, and logs. Connected hosts are runtime state in `state.db`, not a public storage interface.
- Incidental prose and layout in human-oriented exchange reports, help-text wording, and diagnostic wording, unless a specific element is explicitly documented as stable or as machine-parseable output.
