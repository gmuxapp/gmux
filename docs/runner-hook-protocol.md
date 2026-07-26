# Runner hook protocol: tool-neutral authoritative session events

**Status:** Stable · **Related:** ADR 0011, `cli/gmux/internal/ptyserver`

The contract an agent implements to report its session state to the gmux runner
authoritatively. Tool-neutral: the runner makes no per-adapter assumptions in
`handleHookEvent`. pi's extension (`agentext/pi-ext.mjs`) is the reference; the
protocol is not pi-specific.

Per ADR 0011, live state is runner-owned. An agent reports its own facts (held
file, turn phase) rather than the daemon inferring them from fs scans/scrollback
— and a hook even catches a cache-served `/resume` that reads no file. The
runner only **relays** these facts; the one bit of state it keeps is a snapshot
replayed to `/events` (so a restarted daemon re-learns attribution), never used
to guess.

**The hook is the adapter's agent-side translation surface** (ADR 0015). It
translates the tool's *native* events into the `{op}` contract below, in
whatever language the tool dictates — JS in pi's extension (typed access to
pi's API), Go for codex/claude (in the adapter package). It does **not** forward
raw tool events for the runner to parse: translating at the typed-access point
keeps this wire a small, stable contract instead of the tool's churny internal
event model, and keeps `handleHookEvent` tool-neutral. So tool-specific logic
(e.g. pi deriving a first-user-message title until pi names the session) lives
in the hook, mirroring what codex/claude already do.

## Transport

- Runner exports `GMUX_SESSION_SOCK` (its Unix socket) to the agent env.
- Agent POSTs JSON to `POST /hook/event`, **fire-and-forget**: a failed POST
  must never surface into the agent; the next event re-establishes truth.
- Socket is owner-only (0o700).

## Event schema

One JSON object per event, discriminated by `op`. Unknown ops/values are ignored
(forward-compatible); zero-value fields are no-ops.

```jsonc
// op "ready" — the agent can accept input (its composer/input handlers are
// installed). Gates gmux's semantic agent actions (ADR 0027); see below.
{ "op": "ready" }

// op "session" — authoritative bind. Sent on startup and on every rebind
// (switch/new/resume/fork). "session" here denotes the agent's own session
// (its conversation) — the agent's language, per ADR 0015; gmux calls the
// bound artifact's locator a conversation ref (ADR 0022).
{
  "op":     "session",
  "path":   "/abs/path/to/conversation-file",  // required; the conversation ref (a transcript path for today's file-backed agents)
  "id":     "session-id",                       // optional; adapter session id, informational
  "slug":   "human-title",                      // optional; slug source (runner slugifies); omit until titled
  "name":   "human title",                      // optional; sets the adapter title
  "cwd":    "/project/dir",                      // optional; accepted, not yet applied
  "reason": "startup|new|resume|fork|activity"  // optional; informational
}

// op "turn" — agent loop boundary.
{ "op": "turn", "phase": "start" }                            // → active
{ "op": "turn", "phase": "end", "outcome": "completed",       // see vocabulary
  "title": "human title" }                                    // optional
```

### Field reference

| Field     | Op       | Meaning |
|-----------|----------|---------|
| `path`    | session  | The held conversation's ref — adapter-opaque above the runner (ADR 0022); today's file-backed agents report the transcript's absolute path. |
| `id`      | session  | Adapter's session id. Informational — the runner keys on the gmux session id, and never derives a slug from this (it's a UUID for real adapters). |
| `slug`    | session  | Slug source, slugified by the runner. Send only once the session has a title; while omitted the runner leaves the slug empty and the web layer falls back to the gmux session id itself for the URL. |
| `name`    | session  | Display title at bind time. |
| `cwd`     | session  | Project dir. Accepted for forward-compat but not applied — the runner knows the launch cwd. |
| `reason`  | session  | Why the bind happened; informational. |
| —         | ready    | No fields. |
| `phase`   | turn     | `"start"` or `"end"`. |
| `outcome` | turn end | Normalized terminal state — see below. |
| `title`   | turn end | Display title at turn end. |

### Outcome vocabulary

Stable and agent-agnostic; each hook normalizes its native state into one. The
outcome→sidebar mapping is gmux policy in the runner (`applyTurnEnd`), not the
agent's concern.

| Outcome       | Meaning                          | Sidebar                |
|---------------|----------------------------------|------------------------|
| `completed`   | Agent finished its own turn.     | idle + **unread**      |
| `interrupted` | Human or agent stopped the turn. | idle + **interrupted** |
| `error`       | Agent gave up.                   | idle + **error**       |

### Ordering requirement

