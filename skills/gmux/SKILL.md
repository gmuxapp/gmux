---
name: gmux
description: Drive long-running terminal commands and AI coding agents through gmux sessions. Use when the user asks to run a command in the background, send input to a running session, wait for an agent to finish, orchestrate multiple agents in parallel, or capture output from a tmux/screen-style session.
---

# gmux

A command run through gmux becomes a managed session the user can watch live
in a browser. The grammar is verb-first; **running a command always uses the
explicit `--` separator** so gmux never guesses where its own flags end and
the command begins.

## Primitives

```bash
gmux -- <cmd> [args]         # run blocking; command output on stdout, session id on stderr
gmux -d -- <cmd> [args]      # run detached; prints the session id on stdout
gmux agent prompt <id> 'text'   # prompt an AGENT session; waits, prints the exchange report
gmux agent prompt --new 'text'  # launch and prompt; id on stderr, report on stdout
gmux agent logs <id> [-n N]     # the last N exchanges of the conversation (default 1)
gmux agent cancel <id>          # interrupt the work in progress
gmux send <id> 'text' Enter  # RAW: type text and submit (Enter is explicit)
gmux send <id> C-c           # send a control key (interrupt), no text
gmux send --wait <id> 'text' Enter  # raw send AND block until the reply is done
gmux wait <id>               # block until the activity settles; prints the report
gmux wait --quiet <id>       # ...synchronize only, print nothing
gmux wait <id> --for-text S  # block until S appears in the output
gmux tail <id> [-n N]        # RAW: last N lines of terminal output (N=100)
gmux ls [--json]             # list sessions (--json for machine parsing)
gmux kill <id>               # SIGTERM the runner
```

**For agent sessions (pi), prompt with `gmux agent prompt`, not `gmux send`.**
`send` types raw keystrokes and cannot tell you whether the agent accepted
them; `agent prompt` waits until the agent can accept input, submits the way
that agent expects, and reports what actually happened. Keep `send` for
shells, TUIs, and agents without semantic support (Claude Code, Codex — they
fail with `unsupported_adapter`).

`ls` IDs are 8-character prefixes; pass them directly to
`agent` / `send` / `wait` / `tail` / `kill`. Tip: `alias gm='gmux --'` makes
`gm pytest` shorthand for `gmux -- pytest`.

Because `gmux -- <cmd>` streams the command's ANSI-stripped output on stdout,
prints its session id on stderr, and propagates the child's exit code, it
composes: `gmux -- pytest -q | tail` or `if gmux -- pytest -q; then ...`.

## The exchange report

Every semantic agent read — `wait`, a synchronous `agent prompt`, and
`agent logs` — prints the same document:

```
[3 previous exchanges]

[USER]: run the suite and fix what fails

[Agent worked for 12 iterations]

[USER]: also fix the lint warnings

[Agent worked for 4 iterations]

[AGENT]: All 212 tests pass and the linter is clean. …
```

How to read it:

- An **exchange** starts at a user message; an **iteration** is one completed
  model response (tool rounds included), so the count is how much work
  happened. Only the **terminal** response prints — tool calls and
  intermediate prose are behind the iteration counts (use `gmux tail` for
  forensics).
- Live wait/prompt reports are **bounded**. A cut is always marked: an opening
  `[N exchange(s) and M bytes omitted from live report]` line, or an
  `[AGENT, truncated]:` label on capped prose. The outcome is never lost.
  **If you see either marker, re-read with `gmux agent logs -n N`** — logs
  reads the full native history and is not subject to the live budget.
- The last line is the state: nothing extra after `[AGENT]: …` means
  completed; `[Agent active, N iterations so far...]` means still working;
  `[Agent completed without a final response]`, `[Agent interrupted]`,
  `[Agent failed: <reason>]`, `[Wait timed out after Ns; …]` mean what they
  say. Partial final prose is labeled `[AGENT, partial]:`. An empty
  conversation is `[No exchanges yet]`.
