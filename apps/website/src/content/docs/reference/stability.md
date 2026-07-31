---
title: Interface stability
description: Interfaces covered by the gmux 2.x compatibility covenant.
---

This page defines the public interfaces that gmux 2.x keeps compatible. Covenanted interfaces evolve additively: existing variables, files, and keys keep their meaning throughout 2.x.

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

### Configuration files

- `host.toml` contains host-local daemon configuration. Existing keys keep their meaning, but the file is strictly validated: unknown keys are rejected to catch mistakes, except for the documented deprecated `tailscale.hostname`, `discovery.tailscale`, and `[[peers]]` shapes, which are ignored with warnings. See the [host.toml reference](/reference/host-toml/#strict-validation) for details. A `host.toml` using keys from a newer gmux release may therefore require the matching daemon version.
- `settings.jsonc` and `theme.jsonc` contain portable frontend preferences. Their keys evolve additively, and unknown fields are tolerated for forward compatibility.

## Explicitly not covenanted

- Internal environment variables gmux uses as private process plumbing. They are undocumented on purpose and must not be read, set, or passed to session children.
- Runtime state files, including `state.db`, sockets, and logs. Connected hosts are runtime state in `state.db`, not a public storage interface.
- Incidental prose and layout in human-oriented exchange reports, help-text wording, and diagnostic wording, unless a specific element is explicitly documented as stable or as machine-parseable output.
