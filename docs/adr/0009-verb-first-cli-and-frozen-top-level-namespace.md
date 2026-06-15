# ADR 0009: Verb-first CLI, explicit run syntax, and a frozen top-level namespace

**Status:** Proposed
**Date:** 2026-06-15
**Supersedes (in part):** the flag-based action surface of ADR 0005 (the
*transport* decision in 0005 stands; only the user-facing flag grammar changes)

## Context

Before 2.0, `gmux` exposed session actions as mutually-exclusive boolean
flags (`--list`, `--attach`, `--tail`, `--kill`, `--send`, `--wait`,
`--no-attach`, `--host`, `--all`) while the `gmuxd` daemon binary used
verbs (`start`, `run`, `stop`, `status`, `auth`, `remote`, `log-path`).
Two grammars, one product. The flag grammar also required ~150 lines of
hand-rolled mutual-exclusion and interspersed-flag parsing in `cli.go`.

For 2.0 we unify on a single **verb-first** grammar fronted by the
`gmux` binary, and we settle how a command-to-run is expressed.

## Decision

1. **All user-facing actions are verbs under `gmux`.** Session verbs:
   `open`, `ls`, `attach`, `tail`, `send`, `wait`, `kill` (plus `help`,
   `version`). Each verb owns its own flag set; the global
   mutual-exclusion maze is deleted.

2. **`gmux open` is the single canonical UI-launch verb.** No `ui`/`app`
   aliases. (`open` is a verb, consistent grammar, and matches
   `xdg-open`/`gh browse` intuitions.) The web client remains the
   **Frontend** in the glossary; `open` names the *action*, not the thing.

3. **Bare `gmux` prints help.** It has no side effects — in particular it
   does **not** auto-start the daemon.

4. **Running a command is explicit: `gmux -- <cmd> [args…]`.** There is no
   `run` verb (so `run` is not a reserved word) and **no bare shorthand**
   (`gmux pytest` is an "unknown command" error, not "run pytest").
   Power users who want terseness alias `gm='gmux --'` — which is shorter
   than the old shorthand and lives in their shell, not the tool.

5. **The top-level verb namespace is deliberately closed and small.**
   Functionality grows under **namespace groups**, not new top-level
   verbs:
   - `gmux daemon start|stop|restart|status|log-path` — daemon process
     lifecycle (was the `gmuxd` verbs).
   - future groups (e.g. `gmux peer …`) as needed.
   `auth` and `remote` remain top-level (rare, deliberate, setup-time).
   Adding a new top-level verb is a breaking change requiring a major
   bump.

6. **`gmux daemon …` is the canonical front; `gmuxd` keeps its verbs for
   backwards-compatible ops.** `gmux daemon start|stop|restart|status|
   log-path`, `gmux auth`, and `gmux remote` are the documented surface
   and bridge (thin `exec`) to the `gmuxd` binary, which retains its
   existing verbs (`run`, `start`, `stop`, `restart`, `status`, `auth`,
   `remote`, `log-path`; bare `gmuxd` prints help, `gmuxd run` serves
   foreground). The bridge keeps a **single implementation** (in gmuxd)
   rather than copying lifecycle code into gmux.

   This deliberately steps back from "strip `gmuxd` to bare-serve only."
   Rationale: the constraint that drove top-level minimalism — shorthand
   collisions — is gone (decision 4 dropped the shorthand), and daemon
   control is rare, human, and non-scriptable, so a second entry point
   costs little. Against that, a hard break would force migration on
   *infrastructure* config (systemd `ExecStart=…/gmuxd run`, Dockerfiles,
   runbooks) — edited rarely, by different people, and not covered by the
   scriptable-surface migration shim. Keeping `gmuxd`'s verbs is
   backwards-compatible there and lower-risk than moving working code
   across the binary boundary. Docs steer everyone to `gmux daemon …`;
   `gmuxd --help` cross-references it. (This is *not* the `open`/`ui`/`app`
   alias smell of decision 2: that was three names for the single most
   common action; this is one canonical name plus the underlying binary
   that infra already invokes.)

