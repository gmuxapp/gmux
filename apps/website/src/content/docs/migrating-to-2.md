---
title: Migrating to 2.0
description: Every breaking change from 1.x to gmux 2.0, who it affects, and how to migrate.
tableOfContents:
  maxHeadingLevel: 3
---

gmux 2.0 is a breaking release. Most changes are one-time: the daemon migrates its own state automatically, and the CLI tells you the new form of any removed command. This page lists every breaking change — what changed, who is affected, and the exact migration steps.

**The short version:**

1. Upgrade every machine (and rebuild devcontainers) **together** — 2.0 hosts can't peer with 1.x hosts.
2. Update scripts and muscle memory to the verb-first CLI: `gmux -- <cmd>` to run, `gmux open` for the UI, `gmux ls/attach/send/wait/kill` instead of flags.
3. Re-authorize your tailnet hosts: peers now require the host's token (**Settings → Hosts → Add token**, using `gmux auth` on each host).
4. If you parse gmux JSON: `kind` → `adapter`, `session_file` → `conversation_file`.

---

## CLI: verb-first grammar

**Who:** everyone — interactive users, scripts, aliases, agent skills. ([ADR 0009](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0009-verb-first-cli-and-frozen-top-level-namespace.md))

### Bare-command shorthand removed

| Before | After |
|--------|-------|
| `gmux pi` | `gmux -- pi` |
| `gmux pytest --watch` | `gmux -- pytest --watch` |

A bare word now errors with the run-form hint. If you run commands constantly, `alias gm='gmux --'` is shorter than the old shorthand.

### Bare `gmux` no longer opens the dashboard

| Before | After |
|--------|-------|
| `gmux` (no args) → opens the UI | `gmux` prints help; `gmux open` opens the UI |

Daemon auto-start and the update notice moved to `gmux open` (and session launches).

### Action flags replaced by verbs

Every removed flag prints an error naming its replacement — nothing silently changes behavior:

| Before | After |
|--------|-------|
| `gmux --list` / `-l` | `gmux ls` |
| `gmux --all` | `gmux ls --all` |
| `gmux --attach <id>` / `-a` | `gmux attach <id>` |
| `gmux --tail <id>` / `-t` | `gmux tail <id>` |
| `gmux --kill <id>` / `-k` | `gmux kill <id>` |
| `gmux --send <id> <text>` | `gmux send <id> <text> Enter` |
| `gmux --send --no-submit …` | `gmux send <id> <text>` (omit the trailing `Enter`) |
| `gmux --wait <id>` | `gmux wait <id>` |
| `gmux --no-attach <cmd>` | `gmux -d -- <cmd>` |
| `gmux --host <peer> …` | address the session as `<id>@<peer>` |

Note the **inverted `send` semantics**: 1.x auto-appended a newline unless `--no-submit`; 2.0 never auto-submits — add a trailing `Enter` key token to dispatch. Audit any script that pipes prompts.

### Daemon lifecycle fronted by `gmux daemon`

`gmux daemon start|stop|restart|status|log-path` is the canonical interface. The `gmuxd` binary keeps its verbs for service managers, so nothing breaks operationally — but update docs and scripts to the `gmux` spellings. `gmux auth` (token/pairing) and `gmux remote` (Tailscale setup) are top-level verbs.

### New verbs (not breaking, but update your habits)

- `gmux edit [file]` — managed editor sessions, usable as `$EDITOR`. Inside gmux sessions, `EDITOR`/`VISUAL` now default to `gmux edit` when your dotfiles don't set them — scripts that branch on `EDITOR` being empty inside sessions will see a value.
- `gmux send-keys -t <id> …` — tmux-compatible key sending.
- `gmux wait --timeout N` with exit codes `0` (idle/matched) / `2` (died) / `3` (timeout). `gmux wait --for-text S` / `--for-regex P` wait until output appears instead of the idle signal, and work for shell sessions too.
- `gmux send --wait [--timeout N]` fuses send-and-wait race-free (subscribes before delivering the input). Note `send`'s grammar: flags go **before** the id; everything after the id is verbatim, so `gmux send abc -v` sends a literal `-v` with no `--` guard needed.

---

## Multi-machine: tokens everywhere, no autodiscovery