- Wait/prompt reports abbreviate your own prompt (exact-text match, cut with
  `…`); `logs` never abbreviates.

**The exit code is the verdict; stdout is the report — for every outcome.**
`0` completed, `2` intentionally interrupted, `1` failed or timed out, `128+N`
a first local signal killed *the wait* — the agent keeps running, and stdout
is exactly `[Wait interrupted; agent remains active]` (no exchanges: the CLI
has no facts to report yet; use `gmux agent logs` to look). So
`report=$(gmux wait $id)` captures the account
even when `$?` is nonzero; branch on the exit code, never on empty/non-empty
stdout. stderr appears only when gmux could not report at all (unknown
session, unsupported adapter, transport failure — exit 1, empty stdout).
There is no `--json` on wait/prompt/logs; `gmux ls --json` is the
machine-readable list.

## Prompting an agent session

```bash
id=$(gmux -d -- pi)
gmux agent prompt --timeout 600 $id 'run the suite and fix what fails'
gmux agent logs $id                        # re-read the latest exchange any time
git diff | gmux agent prompt $id           # prompt from stdin (stays one prompt)
```

An agent session names a **conversation**, not a process. Prompting an
inactive conversation transparently resumes it; reads never start anything.
Semantic state is **active** (working) or **inactive** (settled) — never
branch on whether a process is alive.

### Launch and prompt in one command: `--new`

```bash
id=$(gmux agent prompt --new --no-wait --name review 'review the diff on this branch')
gmux wait $id

gmux agent prompt --new --model anthropic/sonnet 'summarize this repo'
```

`--new` launches a new **pi** session (same machinery as `gmux -d -- pi`, from
your env and cwd) and sends the prompt as its first work. Pass either an id or
`--new`, never both.

**The bare session id is printed the moment the session exists**, before the
prompt is delivered — so you can always address a session you just created,
even if admission or the work then fails. It means only "this session exists
and is addressable"; the exit code carries everything else. A synchronous run
prints the id on **stderr** and the exchange report alone on stdout; `--no-wait`
prints the bare id on stdout only and exits 0 once the work was **admitted**
(the agent started it) — on a sick session that line can block up to the 60 s
admission window. A launch that never registers prints no id and exits 1. A
failure after the launch leaves the session behind and **you own
it** (it may still be running): retry against the id, read it, or
`gmux kill $id`.

`--new` must come **before** the prompt: after an id it is prompt text like
anything else (`gmux agent prompt $id --new` sends the literal `--new`).
`--model`/`--name` are valid only with `--new`; `--follow-up`/`--steer` are
refused with it (there is no work yet); `--timeout` bounds your wait, not the
launch. `--new` is pi-only — for any other command the two-step
`id=$(gmux -d -- <cmd>)` then `gmux agent prompt $id …` is still fully valid.

### Steering, following up, cancelling

All flags go **before** the id (text after the id is verbatim):

```bash
gmux agent prompt --no-wait $id 'start the long refactor'   # return once admitted, no report
gmux agent prompt --follow-up $id 'then update the docs'    # queue after the current response
gmux agent prompt --steer $id 'stop, use the sqlite path'   # redirect the running work
gmux agent prompt --no-wait --steer $id 'and skip the migration'  # --no-wait composes
gmux agent cancel $id; gmux wait $id; [ $? = 2 ] && echo stopped   # interrupt, then wait
```

Do **not** chain cancel with `&& gmux wait`: interrupted work exits `2`, so the
wait "fails" on exactly the outcome you asked for (and aborts the script under
`set -e`).

**Waits are observational — nothing interrupts them but their own terminal
conditions.** A `--steer`, a `--follow-up` merged into the running loop, or a
human typing into the TUI never ends anybody's wait early: it appears in every
observer's report as a new `[USER]:` boundary, the activity settles once, and
everyone gets the same merged report. No re-arm loop, no ownership rules.

