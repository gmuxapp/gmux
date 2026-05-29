# ADR 0006: Daemon-spawned sessions source a fresh login environment

**Status:** Accepted
**Date:** 2026-05-29
**Related:** ADR 0003 (daemon-initiated resume / restart passes Session.ID), ADR 0005 (CLI routes through gmuxd)

## Context

Every managed session's environment is, today, a frozen copy of the
daemon's environment captured once at daemon startup. The chain:

1. `gmuxd` captures `os.Environ()` when its process starts. The
   daemon is long-lived — typically auto-started by the first
   `gmux` invocation (`ensureGmuxd`), inheriting that terminal's
   env, and then never restarted for days.
2. `launchGmux` forks every runner — for `/v1/launch`,
   `/v1/resume`, **and `/v1/restart`** — with
   `cmd.Env = sessionenv.Strip(os.Environ())`, i.e. the daemon's
   frozen env minus session-identity vars (see the env-leak fix
   that introduced `packages/sessionenv`).
3. The runner builds its child's env via
   `buildChildEnv(os.Environ(), ...)` — its own env, inherited from
   the daemon — overlaying per-session `GMUX_*`, adapter, and
   terminal-capability vars.

The net effect: a session's environment is whatever the daemon's
environment was at daemon-start time, possibly many days and many
dotfile edits ago.

This produces a confusing, recurring symptom:

- A user edits `~/.zshrc` / `~/.bashrc` / `~/.profile` (adds a
  `PATH` entry, a tool shim, an `export`), then **clicks "Restart
  session"** expecting the session to pick it up. It doesn't. The
  restarted runner inherits the same frozen daemon env.
- Worse, the obvious remedy — `gmuxd restart` — only refreshes the
  env if run from a *pristine* login shell. Run from a terminal
  *inside* a gmux session (the common case when living in the
  tool), it re-freezes the already-stale session env, because that
  shell carries the daemon's frozen env to begin with.

The root cause is that the env is *captured once and frozen*, with
no path that re-sources the user's actual login environment. The
"Restart session" button is the sharpest edge: its whole purpose is
to give the user a clean slate, but it silently reuses the stale
env.

## Decision

`launchGmux` **sources a fresh interactive-login shell environment
at launch time** and hands that to the runner, instead of the
daemon's frozen `os.Environ()`. This applies to all three
daemon-initiated spawn paths (launch, resume, restart) since all go
through `launchGmux`. Terminal-initiated paths (`gmux foo`,
`--no-attach`, nested gmux via `spawnDetached`) are unchanged: they
already re-exec from the user's *live* terminal env, which is fresh
by definition.

### Source: an interactive login shell

```
$SHELL -l -i -c '<gmuxBin> --dump-env'
```

- **Login (`-l`)** reads `~/.profile` / `~/.zprofile` /
  `~/.bash_profile`.
- **Interactive (`-i`)** reads `~/.zshrc` / `~/.bashrc` — where
  most users actually put `PATH`, `export`s, and tool shims
  (nvm, mise, pyenv, …). Login-only would still look stale to most
  users.

Together this matches "what a freshly-opened terminal sees", which
is the mental model the Restart button should satisfy.

### Extraction: self-exec dumper over fd 3

A hidden `gmux --dump-env` mode writes `os.Environ()` as
NUL-delimited entries to **file descriptor 3** — a pipe the daemon
passes via `cmd.ExtraFiles[0]`. The shell's rc-file banners,
MOTDs, prompts, and `direnv` notices all go to stdout/stderr and
never touch fd 3, so there is nothing to frame, mark, or parse out
of noise.

```go
// daemon side (sketch)
pr, pw, _ := os.Pipe()
cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c", shellQuote(gmuxBin)+" --dump-env")
cmd.Dir = cwd                 // session launch cwd (see below)
cmd.Stdin = devNull
cmd.Stdout, cmd.Stderr = nil, nil
cmd.ExtraFiles = []*os.File{pw}            // → fd 3 in the child
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
// start, close parent's pw copy, read pr to EOF, parse NUL, wait
```

