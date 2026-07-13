# ADR 0023: unified turn model — one session state machine, per-adapter turn sources

**Status:** Accepted
**Date:** 2026-07-13
**Related:** ADR 0011 (runner-owned session state), ADR 0013 (codex hooks), ADR 0015 (hook translation at the agent side), issue #373 (shell idle waits)

## Context

Session liveness signals were split by adapter kind, each with its own
mechanism and its own gaps:

- **Agent sessions** (pi/claude/codex) got full turn semantics from their
  hooks: `Status.Working` during a turn, unread ("waiting on you") and an
  optional error dot when the turn ends.
- **Shell sessions** got `Status.Working` from runner-tracked OSC 133 prompt
  marks (issue #373), but no unread on command completion, and the daemon
  gated `gmux wait` behind an adapter allowlist plus a per-session
  "prompt-mark evidence" check, rejecting everything else with a 422.
- **One-shot commands** (`gmux -d -- pnpm test`) — no prompt, no hook — had
  no state at all: never "active", never "waiting on you", not waitable.
- The base `Adapter` interface still carried `Monitor(output []byte) *Event`,
  a per-PTY-read inference hook that every built-in adapter stubbed with
  `return nil`; the `Event` type existed only for it.

## Decision

One turn state machine for every session — **active / idle / waiting on
you** — with the turn *source* chosen by adapter kind:

| Session kind | turn opens | turn closes |
|---|---|---|
| hook-driven adapter (`SessionExtender` / `SessionHookCommand`) | hook turn-start | hook turn-end |
| default model, upgraded by observed OSC 133 marks | `133;C` (command starts) | `133;A` / `133;D` (prompt returns) |
| default model, no marks ever (lifetime-as-turn) | process launch | process exit |

Rules:

1. **`adapter.HookDriven` is the only split.** Hook-driven adapters' turn
   state is owned by their hooks (ADR 0015), and prompt marks in their
   output are ignored (a tool the agent runs may print marks; they must not
   fight the hook). Every other adapter — shell, editor, future tools — gets
   the runner's default model with no opt-in: `Working=true` at launch,
   mark upgrade, exit close.
2. **Upgrade is evidence-based, not declared.** The first observed
   `133;A/C/D` mark switches the session to prompt-cycle turns for the rest
   of its life. Argv guessing was rejected: `bash -c script` (shell binary,
   but a one-shot) and `ssh host` with remote shell integration (non-shell
   binary, but mark-emitting) both classify correctly only by evidence.
   Sessions never change adapters mid-life, so there is no conflict window.
3. **Turn close ⇒ unread.** Every genuine working→idle transition sets
   "waiting on you", exactly like an agent's completed turn. The initial
   prompt (launch phase ending) does not. Lifetime-turn exits also set
   `Status.Error` on a non-zero exit code.
4. **Turn-state-at-death.** The exit event no longer clears `Status`; the
   persisted flag records whether the turn was closed when the process
   exited. `wait` resolves death by it: closed turn → `idle` (rc 0: one-shot
   completed, shell exited at its prompt, agent exited after its turn); open
   or never-demonstrated turn → `died` (rc 2). This makes the verdict
   timing-independent — a wait issued after the death answers the same as
   one that watched it live. The child's own exit code remains a separate
   fact on the session.
5. **Every session is waitable.** The daemon's `no_idle_signal` 422 (adapter
   allowlist + evidence gate) is deleted. A markless interactive shell is
   `Working` for its whole life, so a wait on it blocks honestly until exit
   or `--timeout` — "never provably idle" is answered by not answering, not
   by a rejection or a bogus instant idle.
6. **No per-byte adapter inference.** `Adapter.Monitor`, `Event`, `BoolPtr`
   and the `PromptSignaler` capability are deleted. Status truth has exactly
   three sources: hooks, the runner's default turn model, and the child's
   explicit `PUT /status`.

To make the model hold across daemon restarts and late subscribers, the
runner's `/events` endpoint now replays the current status snapshot on
subscribe (previously only the conversation ref and slug were replayed).

## Consequences

- `gmux -d -- pnpm test && gmux wait $id` works: active while running,
  `idle`/rc 0 on completion, "waiting on you" (+ error dot data on non-zero
  exit) in the sidebar.
- Agent contract change (accepted): an agent that finishes its turn and then
  exits cleanly resolves waits as `idle`/rc 0 instead of `died`/rc 2;
  mid-turn deaths remain rc 2.
- Dead sessions now carry a non-nil `Status`. The web UI already keys its
  working/error dots on `Alive` and derives "exited (N)" from the exit code,
  so nothing breaks; rendering a red dot for failed dead one-shots is a
  possible UI follow-up.
- The gray "activity" dot (raw output pulse) is untouched but now redundant
  for default-model sessions; consolidating the UI to a single "active"
  indicator is a follow-up.
- The base `Adapter` interface is four methods (`Name`, `Discover`, `Match`,
  `Env`); there is no status-related capability to implement at all.