7. **Daemon auto-start is intent-driven.** Session verbs (`open`, `ls`,
   `attach`, `tail`, `send`, `wait`, `kill`, and `gmux -- <cmd>`)
   auto-start `gmuxd` when it is down — the daemon is a *stateful broker*
   that rehydrates dead sessions from disk, so even `ls`/`tail` on a cold
   machine have something to serve. `gmux daemon status` and bare `gmux`
   (help) never auto-start.

8. **`gmux auth` stays a distinct verb because it prints a secret.**
   Folding the token reveal into `gmux remote` status would leak live
   tokens into scrollback/screenshares on a casual status check.
   `gmux remote` shows connection *state* only and MUST NOT print the
   token; revealing the token is the explicit, deliberate act of
   `gmux auth`.

9. **Argument convention (positional vs named).**
   - A verb's single **primary operand is positional and comes first** —
     for session verbs, the session reference: `gmux kill <id>`,
     `gmux tail <id>`, `gmux attach <id>`. This deliberately diverges from
     tmux's named `-t` target: gmux is single-target with no
     `window.pane` substructure, so the disambiguation tmux needs does
     not exist, and `gmux kill foo` beats `gmux kill -t foo`.
   - **Behavior modifiers are named flags:** `-d`, `--timeout`, `--json`,
     `--no-submit`, `--all`.
   - **Variadic / verbatim content is trailing positionals**, after the
     primary operand, guarded by `--` when passed through untouched (the
     run command; literal send text).

10. **Two input verbs with distinct contracts (not aliases).**
    - `gmux send <id> <text> [Key…]` — gmux-native. `<text>` is **literal**;
      trailing bare tokens matching key names (`Enter`, `C-c`, `Escape`,
      `Up`…) are interpreted as keys. Common case: `gmux send foo 'pytest -q' Enter`.
      `--no-submit` suppresses the implicit nothing (no Enter unless a
      trailing key says so).
    - `gmux send-keys -t <id> …` — the **verbatim tmux interface**
      (`-t` target, all args are key names by default, `-l` for literal).
      A *compatibility verb*, documented as such, so tmux knowledge and
      existing tmux skills/agent calls port directly. Docs must state
      crisply: *use `send` normally; `send-keys` only when porting tmux*.
    - **Enter is explicit; `--no-submit` is removed.** Submission is
      controlled by a trailing `Enter` key token, tmux-style:
      `gmux send foo 'pytest -q' Enter` submits; omitting `Enter` types
      without submitting. This changes today's implicit-Enter default.

11. **Local-by-default, as a hard invariant (agent blast-radius).**
    The CLI's default worldview is "this host only", so an agent can
    never *accidentally* act on another machine:
    - **Targeting:** bare references resolve strictly against local
      sessions. Fuzzy/prefix matching never crosses a host boundary. A
      bare ref that matches only a peer session is a **miss with a hint**
      ("did you mean `foo@konyvtar`?"), never a silent remote action.
    - **Visibility:** `gmux ls` shows local only; `--all` opts into the
      fleet. Listing scope and targeting scope are **independent** —
      `ls --all` surfacing a remote session never lets a subsequent bare
      `gmux kill foo` reach it.
    - **Opt-in:** crossing a host requires explicitly typing `@peer`.
      This is a deliberate, per-command act; there is no flag or config
      that makes bare refs go remote.
    - This is the existing resolver behaviour (empty host = `Peer == ""`
      only); 2.0 promotes it to a tested invariant so a future refactor
      cannot quietly let prefix-matching bleed across hosts. The
      agent-facing skill keeps `@peer` as a one-line advanced escape
      hatch, not woven through its examples.
    - No regression of ADR 0005: cross-host CLI actions stay *possible*
      (gmuxd still routes via `peer.Forward`/`ProxyWS`), just *explicit*.
      The trust boundary remains the daemon + per-host pairing tokens
      (ADR 0008); the CLI is one local client among others.

12. **Guiding principle: follow tmux conventions wherever sensible.**
    tmux is the reference grammar for terminal-multiplexer agents; matching
    it (`-d` detach, `send-keys`, key names, capture semantics) lets
    existing tmux skills and agent muscle memory transfer to gmux.
    Deliberate divergences (positional target; literal-default `send`;
    no `run` verb) are called out where they occur and justified by
    gmux's narrower, session-centric model.

