# ADR 0013: codex reports session state authoritatively via codex hooks

**Status:** Accepted
**Date:** 2026-06-21
**Related:** ADR 0011 (runner-owned session state), `docs/runner-hook-protocol.md`

## Context

ADR 0011 made live session state runner-owned and pushed by an **agent hook**:
pi loads the gmux extension via `pi -e <ext>` and POSTs authoritative
session/turn facts to the runner socket. It explicitly noted codex as the
holdout — *"codex remains daemon-parsed via the file-watch fallback until it
grows a comparable hook (its own Rust process can't load a node/bun
extension)"* — leaving the daemon's metadata attribution + file-watch
(`filemon.go`, `FileAttributor`) as the last per-adapter inference path.

That premise no longer holds. **Codex has grown a first-class command-hook
system** (`codex-rs/hooks`): 10 lifecycle events, each able to run an external
**command** that receives the event as JSON on stdin. `SessionStart`,
`UserPromptSubmit`, and `Stop` carry `session_id`, `transcript_path`, `cwd`, and
`source` — everything needed to attribute the held conversation file and drive
turn status, the same facts pi reports. (Verified against the codex source, and
against two shipping integrations, cmux and ghostex, which use the same pattern.)

## Decision

Make codex authoritative via its hooks, reusing the tool-neutral `/hook/event`
protocol unchanged. Concretely:

1. **New injection seam: `adapter.SessionHookCommand`.** Unlike pi's argv splice
   (`SessionExtender`), codex loads hooks from its **config**, not a loader
   flag. But codex accepts hook definitions via per-invocation `-c` config
   overrides (a `SessionFlags` config layer): `hooks` is a real `ConfigToml`
   field, and `-c key=value` parses the value as a full TOML value (inline
   arrays/tables allowed). So the runner injects, **per launch**, on the codex
   argv:
   `-c 'hooks.SessionStart=[{hooks=[{type="command",command="gmux __codex-hook SessionStart",timeout=5}]}]'`
   (and likewise for UserPromptSubmit/Stop), plus the trust-bypass flag, and
   sets `GMUX_SESSION_SOCK`. This is **ephemeral and scoped to the gmux-launched
   process — it does not touch the user's `~/.codex`**, matching pi's `-e`.
   (Earlier drafts installed a hook into `~/.codex/hooks.json`, as cmux/ghostex
   do; the `-c` path is strictly less invasive and was chosen instead.)

2. **The hook program is the gmux binary itself** (`gmux __codex-hook <Event>`,
   a hidden subcommand). codex pipes its event JSON to stdin; gmux translates it
   to `/hook/event` bodies and POSTs them to the runner socket. It is
   fire-and-forget, always exits 0, and **no-ops unless `GMUX_SESSION_SOCK` is
   set** — so the globally-installed hook is inert for plain `codex` runs
   outside gmux.

3. **Title/slug come from the transcript.** codex events carry no title, and its
   `session_id` is a UUID that slugifies into an unreadable URL. The hook parses
   the transcript's first user prompt (reusing `ParseSessionFile`) and reports a
   human title plus an explicit title-derived **`slug`** — a small, tool-neutral
   addition to the protocol (the runner prefers an explicit `slug` over
   `Slugify(id)`; pi is unchanged).

4. **Trust scoped to our own hooks via injected `trusted_hash`.** A CLI-injected
   hook is non-managed and therefore `Untrusted`, which codex silently skips.
   The blunt instrument, `--dangerously-bypass-hook-trust`, is a **process-global
   flag**: codex applies it to every non-managed hook source (the user's own
   hooks, installed plugins, and project `.codex/` hooks in already-trusted
   directories), collapsing codex's per-hook trust layer for the whole
   invocation. **Rejected.** Instead gmux injects, alongside the hook
   definitions, a `-c hooks.state={...}` override carrying the exact per-hook
   `trusted_hash` codex computes (`version_for_toml` = sha256 of the canonical,
   sorted-key JSON of the normalized hook identity), keyed by codex's hook_key
   (`/<session-flags>/config.toml:<event>:0:0`). codex reads hook trust state
   from the SessionFlags layer, so **only gmux's own three benign reporting
   hooks become Trusted; every other hook keeps codex's full trust gating.**
   `GMUX_NO_AGENT_HOOK` disables the whole path.

   This reproduces a codex *internal* (the hash + key formats), not a stable
   contract, so it can drift across codex releases. The failure mode is safe by
   construction: a mismatched hash/key leaves our hook `Untrusted` → skipped →
   daemon attribution takes over. It degrades; it never broadens trust. A Go
   test pins the canonical-JSON form so our side can't drift silently.

   Note codex's *directory* trust is a separate, earlier gate: project `.codex/`
   config layers are loaded-but-disabled in untrusted directories and never
   reach hook discovery, so this scheme cannot cause a hook from an untrusted
   repo to run regardless.

