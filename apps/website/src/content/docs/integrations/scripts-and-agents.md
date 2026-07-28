---
title: Scripts and agents
description: Drive gmux sessions from shell scripts, CI, and agent harnesses.
sidebar:
  order: 0
---

`gmux` is designed so the same binary works whether you attach to a session by hand or drive sessions from a script. This page covers the scripted shape: starting sessions non-interactively, sending input, waiting for output, and composing these into agent-orchestration patterns.

:::tip[Driving gmux from an agent?]
Install the [gmux skill](https://github.com/gmuxapp/gmux/blob/main/skills/gmux/SKILL.md) so your agent picks up these patterns automatically:

```sh
npx skills add gmuxapp/gmux
```

The skill follows the [agentskills.io](https://agentskills.io/) standard and works with Claude Code, Codex, Cursor, Copilot, Gemini CLI, OpenCode, and 50+ other agents. Or drop the `SKILL.md` into your agent's skills directory by hand if you prefer not to install the CLI.
:::

## The piped flow

The most useful primitive for scripting is `gmux -- <cmd>` with stdin redirected away from a terminal:

```bash
gmux -- make build < /dev/null
gmux -- pi -p "summarize this PR" < /dev/null
```

Running a command always uses the explicit `--` separator — there is no bare `gmux <cmd>` shorthand. The `-p` (print) flag tells `pi` to process the prompt and exit instead of staying interactive; other agents have similar one-shot modes (`claude -p`, `codex exec`). Without one, the agent stays running and the call blocks indefinitely — for multi-turn work, spawn detached and drive it instead (see [parallel orchestration](#parallel-orchestration)).

When stdin is not a TTY, `gmux -- <cmd>`:

- **Blocks** until the child exits.
- **Streams bounded metadata** to stdout (session id, adapter, command, pid, exit status), not the full PTY output. Your script's logs stay readable.
- **Exits with the child's exit code**, so `gmux -- make build < /dev/null && deploy.sh` works.
- **Keeps the session in the UI** for the duration: a human can watch it live in the browser without affecting the script.

This is the shape every other line on this page builds on. It works the same in CI, cron jobs, and agent harnesses (whose stdin is a pipe by default).

One exception: recognized one-shot adapter subcommands (e.g. `pi update`, `pi --version`) are exec'd directly and never create a session.

## Spawning detached

To start a session and drive it later, spawn it detached with `-d`. It returns immediately and prints the session id:

```bash
id=$(gmux -d -- pi "build the feature")
```

Capture that id and pass it to `send`, `wait`, `tail`, and `kill`.

## Prompting an agent

For **agent sessions on this host** (pi today), `gmux agent` is the verb to script against: it waits until the agent can accept input, submits the way that agent expects, and reports what the daemon observed instead of just "the bytes went out".

```bash
id=$(gmux -d -- pi)
answer=$(gmux agent prompt --timeout 600 "$id" 'run the suite and fix what fails')
gmux agent status "$id"                 # what matters now: state, trigger, answer
```

### Launching and prompting in one command

`--new` does the two steps above at once: it launches a new pi session (exactly
as `gmux -d -- pi` would, from this shell's env and cwd, local daemon only) and
sends the prompt as its first turn.

```bash
id=$(gmux agent prompt --new --no-wait --name review 'review the diff on this branch')
gmux wait "$id" && gmux agent status "$id"

gmux agent prompt --new --model anthropic/sonnet 'summarize this repo'   # id, then the answer
```

**The session id is stdout line 1**, written the moment the session exists —
before the prompt is even delivered — so you can always address the session you
just paid for, including when admission or the turn then fails. It means only
that: the session exists and is addressable. It is not an admission receipt and
not a readiness signal — the exit code carries those. Under `--new`
the completion signal is therefore the **exit code**, not non-empty stdout: a
successful synchronous run prints the id and then the answer. With `--no-wait`
the bare id is the only output and exit `0` means the turn was **admitted** — the
agent actually started it. The id still prints immediately, but the process
returns only once that happened (or the 60 s admission window expires), so on a
sick session the launch line can block that long instead of returning at
delivery. If the launch itself fails, stdout stays empty — no session exists to
address.

The first turn of a freshly launched session prints its answer like any other:
the agent asserts its turn's result at the boundary, so there is no window in
which the reply exists but gmux cannot attribute it.

A failure after the launch leaves the session behind and **you own it**: gmux
does not tear it down, and it may still be running. Retry against the printed
id, read it, or `gmux kill "$id"`.

`--new` must come before the prompt — after a session id it is prompt text like
anything else.

There is no launch-shaped failure mode to handle separately: the prompt travels
the same readiness-gated path as every later one, so an agent that never comes
up fails its first prompt with the same `admission_timeout` as its tenth.

`--model` and `--name` are valid only with `--new`; `--follow-up` and `--steer`
are refused with it (a session that does not exist yet has no turn to queue
behind or steer); `--timeout` bounds your wait, never the launch. `--new`
launches pi only — for any other command, the two-step `gmux -d -- <cmd>` plus
a prompt remains fully supported and is the shape to use.

`agent prompt` blocks until the turn ends and prints the agent's answer on stdout — the turn's final message only (no `[tool]` lines), as the agent asserted it for that turn (gmux does not reconstruct it from the transcript, so an answer is only ever attributed to the turn that produced it). Exit codes are gmux's global taxonomy, shared by every verb that reports a gmux verdict (`gmux -- <cmd>`/`gmux edit` pass the child's code through, and `gmux daemon|auth|remote` pass gmuxd's): `0` the turn completed, `2` it was intentionally interrupted or the wait was resolved early because somebody steered the turn, `1` anything else (a failed turn, a `--timeout`, a dead runner, an injection the agent never acknowledged). A turn that did not complete prints **nothing on stdout** and a short status-shaped report on stderr instead — outcome, reason, the trigger excerpt, and the injected text when somebody steered — because a previous turn's answer must never be handed back as this one's, and a bare non-zero exit tells you nothing. `--json` swaps both for one stdout envelope (`{outcome, reason, output, trigger, steered_by, truncated, message}`, absent rather than empty) and keeps stderr silent. Every terminal path prints exactly one envelope, success included, so silence is never an outcome a script has to interpret; it is refused with `--quiet` (on `wait`) and with `--no-wait`. `gmux agent status <id>` shows whatever exists in that case (and `gmux agent logs --agent -n 1 <id>` the answer alone), and works even after the session has died — as a snapshot read it can be staler than the answer a wait carries.

The other shapes:

```bash
git diff | gmux agent prompt "$id"                  # prompt from stdin, one prompt
gmux agent prompt --no-wait "$id" 'start the long refactor'
gmux agent prompt --follow-up "$id" 'then update the docs'   # queue behind the current turn
gmux agent prompt --steer "$id" 'stop, use the sqlite path'  # redirect the running turn
gmux agent prompt --no-wait --steer "$id" 'and skip the migration'   # --no-wait composes with either mode (returns at delivery here: the turn is already running)
gmux agent cancel "$id"; gmux wait "$id"; [ $? = 2 ] && echo stopped   # interrupt, then wait
```

Note the `;` rather than `&&`: an interrupted turn exits `2`, so chaining the wait with `&&` "fails" on the outcome you asked for.

**Steering interrupts waits.** A user message injected into a turn somebody is waiting on — a `--steer`, a `--follow-up` that merged into the running loop, or a human typing into the TUI — ends *their* wait early with exit `2` and reason `steered`: the answer they were promised now answers something else. The turn keeps running, so re-arm:

```bash
gmux wait "$id" || gmux wait "$id"    # first wait steered; the second gets the redirected answer
```

The injecting command keeps its own result: gmux matches the agent's report of a message entering the loop against the text it delivered, and hands that caller the merged close — but only while its message is the loop's **last** injection (a later one supersedes it, reason `steered_again`), and only if the agent acknowledged it before the turn settled (otherwise the result is `indeterminate`, exit `1`, never an answer that may predate the injection). The match is textual, since the agent's report carries the message and nothing else, so gmux requires an exact match — or a prefix, but only for an excerpt the agent explicitly reported as truncated — and credits an ambiguous report — two in-flight deliveries that could both explain it — to nobody, leaving both callers `indeterminate`.

`--follow-up` therefore has two modes: delivered to an **idle** agent it starts an ordinary turn; delivered into a **running** turn it merges into that turn, so the merged close's answer is the follow-up's — and other waiters on that turn are interrupted exactly as by a steer.

Flags go before the id; everything after the id is the prompt, verbatim. A plain prompt restarts a dead retained session to deliver it; `--steer` and `cancel` require a live, active turn and never resume.

Errors name a stable code. Treat `admission_timeout`, `delivery_timeout` and `transport_error` as **indeterminate** — the prompt may already have landed, so a blind retry can duplicate it — and treat a bare transport failure with no code (a dropped connection to gmuxd, a daemon restarted mid-prompt) the same way: the request may have been delivered before the connection went away. `runner_outdated`, `precondition_failed`, `delivery_pending`, `not_ready`, `not_running` and `incarnation_mismatch` (the runner was replaced mid-flight, and the replacement refused an action meant for its predecessor) all guarantee that nothing was delivered, so they are safe to retry. `unsupported_adapter` means the session's agent has no semantic support yet: use raw `send`/`tail` for those, as below.

For pi, `agent cancel` also restores queued follow-ups into the composer, so after a cancel the composer may hold text nobody retyped — the next prompt submits it together with the new one. `--follow-up` and `cancel` also depend on pi's default alt+enter/escape keybindings; a session whose user remapped them loses both silently.

## Sending input

`gmux send <id> [text] [keys]` pushes input into a running session, as if typed at the keyboard. Text is sent literally; trailing key names (`Enter`, `C-c`, …) are sent as keys. **Submission is explicit** — add a trailing `Enter` to dispatch a line:

```bash
gmux send <id> 'shorter inline message' Enter
gmux send <id> Enter < prompt.txt          # pipe a file, then submit
gmux send <id> C-c                          # interrupt, no Enter
```

When no text argument is given and stdin is a pipe, gmux reads stdin until EOF (capped at 1 MiB). `send` is gated by the daemon's owner-only Unix socket; see the [CLI reference](/reference/cli/) for the access-control story. For drop-in tmux compatibility, `gmux send-keys -t <id> [-l] <keys...>` is also supported.

## Waiting

`gmux wait <id>` blocks until a session's current turn ends — the primitive that turns sequential orchestration into one line per step. For agent sessions, `gmux agent prompt` bundles the send and the wait; for raw sessions, `send --wait` does:

```bash
gmux agent prompt <id> < step-1.txt      # agent session: prompt, wait, print the answer
gmux send --wait <id> Enter < step-2.txt # raw session: type, submit, wait

gmux agent logs <id> -n 2      # the prompt and the reply, clean markdown
```

The idle signal is the same `Status.Active` flag the UI's spinner consumes, so `wait` returns the moment the agent emits its closing message. On an already-idle session it returns immediately and reports the **last** turn's conclusion and result — to gate on a turn you are about to trigger, use `gmux agent prompt` or `send --wait` (below), which arm the wait before delivering anything. Exit codes: `0` the turn completed (or the output condition matched), `2` the turn was intentionally interrupted, `1` anything else — a failed turn, a death, or `--timeout N` elapsing.

`wait` is also **result-bearing**: for a result-bearing agent (pi today — one that asserts its own turn boundary and result), a turn that completed normally prints that turn's final message, exactly like `gmux agent prompt`. A wait that arrives after the turn already closed reports the conclusion but no answer: it never observed the turn, so it does not claim one — `gmux agent status` is the snapshot read for that.

```bash
answer=$(gmux wait "$id")       # the reply, or empty for a shell/other agent
gmux wait --quiet "$id"         # synchronize only
```

A failed, interrupted or dead turn prints nothing and reports the condition on stderr; shell sessions, non-pi agents and output-condition waits are synchronization-only, as before.

**A failed command fails its wait — for lifetime turns only.** For sessions whose turn is their whole lifetime (`gmux -d -- make build`, shells *without* OSC 133 integration) a non-zero child exit closes the turn with an error, so `gmux wait` exits `1`. That is deliberate — `gmux wait "$id" && deploy.sh` must not deploy a failed build — but it means "did it finish?" and "did it succeed?" are the same question here. If you only need the former, run it in the foreground (`gmux -- make build`, which propagates the child's code) or read `exit_code` from `gmux ls --json`.

**Per-command turns carry no result.** In a shell whose integration emits OSC 133 prompt marks (fish does by default) each command is its own turn, and gmux consumes the marks only as busy/idle transitions — the exit code in the `D` mark is deliberately dropped. `gmux wait` therefore exits `0` when the prompt returns, pass or fail, so `gmux wait "$shell_id" && deploy.sh` **does** deploy after a failed build. Run the command as its own session if you need its verdict.

**Prefer `send --wait` over `send` then `wait`.** The two-command form is subtly racy: `wait`'s opening snapshot can catch the *previous* turn's idle state before the send-induced `Active=true` has propagated, returning immediately. `gmux send --wait <id> … Enter` subscribes before delivering the input, so it always gates on a fresh turn. Bound it with `--timeout N`; exit codes match `wait` — `send --wait` reports the same turn conclusion (including `2` for an interrupted turn) but prints no agent result. A standalone `gmux wait <id>` is still the right tool when you're waiting on a turn you didn't just trigger.

**Waiting on output.** `gmux wait <id> --for-text 'DONE'` (or `--for-regex 'error: \d+'`) blocks until text appears in the session's output instead of on the idle signal. The match runs server-side against gmuxd's scrollback (per rendered line), and — unlike the idle wait — works for **shell** sessions too:

```bash
gmux wait <id> --for-text 'Listening on' --timeout 30   # wait for a server to come up
```

Every session is waitable: agents get their idle signal from turn hooks, shells with OSC 133 prompt marks (fish out of the box) get per-command idle, and everything else — one-shot commands, shells without integration — is one lifetime-long turn that closes when the process exits. Careful with the last kind: an interactive shell without integration never exits on its own, so bound that wait with `--timeout` or an output condition. `wait` only works for sessions on the local host — peer sessions (`<id>@<peer>`) are rejected. To wait for a shell *command* to finish, running it as its own session through the blocking piped flow above (`gmux -- make build < /dev/null`) is still the simplest shape.

## Reading output

Three commands split by **what you know you want**, so a script never has to know what is running in a session to know what shape its output will be:

```bash
gmux tail <id> -n 50                # the raw screen: terminal text, any session
gmux agent logs <id> --agent -n 1   # the exact text you want (here: the answer alone)
gmux agent status <id>              # "show me what matters": state, trigger, content
```

`gmux tail <id>` is always the raw view: the last `-n N` **lines** of rendered terminal output as plain text (default 100), for shells, one-shot commands and agents alike.

`gmux agent logs <id>` prints the session's conversation as clean markdown for agents that persist a conversation file (pi): `## User` / `## Assistant` messages with compact `[tool] …` one-liners — the actual exchange, not the TUI's box-drawing and spinners. Here `-n N` counts **messages** (default 100), counted after the message-type filter: no flag gives user messages plus assistant prose, and `--user`/`--agent`/`--tool`/`--all` **replace** that set (`--agent -n 1` is the latest answer alone). `--json` prints a JSON array of `{role, type, text, prose}` — the machine contract for this read. There is no `--thinking`: no adapter renders thinking blocks, so the flag is refused by name instead of printing nothing.

`gmux agent status <id>` answers "what is going on with this session" in a fixed three-part shape — a state line (alive/dead, active/idle, the last *closed* turn's outcome and rough recency), the triggering excerpt, and the relevant content (the final answer when idle; the last few messages and a working indicator while a turn runs). A running turn is reported with no outcome, and a session that died mid-turn reads `the turn never finished (runner died)` — the same verdict `gmux wait` reaches for that row (`error`, cause `runner_died`), so the two verbs never disagree. `--json` gives one object with those three parts and a `content.kind` of `"answer"`, `"recent"` or `"none"`; fields describing a turn that does not exist (`state.last_turn_outcome`, `state.last_turn_cause`, `trigger.code`) are absent rather than empty, so branch on `state.active`/`state.turn_recorded`.

Both agent reads are snapshots of the agent's stored conversation: they never start or resume anything and work on a dead session, and they can be staler than the answer a `wait` or a synchronous `prompt` carries. Agents with no readable conversation answer `unsupported_adapter` for `logs`, while `status` still prints its (adapter-independent) state line with a note — `gmux tail` is the fallback for the content. Pair the reads with `wait` to capture an agent's final answer:

```bash
gmux agent prompt --timeout 600 <id> < ship-prompt.txt   # prints the reply
url=$(gmux agent logs --agent -n 1 <id> | grep -oE 'https://github\.com/[^ ]+/pull/[0-9]+' | tail -1)
echo "$url"
```

## Discovery and cleanup

```bash
gmux ls            # all local sessions, alive first, newest first
gmux ls --all      # include sessions on paired peer hosts (ids print as <id>@<peer>)
gmux ls --json     # machine-readable, for parsing in scripts
gmux kill <id>     # SIGTERM the runner, normal exit lifecycle
```

`send`, `tail`, and `kill` accept `<id>@<peer>` ids; `wait` and `gmux agent` (including `agent logs`) do not (local only — run `gmux agent` in a session on the owning host instead).

Every verb accepts id prefixes, full session ids, or slugs, so the eight-character short form `ls` prints passes straight back to `kill`, `send`, `tail`, or `wait`.

## Parallel orchestration

Spawn N agents in parallel, then wait for each in turn. Sequential waiting finishes when the slowest agent does — same wall-clock as backgrounding the `wait` calls, but exit codes are per-session and the loop reads as a straight line:

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  ids+=( "$(gmux -d -- pi "Implement $ticket. Return when you're done.")" )
done

for id in "${ids[@]}"; do
  gmux wait "$id" --timeout 600 || echo "$id did not finish cleanly: $?"
done

for id in "${ids[@]}"; do
  echo "=== $id ==="
  gmux agent logs "$id" -n 100
done
```

The agents run concurrently because `gmux -d -- pi <prompt>` returns as soon as the session registers and prints just the session id (no grep needed); the wait loop gates the harvest step on every agent reaching idle.

## Nested gmux

When `gmux -- <cmd>` runs inside an existing gmux session (detected via the `GMUX=1` env var), gmux auto-detaches into a headless background process so you don't get a PTY-within-PTY. The auto-detach only triggers when stdin is a TTY: agent harnesses whose stdin is a pipe land in the piped flow above and behave normally. You don't need to special-case nested invocations.

## Editor sessions as $EDITOR

`gmux edit <file>` opens a managed editor session, blocks until the editor closes, and propagates its exit code — so it works anywhere `$EDITOR` does (`git commit`, `crontab -e`). Inside gmux sessions, `EDITOR`/`VISUAL` already default to `gmux edit` when your dotfiles don't set them, so an agent asking git to open an editor gets a proper tab in the UI.

## Agent-specific integrations

Each adapter has its own status and resumption story. See:

- [pi](/integrations/pi/)
- [Codex](/integrations/codex/)
- [Claude Code](/integrations/claude-code/)
