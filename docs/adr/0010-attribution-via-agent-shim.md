# ADR 0010: Authoritative session attribution via an agent-shim

**Status:** Accepted
**Date:** 2026-06-16
**Related:** ADR 0002 (project ownership from session origin), ADR 0003
(resume by id passthrough), ADR 0004 (SessionStream)

## Context

The sidebar is **conversation-keyed**: a slot means "this is conversation
X" so gmux can monitor X's session file for activity and resume the right
conversation if the runner dies. But gmux does **not** own the conversation
identity — pi/claude/codex choose their session file *after* launch (the
file is written only once there is an event), so the identity does not exist
at spawn time.

Historically gmux **guessed** the conversation by matching a runner's
terminal **Scrollback** against candidate adapter session files
(`FileAttributor`, content-similarity scoring). This guess is:

- **post-hoc and slow** — it runs on a throttled timer after the runner
  registers, and only once enough text exists to score;
- **ambiguous** — small or overlapping sessions score poorly or collide,
  with no abstain-on-tie guard;
- **blind to rebind** — a runner can `/resume` a different conversation
  mid-life with no signal;
- **blind to N:1** — the model assumed one runner per conversation.

These manifested as sidebar bugs (sessions disappearing on relaunch,
dismissed sessions reappearing, renamed sessions sorting to the bottom)
that are attribution races in storage costume.

## Decision

1. **Attribution is authoritative, sourced from the agent itself.** The
   runner injects a small, readable JS preload — the **Agent-shim**
   (`cli/gmux/internal/agentshim/hook.mjs`) — into node/bun agent
   processes via append-safe env:
   - node: `NODE_OPTIONS="… --import file:///abs/hook.mjs"`
   - bun: `BUN_OPTIONS="… --preload /abs/hook.mjs"`

   The shim wraps the agent's `fs` write surface and, on every `*.jsonl`
   write, POSTs `{op,path,pid}` to the runner's Unix socket
   (`POST /shim/event`). The runner records it on session state and emits
   a `session_file` event; the daemon turns it into the attribution
   (`FileMonitor.AttributeFromShim`). The agent told us which file it
   holds — no guessing.

2. **Injection is adapter-gated.** A new `adapter.SessionShimmer`
   capability opts an adapter in (pi today; claude/codex are one-liners).
   Shells and other non-jsonl adapters are never shimmed — injecting
   `NODE_OPTIONS` into a shell would leak the preload into every node
   process the user runs there.

3. **Writes only; reads are ignored.** A `/resume` picker `readdir`s and
   bulk-reads every session file — pure noise. A real resume/rebind always
   ends in a *write* to the chosen file, so writes alone catch both
   first-attribution and rebind.

4. **Hello-gating.** The shim announces itself (`hello`) at startup, before
   any write. The daemon marks the session shim-covered and **suppresses
   scrollback** for it until the real file is reported, closing the
   pre-write window where scrollback would mis-attribute a fresh session
   to a stale file.

5. **Re-announce on reconnect; no persisted attribution.** The runner
   replays its current shim state to every new `/events` subscriber, so a
   restarted daemon re-learns attribution the instant it resubscribes.
   This retires `attributions.json` — there is no persisted attribution
   state. Unshimmed sessions re-derive via the fallback.

6. **Scrollback is the demoted fallback.** `GMUX_RUNNER_SOCK` is set by the
   runner (its own socket) at spawn, so the shim always reports to the
   runner and the runner always replays — daemon start/restart timing never
   opens a gap. The fallback is reached only for agents the shim can't
   cover: non-node/bun runtimes (e.g. a Rust `codex`) and runners spawned by
   a pre-shim gmux during a rolling upgrade. Note the genuinely fuzzy path —
   scrollback content-matching — is **pi-only**; codex and claude attribute
   by file metadata (cwd + timestamp). All of it is annotated
   `Deprecated`/FALLBACK and logs a `(FALLBACK)` marker so reliance can be
   measured.

## Consequences

- Attribution is **instant** (on first write) and **correct across
  rebind** (`/resume`); the throttle/score-gate window that drove the
  sidebar races is gone for shimmed agents.
- The shim is **tool-agnostic** — the same code serves any JSONL agent —
  so the per-adapter attribution surface collapses toward a thin
  path-recogniser + line-parser.
- The shim ships as a third, intentionally **readable** artifact, embedded
  via `go:embed` and materialised to a content-addressed cache path.
- **N:1 is now observable**: each runner reports independently. The
  `attributions` map is still 1:1 (filePath→sessionID), so two runners on
  one conversation resolve to **last-binder-wins** with no conflict.
  Representing true N:1 in the data model is future work, now unblocked by
  authoritative per-runner reporting.
- **Caveat:** the fallback is reached only for agents the shim can't cover
  (non-node/bun runtimes; pre-shim runners mid-upgrade). The `(FALLBACK)`
  log surfaces how often that happens.

## Alternatives considered

- **Keep improving scrollback matching** (margin guards, larger windows).
  Rejected as primary: it is fundamentally a post-hoc guess and cannot see
  rebind or N:1. Retained only as the fallback.
- **Run `/session` in the agent to print the filename.** Intrusive (injects
  into the live PTY, races user input, pi-version-specific). Last resort.
- **A pi extension** pushing the conversation id. Authoritative but a
  user-install and pi-specific; the shim delivers the same signal for any
  node/bun agent with no install.
- **Read process memory / hook the launch.** Invasive and fragile across
  versions; the shim is the clean equivalent.
- **CLI-arg rewriting** (`pi -e ext args`). Couples us to each agent's CLI
  surface and special cases (`update`, `--help`); the env-preload is
  arg-agnostic.
