# ADR 0032: Session input doors and standard streams

**Status:** Accepted
**Date:** 2026-07-30
**Amended:** 2026-08-25 (definite pending stdin is refused before launch)
**Related:** ADR 0028 (CLI output channels)

## Context

gmux is a session manager, not a pipeline element. Every managed command runs
on a durable PTY so it can be watched, attached to, and replayed. The existing
non-interactive foreground path relayed cleaned PTY output and the exit code,
but silently ignored launcher stdin. It also selected transparent attach from
stdin alone, putting a terminal into raw mode even when stdout was redirected.

## Decision

1. **Launcher stdin is not session input.** Input enters only through a live
   attach (including the web UI), `gmux send`, or semantic agent verbs such as
   `gmux agent prompt`. In headless mode gmux neither reads nor forwards stdin.
   If launch-time inspection, without consuming bytes, finds pending data in a
   pipe or unread bytes in a regular file, gmux refuses the invocation before
   minting an ID, binding a socket, starting the child, or contacting gmuxd.
   Empty pipes, character devices such as `/dev/null`, sockets, unknown types,
   and inspection errors remain accepted because they are common harness launch
   sources and do not prove that data would be discarded.
2. **The PTY merge is part of stdout.** A child's stdout and stderr are one
   terminal stream, as with `ssh -t` or `script`. That combined payload is
   relayed on gmux stdout; gmux stderr remains the channel for the session id
   and diagnostics, including the pre-launch stdin refusal. This refines ADR
   0028 without changing its channel discipline.
3. **Transparent attach requires both stdin and stdout TTYs.** Otherwise gmux
   uses the headless relay: cleaned stdout, metadata and diagnostics on stderr,
   no stdin relay, and no raw terminal mode. The same predicate governs nested
   gmux auto-detach. stderr TTY status is irrelevant.
4. **Sessions always use a PTY.** There is no pipes mode and no `-i`/`-t` flag
   matrix. A command requiring ordinary pipeline semantics belongs outside a
   managed session (or in an existing passthrough adapter).
5. **The input doors are the contract.** Help and diagnostics point to attach,
   `send`, and agent prompting. The scripted pipeline-shaped recipe is
   `id=$(gmux -d -- cmd); gmux send "$id" 'input' Enter`.

## Conventions survey

- **tmux/screen: follow.** Server-owned terminals receive input through attach
  or send-keys. gmux's additional foreground wait makes proven launch-time data
  loss actionable: pending stdin is refused before the session exists.
- **ssh: differ deliberately.** ssh owns one command invocation; gmux owns a
  durable session with later and concurrent writers. ssh's `-n` also shows how
  implicit stdin coupling harms scripts.
- **docker: follow the default.** Without `-i`, launcher stdin is disconnected.
  gmux's session-addressed input doors remove the need for an `-i` axis.
- **script/unbuffer/expect: differ.** Their PTY disguises one invocation and
  assumes one writer and matching lifetimes; a gmux PTY is durable state.
- **nohup/setsid/systemd-run/CI: follow.** Managed or detached work must not
  unexpectedly depend on launcher stdin, and interactivity limitations belong
  in diagnostics.

## Rejected alternatives

- **Forward stdin with EOF-to-VEOF translation.** Launches from `/dev/null`, CI,
  cron, and common agent harnesses would inject immediate `^D` and terminate
  agents. `while read; do gmux -- …; done` would consume the loop's input. The
  implementation would also require slave-termios VEOF lookup, unterminated
  line handling, backpressure, and detach machinery for a pipeline use case
  that sessions do not serve.
- **Forward without EOF translation.** It retains stdin stealing while leaving
  draining children unable to observe EOF.
- **Refuse every non-TTY stdin source.** Agent harnesses routinely provide an
  empty pipe or `/dev/null`; refusing only input that is definitely pending
  prevents proven data loss without rejecting those ordinary launches.
- **No PTY or a separate child-stderr channel.** Either removes attachable
  terminal semantics or destroys the session's ordered terminal truth.
- **Inject VEOF at launch.** This kills agents and later-send workflows.
- **Adapter-specific forwarding.** Stream semantics must not depend on command
  classification.

## Consequences

`printf hi | gmux -- cat` now fails synchronously instead of creating an idle
session whose `cat` can never receive the supplied bytes or EOF. The refusal
consumes no input and creates no session artifacts. Empty harness pipes remain
accepted. Redirecting stdout selects a cleaned headless relay and never alters
the caller's terminal. Child-stderr bytes are intentionally part of stdout,
while session IDs and gmux diagnostics remain on stderr. In headless foreground
mode, SIGINT shuts down the session rather than forwarding a terminal
keystroke. Passthrough commands, terminal sizing, and daemon behavior are
unchanged; explicit detached launches apply the same stdin preflight before
re-execing the runner.