**Who:** anyone with peered hosts, tailnet setups, or devcontainers. ([ADR 0008](https://github.com/gmuxapp/gmux/blob/main/docs/adr/0008-peer-authentication-via-token.md))

### All hosts must run 2.0

The wire protocol is v2-only: a 2.0 hub cannot aggregate a 1.x spoke and vice versa. **Upgrade every machine together, and rebuild devcontainers** so the feature installs a matching gmux.

### Tailnet identity no longer grants access

Before, passing the Tailscale allow list granted the full API. Now tailnet identity only gets you to the login page; every request additionally needs the host's bearer token — the same two-gate model as the browser. If you opened `https://gmux-<host>.ts.net` and got straight in, you'll now see the login page once: paste the token from `gmux auth` on that host.

### Tailscale peer autodiscovery removed

Before, gmux machines on your tailnet appeared as hosts automatically. Now peers are explicit: run `gmux auth` on the host, paste its connect URL into **Settings → Hosts → Connect to host**.

**Automatic migration on first 2.0 start:** hosts you had project references on are imported from the old discovery cache as **Auth needed** rows — click **Add token** on each to bring it back online (references keep resolving throughout). Unreferenced machines are dropped; re-add them on demand. The legacy cache is deleted, and `projects.json` is backed up to `projects.json.bak` first.

### Removed `host.toml` keys

These are **ignored with a warning** (not fatal), so an old config won't brick the daemon. Remove them to silence the warning:

| Key | Replacement |
|-----|-------------|
| `tailscale.hostname` | Name derives from the OS hostname (`gmux-<hostname>`) and is then owned by Tailscale. Seed a different name *before first registration* with `GMUXD_TS_HOSTNAME`, or rename in the Tailscale admin console. |
| `[[peers]]` | Runtime state in `peers.json`, managed via **Settings → Hosts**. |
| `discovery.tailscale` | Gone — add tailnet hosts via **Connect to host**. |

### Host renames no longer follow automatically

A peer's name is now frozen at first contact (ADR 0017): renaming a machine doesn't relabel your roster, and references keep working under the original label. Node IDs act as a liveness anchor — a removed-and-re-added host reclaims its references automatically.

---

## API & schema: terminology rename

**Who:** anyone parsing `gmux ls --json`, the REST API, SSE payloads, or `meta.json` files.

| Before | After |
|--------|-------|
| `"kind": "pi"` (session JSON) | `"adapter": "pi"` |
| `KIND` column in `gmux ls` | `ADAPTER` |
| `GET /v1/conversations/{kind}/{slug}` | `GET /v1/conversations/{adapter}/{slug}` |
| `"session_file"` | `"conversation_file"` |
| `resume_key` field | gone — use `conversation_file` (resume identity) and `slug` (membership/URLs) |
| `stale` field | gone — derive from `runner_version`/`binary_hash` vs `GET /v1/health` |

The daemon reads legacy `meta.json` keys and accepts the legacy runner `session_file` event for one release (dropped in v2.1) but writes/emits only the new names. The `GMUX_ADAPTER` env var is unchanged (it was already named that in 1.6). URL path segments (`/project/pi/slug`) are unchanged, so bookmarks keep working — though a Claude `/rename` now moves the slug with the title.

### Wire protocol v2

Custom consumers of the daemon SSE stream: the per-event `session-upsert`/`session-remove` surface and bulk-GET prefetch are gone. Subscribe to `GET /v1/events` and consume full-replacement `snapshot.sessions` / `snapshot.world` payloads plus lossy `session-activity` pings. `GET /v1/sessions` remains for one-shot listing.

### Same-origin enforcement

Cookie-authenticated mutations and WebSocket upgrades are now rejected cross-origin (`403 cross_origin`). Browser-based tooling on another origin must switch to bearer-token auth; reverse proxies that rewrite `Host` must forward the browser-facing host in `X-Forwarded-Host`. See [Security](/security/#browser-sessions-same-origin-enforcement).

---

## Adapter API (out-of-tree adapters & integrations)

**Who:** authors of custom adapters or tooling against `packages/adapter` / the runner socket.

- **Renames:** `SessionFiler` → `ConversationFiler`, `ParseSessionFile` → `ParseConversationFile`, `SessionFileInfo` → `ConversationInfo`, `SessionRootDir`/`SessionDir` → `ConversationRootDir`/`ConversationDir`; internal `Kind` → `Adapter`.
- **Removed capabilities:** `FileMonitor`, `FileAttributor`, `SessionFileLister`. Daemon-side file attribution and live tailing were replaced by runner-owned agent hooks (`SessionExtender` / `SessionHookCommand` + `POST /hook/event`) and adapter-owned `ConversationSource`s. There is no metadata-matching fallback: an unhookable tool runs without daemon-reported live state.
- **`Status.label` removed:** `Status` is only `{working, error}` booleans. Scripts that `PUT /status` with a `label` should drop it — display text is derived in the frontend.
- **Runner endpoints removed:** `GET /scrollback/text` and `GET /scrollback/tail` are gone from the runner socket. Use `gmux tail <id>` or gmuxd's `GET /v1/sessions/<id>/scrollback?tail=N` (works for dead sessions too).
- **New surface:** `GMUX_SESSION_SOCK` env var, `POST /hook/event` hook protocol (`docs/runner-hook-protocol.md`), `GMUX_NO_AGENT_HOOK` opt-out, `ConversationProber`, `PassthroughDetector`, `SessionRegistrar`/`SessionFinalizer`.

---

## Behavior changes

### Agent status is hook-driven

pi, Claude Code, and Codex now report status/titles/attribution through injected hooks instead of file watching:

- **Codex needs CLI ≥ 0.135.0** for live status; older versions launch fine but show no working/idle state.
- **Shell-wrapped launches** (`gmux -- bash -c 'claude'`) can't be hooked and run without live status.
- The agent's argv gains a hook argument (`-e <ext>` for pi, `--settings` for claude, `-c hooks.…` for codex). `GMUX_NO_AGENT_HOOK=1` launches the agent unmodified.

### Sessions and retention

- **Dead sessions persist across daemon restarts** (from `~/.local/state/gmux/sessions/<id>/meta.json`), and **dismiss is now permanent** — restarting the daemon no longer resurfaces dismissed sessions.
- gmux no longer surfaces conversations it never saw by scanning `~/.claude`/`~/.codex`/`~/.pi` into resumable sidebar entries; those files still power URL resolution and resume of known sessions.
- Retention caps apply: conversation-less dead sessions age out (30 days / 200 max), dead-session scrollback is capped at 256 MB aggregate. Override with `GMUX_SESSION_RETENTION_DAYS` / `GMUX_SESSION_RETENTION_MAX` / `GMUX_SCROLLBACK_CACHE_MB`.

### Per-session sockets moved

1.6 put runner sockets in shared `/tmp/gmux-sessions`; 2.0 uses `~/.local/state/gmux/run/sessions` (per-user, 0700). Tooling should read `$GMUX_SOCKET` instead of constructing paths. Legacy directories are scanned for one release so pre-upgrade runners survive; the shim disappears in v2.1. `GMUX_SOCKET_DIR` still overrides.

### Fresh login environment

Daemon-initiated launches (UI launcher, resume, restart) source a fresh `$SHELL -l -i` environment per launch instead of inheriting the daemon's frozen environment. Dotfile edits take effect on the next launch without a daemon restart. (No `$SHELL` — Docker/systemd — means unchanged behavior.)

### Devcontainer discovery requires the devcontainer label

Containers are only auto-discovered when they carry the `devcontainer.local_folder` label (set by the devcontainer CLI / VS Code) in addition to `GMUXD_LISTEN`. Plain `docker run` containers with `GMUXD_LISTEN` set are no longer picked up — add them as manual peers, or use the [Running in Docker](/running-in-docker/) flow.

### projects.json schema v3

Migrated automatically (with a `.bak` backup). The `hosts` match-rule field is dropped; cross-host projects are peer **reference items** (`{slug, peer, node_id}`). Tools that read or write `projects.json` must handle items without `match`. See [projects.json](/reference/projects-json/).

---

## UI changes (where did X go?)

Not breaking, but 1.x docs and muscle memory point at moved things:

- **Home screen** is now a pure activity dashboard (Waiting / Active / recency buckets). Host cards and quick-launch buttons are gone.
- **Project management** moved from the sidebar's "Manage projects" modal to **Settings → Projects** (gear button).
- **Hosts roster** lives in **Settings → Hosts**, with explicit Online / Connecting… / Auth needed / Offline statuses.
- **Mobile toolbar** reworked: dedicated ↑ ↓ and word-jump keys are always present; ctrl/alt arm-and-highlight instead of relabeling keys; paste moved off the toolbar (paste keybind or long-press).
- **Cmd/Ctrl+F** now opens find-in-terminal instead of browser find. Restore browser find with `{ "key": "secondary+f", "action": "none" }` in [`settings.jsonc`](/reference/settings/#keybinds-guide).

Nothing in `settings.jsonc` or `theme.jsonc` changed — old files parse identically.
