---
title: Orchestrating agents
description: Launch AI coding agents as gmux sessions, drive them with prompts, and harvest their results — one at a time or in parallel.
---

gmux turns AI coding agents into managed sessions: an agent you launch from a
script or from *another agent* runs in its own terminal, the user can watch it
live in the browser, and your code drives it semantically — prompts in,
exchange reports out.

:::note
Semantic agent orchestration works with **pi** today. Other agents (Claude
Code, Codex) run fine as gmux sessions, but answer `unsupported_adapter` on
`gmux agent` verbs — drive those with raw [`send`/`tail`](/reference/cli/#gmux-send---wait---timeout-n-id-text-key)
or their one-shot modes (`claude -p`, `codex exec`).
:::

:::tip[Driving gmux from an agent?]
Install the [gmux skill](https://github.com/gmuxapp/gmux/blob/main/skills/gmux/SKILL.md)
for running commands and orchestrating agents through observable sessions, so your agent
picks up these patterns automatically:

```sh
npx skills add gmuxapp/gmux
```

The skill follows the [agentskills.io](https://agentskills.io/) standard and
works with Claude Code, Codex, Cursor, Copilot, Gemini CLI, OpenCode, and 50+
other agents. Or drop the `SKILL.md` file into your agent's skills directory
by hand.
:::

## The core loop

```bash
gmux agent prompt --new 'Fix the auth bug in ...'    # launch + prompt + wait, one command
gmux agent prompt a1b2c3d4 'now add tests'           # follow up on the same conversation
gmux agent logs a1b2c3d4                             # re-read the latest exchange any time
```

An agent session names a **conversation**, not a process: prompting an
inactive conversation transparently resumes it, and reading one never starts
anything. Semantic surfaces report the agent as **active** or **inactive** —
your script never branches on whether a process happens to be resident.

`--new` launches a new pi session (from this shell's env and cwd, local daemon
only) and sends the prompt as its first work. A synchronous run prints the
bare id on stderr as soon as the session exists, then the exchange report alone
on stdout when the work settles; `--no-wait` prints the bare id on stdout and
returns once the work was admitted — the command-substitution shape:

```bash
id=$(gmux agent prompt --new --no-wait --name review 'review the diff on this branch')
gmux wait "$id"
```

The session id prints on its channel the moment the session exists, *before*
the prompt is delivered, so you can always address the session you just paid
for — even when admission or the work then fails. A failure after the launch leaves the
session behind and **you own it**: retry against the id, read it with
`gmux agent logs`, or `gmux kill` it. `--model` and `--name` are valid only
with `--new`, and `--new` must come before the prompt — after a session id it
is prompt text like anything else. The two-step `gmux -d -- pi` plus a prompt
remains fully supported.

## Safe multiline prompts

For a nontrivial prompt, use stdin with a **quoted heredoc** instead of a
double-quoted shell argument. Quoting the delimiter keeps backticks,
`$variables`, `$(commands)`, quotes, and other shell syntax literal:

```bash
gmux agent prompt --new --no-wait --name review <<'PROMPT'
Review the `auth` package and run its tests.

Check $(git status) before editing. Work alone; do not spawn agents.
PROMPT
```

The prompt receives `$(git status)` verbatim; the launching shell does not run
it. The same form works when prompting, steering, or following up with an
existing session. Short static prompts may remain single-quoted arguments,
and files can be redirected directly: `gmux agent prompt "$id" < prompt.md`.

## The exchange report

`agent prompt` and `gmux wait` block until the activity settles, then print
the **exchange report** on stdout — what was asked, every further user message
that entered the loop, `[Agent worked for N iterations]` markers, and the
terminal response:

```
[USER]: run the suite and fix what fails…

[Agent worked for 12 iterations]

[AGENT]: All 212 tests pass. Two fixtures needed …
```

Your own prompt is abbreviated (you already know it); everything else prints
in full within the live transport budget — an oversized activity's report
marks its cuts (`[AGENT, truncated]:` on capped prose,
`[N exchange(s) and M bytes omitted from live report]` when early user
boundaries were dropped) and never loses the outcome. When a report carries
either marker, read the full text with `gmux agent logs -n N`, which reads the
complete native history.

**The exit code is the verdict; stdout is the report.** The report prints for
every outcome — `0` the activity completed, `2` it was intentionally
interrupted (report ends `[Agent interrupted]`), `1` a failed activity or an
elapsed `--timeout` (`[Agent failed: <reason>]`; an authoritative timeout
report ends `[Wait timed out after Ns; agent active, N iterations so far...]`,
while the hard local deadline falls back to
`[Wait timed out after Ns; session state unknown]`), `128+N`
for a first local signal, which prints only
`[Wait interrupted; agent remains active]` and leaves the agent running. So
`report=$(gmux agent prompt …)` captures the account even when `$?` is
nonzero. stderr is reserved for gmux's inability to produce a report at all
(unknown session, unsupported adapter, transport failure) — plus, for a
synchronous `--new`, the single line carrying the fresh session's id. A single-session
command then prints nothing on stdout and exits 1. After a multi-wait has
armed, one such failure can coexist with ordered stdout blocks from the other
sessions (and a header for the failed session); stderr identifies which report
could not be produced. There is no `--json` on prompt/wait/logs today; script
against the exit code and the report.

## Steering, following up, cancelling

```bash
git diff | gmux agent prompt "$id"                  # prompt from stdin, one prompt
gmux agent prompt --no-wait "$id" 'start the long refactor'
gmux agent prompt --follow-up "$id" 'then update the docs'   # queue behind the current work
gmux agent prompt --steer "$id" 'stop, use the sqlite path'  # redirect the running work
gmux agent cancel "$id"; gmux wait "$id"; [ $? = 2 ] && echo stopped   # interrupt, then wait
```

Note the `;` rather than `&&`: an interrupted activity exits `2`, so chaining
the wait with `&&` "fails" on the outcome you asked for.

**Steering never interrupts a wait.** A user message injected into an activity
somebody is waiting on — a `--steer`, a `--follow-up` that merged into the
running loop, or a human typing into the TUI — simply appears in their report
as a new `[USER]:` boundary. Every wait on the activity runs to the agent's
own settle and returns the same merged report: the report *is* the story of
everything that entered the loop.

`--follow-up` has two modes: delivered to an **inactive** agent it starts
ordinary work; delivered into **running** work it merges into that activity,
which settles once for everybody. Flags go before the id; everything after the
id is the prompt, verbatim. A plain prompt transparently resumes an inactive
conversation to deliver it; `--steer` and `cancel` fail when no activity is in
progress and never resume.

## Reading a conversation

```bash
gmux agent logs "$id"          # the latest exchange: prompt, work, response
gmux agent logs "$id" -n 5     # …the last five exchanges
gmux tail "$id" -n 50          # the raw terminal, when you need the tool output
```

`agent logs` renders the conversation as **visible exchanges** — the same
document a wait report uses — read from the agent's own conversation file, not
the TUI's box-drawing and spinners. `-n N` counts exchanges (default 1); only
the terminal response prints in full, earlier history is one
`[N previous exchanges]` line. While the agent is active the latest exchange
ends `[Agent active, N iterations so far...]`, so `logs` is also the way to
check on running work without waiting on it. `logs` never abbreviates and
never starts or resumes anything — it is always safe to look.

Pair the reads with a wait to harvest a specific artifact:

```bash
gmux agent prompt --timeout 600 "$id" < ship-prompt.txt
url=$(gmux agent logs "$id" | grep -oE 'https://github\.com/[^ ]+/pull/[0-9]+' | tail -1)
```

## Parallel agents

Spawn N agents, then wait on all of them with one command:

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  if id=$(gmux agent prompt --new --no-wait --name "$ticket" "Implement $ticket. Return when you're done."); then
    ids+=("$id")
  else
    status=$?
    if [[ -n "$id" ]]; then
      printf 'launch failed after creating %s; inspect or kill it\n' "$id" >&2
    fi
    exit "$status"
  fi
done

gmux wait "${ids[@]}" --timeout 600
```

Every launch is checked before its id is appended, so a bad launch stops the
fan-out before `wait`. Failure after session creation or during prompt
admission can still leave an id and session behind; the failure branch keeps
that id available to inspect with `gmux agent logs "$id"` or clean up with
`gmux kill "$id"`. The multi-id `wait` then blocks until every admitted agent
settles and prints each report in argument order under an
`=== <full-8-character-id> ===` header, exiting with the worst verdict across
the set. Individual sessions can still be re-read or re-waited afterwards — a
wait never affects the agents it observes.

## One workspace per agent

Give each concurrent agent its own worktree, following your project's
conventions (git worktrees, jj workspaces, or any tool that wraps them), and
launch the agent from inside it. Two agents editing the same working copy
corrupt each other's work in ways neither can see. Isolation pays off even for
review agents: in their own worktree they can build, run tests, and probe the
change instead of only reading it.

## Error codes

Prompt and cancel failures name a stable code on stderr. Treat
`admission_timeout`, `delivery_timeout` and `transport_error` as
**indeterminate** — the prompt may already have landed, so a blind retry can
duplicate it; inspect with `gmux agent logs` first. A bare transport failure
with no code (a dropped connection to gmuxd) is indeterminate for the same
reason. `runner_outdated`, `precondition_failed`, `delivery_pending`,
`not_ready`, `not_running`, `incarnation_mismatch` and `runner_unreachable`
all guarantee nothing was delivered and are safe to retry.
`unsupported_adapter` means the session's agent has no semantic support: use
raw `send`/`tail`. `unsupported_action` means the adapter cannot express this
particular action (for example, cancel on an adapter without an interrupt key)
and is equally safe to retry with a different approach.

For pi, `agent cancel` also restores queued follow-ups into the composer, so
after a cancel the composer may hold text nobody retyped — the next prompt
submits it together with the new one. `--follow-up` and `cancel` depend on
pi's default alt+enter/escape keybindings; a session whose user remapped them
loses both silently.

## See also

- [CLI reference](/reference/cli/) — the full grammar of `gmux agent` and `gmux wait`, plus raw-session scripting: the piped `gmux -- <cmd>` flow, [`send`](/reference/cli/#gmux-send---wait---timeout-n-id-text-key), shell [waits](/reference/cli/#gmux-wait-id), and the [`ls --json`](/reference/cli/#ls---json-schema) contract
- [pi integration](/integrations/pi/) — how pi reports status and resumes conversations