5. **Version-gated, fallback retained.** The hook integration is gated on codex
   **≥ 0.135.0** (hooks documented and stable; the hook config/trust shapes we
   depend on are present). Below the floor — or if `codex --version` can't be
   read — gmux launches codex unmodified and the daemon's existing metadata
   attribution + `ParseNewLines` stay in charge. The `-c hooks…` overrides would
   make an older codex reject the launch, so the gate is load-bearing, not
   cosmetic. The codex adapter keeps `FileAttributor`/`FileMonitor` for exactly
   this reason.

## Consequences

- On codex ≥ 0.135, attribution and status are push-based and exact, like pi:
  the hook reports the held file even for a cache-served resume, and the daemon
  suppresses its file parse for hook-attributed sessions (the existing
  `AttributeFromHook` path, adapter-independent).
- Turn-end status is coarser than pi's. Codex's `Stop` hook input carries no
  outcome field (`stop.command.input.schema.json`: 9 fields, none about how the
  turn ended), so the hook always reports `completed` — a user interrupt or
  error exit shows as completed+unread, not idle/error. Revisit if codex adds an
  outcome-bearing Stop field.
- The daemon's metadata-attribution engine (`filemon.go` fallback,
  `FileAttributor`) is **no longer codex's only path** — but it is still
  required for older codex. It can be removed only once a hooks-capable codex is
  the supported floor. This ADR records that as the explicit precondition for
  the deferred `filemon-simplify` work; this PR does not touch it.
- gmux does **not** write to the user's codex config. The hook exists only for
  the duration of a gmux-launched codex process (injected via `-c`); plain
  `codex` runs are completely unaffected, and there is nothing to clean up or
  uninstall.
- Caveat: a `-c hooks.SessionStart=…` override sits in the `SessionFlags` config
  layer, which takes precedence over the user's `config.toml` inline
  `[hooks].SessionStart`. Hooks the user defines in a separate `hooks.json` load
  via a different discovery source and still run; only a user's *inline
  config.toml* SessionStart/UserPromptSubmit/Stop hooks could be shadowed, and
  only during gmux-launched codex. Merging the user's inline hooks into the
  override is possible but reintroduces config reading; deferred as unnecessary.

## Alternatives considered

- **`--dangerously-bypass-hook-trust`.** Simple and robust to codex version
  changes, but process-global: it un-gates the user's, plugins', and
  already-trusted projects' hooks for the whole invocation, collapsing codex's
  per-hook trust defense. Rejected — see decision 4. (The chosen `trusted_hash`
  approach is the "scope it to our hook" alternative, accepting the hash
  fragility precisely because it fails safe.)
- **Managed hooks via `requirements.toml`** (`is_managed` ⇒ always trusted, no
  hash). The robust "automation" path, but `requirements.toml` is an
  admin/MDM-flavored, system-scoped mechanism, and managed hooks can't be
  expressed as an ephemeral per-launch `-c` override. Left as a possible future
  hardening.
- **Installing a hook into `~/.codex/hooks.json`** (the cmux/ghostex approach).
  Works, and preserves the user's other hooks via a merge, but is a global,
  persistent mutation of the user's config that outlives the gmux session.
  Rejected in favour of ephemeral `-c` injection.
- **`CODEX_HOME` redirection to a gmux-managed config layer.** Would avoid
  touching the user's `~/.codex`, but `CODEX_HOME` *replaces* rather than layers
  — it would orphan the user's sessions, auth, and history. Rejected.
- **The older `notify` program** (`agent-turn-complete` only). Too coarse: no
  session-bind event, no transcript path. Superseded by hooks.