`--follow-up` has two modes: to an **inactive** agent it starts ordinary work
(like a plain prompt); into **running** work it merges into that activity —
one settle, one report, your follow-up as one of its `[USER]:` lines.
`--steer` requires work in progress; steering nothing is an error.

`--follow-up` and `--steer` are mutually exclusive; `--no-wait` composes with
either (it only decides whether you block, so `--no-wait --timeout N` is a
usage error). A plain prompt (or `--follow-up` to an inactive agent) starts
work, so `--no-wait` returns once the agent actually began it; `--steer` and a
merging `--follow-up` join work already under way, so `--no-wait` returns at
delivery. `cancel` returns when the interrupt is *delivered* — follow with
`gmux wait` if the next step needs the work actually stopped.

For pi, `cancel` also **restores queued follow-ups into the composer**: after a
cancel the composer may hold text nobody retyped, and the next prompt submits it
together with the new one. And `--follow-up`/`cancel` depend on pi's default
alt+enter/escape keybindings — a session whose user remapped them loses both
silently.

Errors carry a stable code. `admission_timeout`, `delivery_timeout` and
`transport_error` are **indeterminate** — the prompt may already have landed,
so do not blindly retry; inspect with `gmux agent logs` first. So is a bare
transport failure with no code (a dropped connection to gmuxd): the request
may have been delivered before the connection went away. `runner_outdated`
(the session predates semantic actions: restart it), `precondition_failed`,
`delivery_pending`, `not_ready`, `not_running` and `incarnation_mismatch` (the
runner was replaced mid-flight and the replacement refused an action meant for
its predecessor) all guarantee nothing was delivered and are safe to retry.
Local sessions only (peer ids are refused).

## Sequential orchestration

```bash
id=$(gmux -d -- pi "implement the feature")
gmux wait $id

gmux agent prompt --timeout 900 $id "$(cat review.txt)"
```

For **raw** (non-agent) sessions, **prefer `gmux send --wait` over
`gmux send … && gmux wait`** for "send a prompt and wait for the reply": the
two-command composition can observe the *previous* turn's idle state and
return before the agent has even started, while `--wait` is race-free (the
daemon arms the wait before delivering the input and requires a fresh
active→idle transition). `--wait` requires the input to submit (a trailing
`Enter` or a `\r` in piped stdin) and accepts `--timeout N`; exit codes match
`gmux wait` below, but `send --wait` prints no report.

## Parallel orchestration

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  ids+=( "$(gmux agent prompt --new --no-wait --name "$ticket" "Implement $ticket. Return when done.")" )
done

for id in "${ids[@]}"; do
  gmux wait --quiet "$id" --timeout 600 || echo "$id failed: $?"
done

for id in "${ids[@]}"; do
  echo "=== $id ==="
  gmux agent logs "$id"     # each agent's latest exchange: prompt, work, response