13a. **`gmux tail` is snapshot-only.** `gmux tail <id>` (default ~100
     lines), `-n N` for count, `--raw`/`-e` to preserve ANSI (stripped by
     default). **No `-f`/follow and no `logs` verb**: "block until output
     X" is served by `wait --for-text`/`--for-regex` (better: in-daemon,
     timeout, exit code); live human watching is served by the browser
     and `attach`. Streaming a pane elsewhere (tmux `pipe-pane`) is niche
     and deferred to a future namespace if requested.

13b. **`gmux ls` output.** Human default: short id · state
     (alive/idle/dead) · kind · slug/title · command · age (+ `peer`
     column under `--all`). `--json` emits a single stable-schema array
     (`id, slug, kind, alive, idle, pid, title, command, started_at,
     exited_at, exit_code, peer`) so agents stop scraping the table.
     tmux-style `-F`/`--format` is deferred (addable under the
     frozen-namespace policy).

13. **`gmux wait` condition is explicit; default is idle-or-exit.**
    `gmux wait <id>` blocks until the session goes **idle** (agent turn
    end; shell idle-detection is a planned follow-up) **or the session
    exits** — whichever comes first, so a detached non-agent command
    (`gmux -d -- pytest`) is waitable without hanging. Explicit
    conditions: `--for-text <str>` (fixed substring), `--for-regex <pat>`,
    bounded by `--timeout <secs>`. On timeout, exit nonzero **and print
    the current tail to stderr** so the wait is diagnosable. Cross-peer
    `wait` returns "not supported across peers yet" (ADR 0005 deferral).

## Considered alternatives

- **Keep the bare shorthand `gmux <cmd>` alongside `gmux -- <cmd>`.**
  Rejected: the shorthand *freezes* the top-level namespace (every new
  verb could shadow a user's same-named binary), forecloses friendly
  "unknown verb, did you mean…?" errors, and forces a verb-vs-program
  disambiguation branch in the parser. The `gm` alias recovers the
  ergonomics without any of these costs.
- **Require `gmux run <cmd>`.** Rejected in favour of `gmux -- <cmd>`:
  `--` is universal, needs no reserved word, and the parser already
  implements it.
- **One binary for daemon + runner (symlink `gmuxd`→`gmux`).** Rejected:
  keeping `gmuxd` separate matches daemon conventions and keeps `ps`,
  systemd units, and packaging legible.

## Consequences

- ~150 lines of mutual-exclusion / interspersed-flag parsing in `cli.go`
  are removed; each verb parses its own flags normally. Only `gmux --`
  needs stop-at-first-positional.
- Verb *typos* are impossible to fool-proof only because there is no
  shorthand — every bare word is a verb, so `gmux opn` yields a clean
  "unknown command" with a suggestion.
- Breaking change appropriate to a major version. **Migration uses an
  error-only shim, not a forwarding shim.** Old forms (every removed
  flag, `gmuxd <verb>`, and the removed bare `gmux <cmd>` shorthand) are
  recognized *solely to emit a precise migration error* and exit
  nonzero — e.g. `--list` → "use `gmux ls`"; `gmuxd status` → "use
  `gmux daemon status`"; bare `gmux pytest` → "unknown command 'pytest';
  to run a command use `gmux -- pytest`". This is a static guidance
  table carrying **zero old behavior**, so the old flag parser and the
  mutual-exclusion conflict-matrix are genuinely deleted (a forwarding
  shim would have forced keeping them). The table deletes cleanly in
  2.1. Cost accepted: existing *scripts* break immediately rather than
  working-with-warning — appropriate for a major version, and agents
  migrate the instant the skill is updated. Note the shim covers the
  *scriptable* surface only; daemon/ops invocations (`gmuxd <verb>`)
  keep working unchanged per decision 6, so infrastructure config needs
  no migration.
- `--host` is dropped. `<id>@<peer>` is the **sole** way to address a
  peer session, which simplifies `matchSession` (the `host` parameter
  and the `--host`/`@suffix` reconciliation branch both go away).