```go
// gmux --dump-env (sketch)
f := os.NewFile(3, "env")
for _, e := range os.Environ() {
    f.WriteString(e)
    f.Write([]byte{0})
}
f.Close()
```

This reuses the established `spawnDetached` handshake-fd pattern
(daemon passes a pipe as `cmd.ExtraFiles[0]`/fd 3). `--dump-env` is
a daemon-internal flag, matching the `--resume-id` /
`--initial-cols` convention from ADR 0003 — greppable in `ps`,
tagged daemon-internal in `--help`, and dispatched before the run
path so it is never mistaken for a command to exec.

The one assumption is that fd 3 survives through
`$SHELL -l -i -c '<gmux> --dump-env'`. This is standard POSIX
behavior (the shell execs the command with inherited fds ≥ 3). If
it ever doesn't, the read returns empty and we hit the fallback.

### Merge, not from-scratch

The capture shell **inherits the daemon's environment** as its
starting point (no `env -i`), then layers rc-file changes on top.
The result is `daemon-env ∪ rc-file-changes`:

- Session/PAM-provided vars the dotfiles never set survive —
  `DISPLAY`, `SSH_AUTH_SOCK`, `XDG_RUNTIME_DIR`,
  `DBUS_SESSION_BUS_ADDRESS`. A from-scratch capture would lose
  these and break GUI apps, ssh-agent, and the runtime socket dir.
- Fresh `PATH` / exports / tool shims from `.zshrc` apply on top.

Accepted tradeoff: a variable *removed* from dotfiles still lingers
(rc files add/override but don't un-set what they no longer
mention). This matches how a freshly-opened terminal behaves (it
too inherits the session env) and is low-impact compared to losing
the session vars.

This also means **zero protocol change**: `launchGmux` sets
`cmd.Env = sessionenv.Strip(captureLoginEnv(cwd))`; the runner's
`os.Environ()` becomes that, and `buildChildEnv` still overlays the
per-session `GMUX_*` / adapter / terminal vars. `sessionenv.Strip`
is still applied so any session-identity vars that leaked into the
captured env are dropped, while `GMUX_SOCKET_DIR` (config) is
preserved.

### Working directory: the session's launch cwd

The capture runs in the session's launch cwd, so per-directory env
hooks (`direnv`/`.envrc`, `mise`) that the user has intentionally
set up are honored. Users who have such hooks accept the extra
startup cost; `direnv`'s "blocked" notices land on stderr and are
discarded.

### Robustness: synchronous, bounded, fail-safe

- **Synchronous** inside `launchGmux` / the HTTP handler. Launch,
  resume, and restart are deliberate, infrequent user actions;
  the `/v1/restart` handler already blocks up to 5s waiting for the
  old runner to die, so multi-second handler latency is not new.
- **5s hard timeout**, injectable for tests. Generous enough that
  legitimately-slow shells (nvm/mise) complete rather than falsely
  timing out into stale env.
- **Process-group kill** on timeout: the shell runs in its own
  process group (`Setpgid`), and expiry sends `SIGKILL` to the
  group (`syscall.Kill(-pid, SIGKILL)`) so rc-spawned children
  cannot be orphaned or hold the read open. Same pattern as
  `ptyserver.handleKill`.
- stdin ← `/dev/null`, stderr discarded.

### Fallback: degrade to today's behavior

Any failure falls back to `sessionenv.Strip(os.Environ())` — the
daemon's current env, i.e. exactly today's behavior — with a
warning logged. Two invariants:

1. **Never produce a worse env than today.** A failed refresh must
   never break a launch or strip vars the session would otherwise
   have.
2. **Never block forever.** The timeout + process-group kill bound
   the wait.

Trigger conditions for fallback: `$SHELL` unset (typical for
systemd/Docker daemons, where a login shell is meaningless — we
skip rather than guess `/bin/sh -l -i`); `gmuxBin` unresolved;
shell exits non-zero; empty/short read; timeout.

### Always-on, gated by `$SHELL`

No config toggle. `$SHELL`-unset already skips capture, covering
headless deployments; for interactive users this is the desired
default. A toggle is cheap to add later if a real need surfaces;
pre-adding it is speculative config surface.