done
```

## Waiting

`gmux wait <id>` blocks until the session settles — an agent finishing its
work (the agent itself asserts the boundary), a shell finishing its command
and returning to a fresh prompt, or a one-shot command's process exiting —
optionally bounded by `--timeout N`. Exit codes (the global taxonomy):

- `0` completed normally (a one-shot that exited **0**, a shell back at its
  prompt, an output condition that matched)
- `2` intentionally interrupted
- `1` everything else: failed work, `--timeout` elapsed, the session exited
  before its output matched
- `128+N` a first `^C`/SIGTERM stopped **the wait** — stdout is exactly
  `[Wait interrupted; agent remains active]`, the agent keeps running; a
  second signal kills immediately

For a pi session, `wait` prints the exchange report of the activity it
observed — on every outcome; `--quiet` suppresses it. A `wait` armed while no
activity is in progress returns immediately with the **latest exchange** and
that activity's settled verdict (`[No exchanges yet]`, exit 0, for a fresh
conversation). To gate on work you are about to trigger, use
`gmux agent prompt` or `gmux send --wait`, which arm the wait before
delivering. Shell sessions, Claude/Codex sessions and output-condition waits
are synchronization-only (no report).

**A failed command fails its wait — for lifetime turns only**: a session whose
turn is its whole lifetime (`gmux -d -- pnpm test`, a shell *without* OSC 133
integration) closes that turn with an error on a non-zero child exit, so
`gmux wait` exits 1, and `gmux wait $id && next-step` cannot run after a failed
build. If you only need "it finished", read `exit_code` from `gmux ls --json` or
run it in the foreground (`gmux -- pnpm test`), which propagates the child's code.

**Per-command turns carry no result.** In a shell whose integration emits OSC 133
prompt marks (fish, out of the box), every command is its own turn and gmux reads
the marks purely as busy/idle — the exit code in the `D` mark is not consumed. So
`gmux wait` exits 0 when the prompt returns, pass or fail: `gmux wait $shell_id &&
deploy.sh` **will** deploy after a failed build. Run the command as its own
session (`gmux -- make build`) if you need its result.

Every session is waitable. **Shell sessions** get per-command idle from OSC
133 prompt marks (fish emits them by default; bash/zsh need shell
integration): `gmux send --wait <id> 'make build' Enter` blocks until the
command finishes and the prompt returns. Sessions that never emit marks —
one-shot `gmux -d -- <cmd>` runs, or shells without integration — are one
lifetime-long turn: `wait` blocks until the process exits. Careful: an
interactive shell without integration never exits on its own, so bound that
wait with `--timeout` or use an output condition below.

To wait for specific **output** instead of settling, use `--for-text <substr>`
or `--for-regex <pattern>` (works for shell sessions too — no grep loop needed):

```bash
gmux wait $id --for-text 'listening on' --timeout 60
gmux wait $id --for-regex 'tests? passed: \d+' --timeout 120
```

Same exit codes (`0` matched, `1` the session exited first or the timeout
elapsed), and no report is printed. Matching is line-wise against the rendered
terminal output (ANSI stripped, same text `gmux tail` shows), including output
that appeared before the wait started, so the pattern must fit on one terminal
line.

## Reading a session

| You want | Command | `-n` counts |
| --- | --- | --- |
| The raw screen | `gmux tail <id>` (any session) | lines |
| The conversation | `gmux agent logs <id>` (agents) | exchanges |

`gmux tail` is always raw: the last N lines of rendered terminal output, plain
text, for shells, one-shot commands and agents alike. It is where tool output
and TUI detail live.

`gmux agent logs` renders the stored conversation as exchanges (default: the
latest one). It is a **store-only read**: it never starts or resumes anything,
so it is always safe — including on a settled conversation with no resident
process, and while the agent is working (`[Agent active, N iterations
so far...]` at the end tells you it is). `-n` must be positive; earlier
history is summarized as `[N previous exchanges]`.

```bash
gmux agent prompt $id 'summarize your findings'   # report includes the reply
gmux agent logs $id                                # the same exchange, re-read
gmux agent logs -n 3 $id                           # more history
```

Shell sessions, and agents with no readable conversation
(`unsupported_adapter`), are read with `gmux tail` instead.

## Other agents have one-shot modes

Agents stay running by default. To make them exit after one prompt, use the
agent's print mode: `pi -p`, `claude -p`, `codex exec`:

```bash
gmux -- pi -p "summarize this PR"
```

## Sessions on other machines

Sessions are **local by default** — bare IDs only ever match this host, so you
can't accidentally act on another machine. To address a peer session
explicitly, suffix the ID with `@<peer>` (see them with `gmux ls --all`):

```bash
gmux tail abc123@laptop
```

## Reference

- <https://gmux.app/reference/cli/>
- <https://gmux.app/integrations/scripts-and-agents/>