A terminal end applies **only while a turn is open**: the runner ignores an end
that arrives against an already-closed turn, so a duplicate or an
unconditional-on-exit hook (Claude's `SessionEnd` after `Stop`) cannot rewrite a
good closure. This is turn *polarity*, not turn *identity* — the runner cannot
recognize a logically stale end that arrives after the next turn already
started, and would close the new turn with it.

Hooks must therefore deliver events **in order**: never issue event N+1 before
N has been delivered. pi's extension serializes its POSTs on a single promise
chain. Claude's own hook execution is the agent's business and is not uniformly
blocking — `Stop` is awaited, while `StopFailure` output is documented as
ignored — so ordering between a turn end and a near-simultaneous `SessionEnd`
is not guaranteed there. An agent that cannot guarantee ordering needs a turn
token, which this protocol does not have yet.

## Readiness (`op: "ready"`)

The runner's semantic action routes (`POST /prompt`, `POST /cancel` — ADR 0027)
refuse to write a single byte until the agent has reported `ready`, then wait up
to the adapter's `ActionReadyTimeout` (pi: 10s) and give up **without
delivering anything**, so a caller's retry cannot duplicate a prompt.
Raw `POST /input` is unaffected: raw input is unconditional and always
immediately available.

Both semantic routes also require the caller to name the runner process it means,
in `X-Gmux-Expect-Incarnation` (the value the runner reports as
`X-Gmux-Incarnation`). A socket pathname is reusable, so the process answering it
may not be the one the caller decided about; a mismatch is refused with
`409 incarnation_mismatch` **before anything is written**, which makes it the one
delivery failure that is a guaranteed non-delivery and therefore safe to retry
against the current occupant. A missing header is a caller bug
(`400 invalid_request`), not an older client: `/prompt` and `/cancel` shipped
together with this check, so a runner that serves them at all enforces it, and an
older runner answers 404/426 instead.

Rules for hook authors:

- **Report it as early as the composer is genuinely usable**, and *independently
  of the conversation*. A brand-new session usually has no conversation file
  yet; gating readiness on a bind would deadlock the session's first prompt.
  pi's extension posts `ready` first thing in `session_start`, which pi fires
  after installing the editor, key handlers and submit handler.
- **Repeat it freely.** The runner is idempotent about it and releases every
  waiting request at once. Reporting on every bind is the recommended shape:
  a rebind is evidence the composer is alive.
- **It is generation-local.** The runner never persists readiness, never emits
  it as session status, and never resets it on a conversation rebind (same
  process, same composer). Only a new runner process starts unready.

### `turn` start is load-bearing for prompt delivery

Once the runner delivers a semantic prompt, it holds a **delivery reservation**
and refuses further prompts that require an idle agent, so a caller cannot
duplicate text into the same composer. The reservation is released by exactly
one thing: an **inactive→active edge that happens after the delivery** — in
practice a fresh `{"op":"turn","phase":"start"}` reported while the session was
idle.

The precision matters:

- a **repeated** `active` report about a turn that was already running (a hook
  re-reporting, a script's `PUT /status` mid-turn) is not new information and
  releases nothing — which is what lets a queued `after_turn` prompt keep its
  reservation until its *own* turn starts;
- an edge that happened **before** the delivery belongs to another turn and is
  never counted for it;
- **inactive** reports — a turn end, an idle `PUT /status`, a cleared status —
  are not evidence that the delivered prompt was consumed;
- there is deliberately **no timeout**: the passage of time proves nothing about
  whether the agent received the text.

Scope, so the guarantee is not overread: it holds **among gmux's semantic
requests, absent concurrent raw input**. Raw `POST /input` and a human at the
terminal write unconditionally and take part in none of this, so a turn a human
starts while a delivery is in flight can be credited to that delivery — the edge
is causally ambiguous without a turn token, and gmux resolves the ambiguity
toward availability rather than wedging the common case where the edge really is
the delivered prompt's turn.

So a hook that reports turn ends but never turn starts will accept one semantic
prompt per runner generation and refuse the rest with `delivery_pending`. Report
the start. (Raw `POST /input` stays unconditional throughout, and is the escape
hatch for a session in that state.)

## The runner does NOT, for hooked sessions

Parse the conversation file, infer status from PTY/scrollback, apply per-adapter
heuristics in `handleHookEvent`, or use the `conversation_file` snapshot for anything
but `/events` replay.

## Implementing for a new agent

1. **Load the hook** via the seam matching how the agent loads extensions
   (below). Both are ephemeral, scoped to the launch, and no-op without
   `GMUX_SESSION_SOCK`.
2. Report a `session` event on every bind.
3. Report `turn` start/end, normalizing to the outcome vocabulary.

### Injection seams

- **`SessionExtender`** (pi): the runner materializes the embedded pi extension
  and splices `pi -e <path>` into the argv.
- **`SessionHookCommand`** (codex): the runner injects a `gmux __codex-hook`
  command hook via the agent's config-override flags (`-c hooks.<Event>=...`),
  with the gmux binary itself as the hook program. It also carries the per-hook
  `trusted_hash` codex computes so only gmux's own hooks are trusted (never the
  global `--dangerously-bypass-hook-trust`). Version-gated; older codex (or a
  hash mismatch) injects nothing and the session runs without daemon-reported
  live state — there is no metadata-attribution fallback.
- **`SessionHookCommand`** (claude): Claude Code takes hooks through settings,
  so the runner splices `--settings <inline-json>` (a `gmux __claude-hook`
  command hook). That layer merges with the user's settings and hook arrays
  concatenate, so gmux's hooks add to rather than clobber the user's.