## Consequences

### Positive

- **Restart actually refreshes the environment.** The button's
  purpose — a clean slate — is satisfied: a restarted session sees
  current dotfiles, like a freshly-opened terminal.
- **No daemon restart required** to pick up env changes for new or
  restarted sessions.
- **No new persistent state, no protocol change.** The fresh env
  threads through the existing `cmd.Env` → runner `os.Environ()` →
  `buildChildEnv` chain.
- **Headless deployments unaffected.** `$SHELL`-unset skips capture
  and inherits the daemon env unchanged, as before.

### Negative

- **Per-launch shell-startup cost.** Every daemon-initiated launch
  now forks an interactive-login shell (typically 100–500ms, up to
  1–2s for heavy rc setups). Accepted: these are infrequent,
  deliberate actions, and caching was explicitly rejected as
  reintroducing the staleness class of bug.
- **Removed dotfile vars linger.** The merge model keeps the
  daemon's stale copy of a var the user deleted from their
  dotfiles. Rare; matches freshly-opened-terminal semantics.
- **Interactive rc files run on launch.** Anything a user's
  `.zshrc` does unconditionally (slow plugin managers, network
  calls) runs per launch and counts against the 5s budget; on
  timeout the launch falls back to stale env.
- **`exec` in rc bypasses the dumper.** A `.zshrc` that
  `exec`s another program replaces the shell before
  `gmux --dump-env` runs; fd 3 never gets written and we fall
  back. Acceptable edge case.

### Breaking

None. Behavior changes only for daemon-spawned sessions, and only
to give them a fresher (superset-on-merge) environment. Headless
daemons (`$SHELL` unset) are byte-for-byte unchanged. No schema,
HTTP, or SSE surface changes.

## Alternatives considered

### A. Status quo: freeze the daemon env at startup

Rejected — it is the bug. The env is captured once and never
re-sourced; the Restart button and `gmuxd restart`-from-inside-a-
session both fail to refresh it.

### B. Cache the captured env (memoize, invalidate on some signal)

Rejected. Caching reintroduces exactly the staleness class we are
fixing, plus an invalidation policy to get wrong. Launch/resume/
restart are infrequent enough that a fresh capture each time is
affordable, and "fresh every time" is the only model with no
hidden drift.

### C. Login-only shell (`-l`, no `-i`)

Rejected as the primary mode. It misses `~/.zshrc` / `~/.bashrc`,
where most users' env actually lives, so it would still look stale
to most users.

### D. From-scratch capture (`env -i $SHELL -l -i`)

Rejected. Truly fresh, but loses session/PAM vars
(`DISPLAY`, `SSH_AUTH_SOCK`, `XDG_RUNTIME_DIR`, …), breaking GUI
apps, ssh-agent, and the runtime socket dir. Merge keeps these
while still applying dotfile changes.

### E. Parse `env` output on stdout with a random marker

Rejected in favor of the fd-3 dumper. Stdout is shared with
rc-file noise, so it needs marker framing; and portable
NUL-delimited dumping is awkward (`env -0` is GNU-only, and bash
exports *functions* as newline-containing env vars, so
newline-delimited parsing is unsafe). The fd-3 dumper sidesteps
both: a private channel, our own NUL format, no marker.

### F. A gmux-specific env file (`~/.config/gmux/env`)

Rejected as the primary mechanism. It is a second place to manage
env and would not reflect ordinary dotfile edits — the exact thing
users expect Restart to honor. Could be added later as an
*additive* layer for power users.

### G. Config toggle / opt-out from day one

Deferred. `$SHELL`-unset already covers headless deployments, and
the behavior is the right default for interactive users. A toggle
(or a `GMUX_NO_REFRESH_ENV` escape hatch) is cheap to add if a real
need surfaces; pre-adding it is speculative surface.

### H. Capture in a neutral cwd ($HOME / daemon cwd)

Rejected. Capturing in the session's launch cwd honors intentional
per-directory hooks (`direnv`/`mise`); a neutral cwd would ignore
them. Users with such hooks accept the cost.
