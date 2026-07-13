---
name: gmux
description: Drive long-running terminal commands and AI coding agents through gmux sessions. Use when the user asks to run a command in the background, send input to a running session, wait for an agent's turn to finish, orchestrate multiple agents in parallel, or capture output from a tmux/screen-style session.
---

# gmux

A command run through gmux becomes a managed session the user can watch live
in a browser. The grammar is verb-first; **running a command always uses the
explicit `--` separator** so gmux never guesses where its own flags end and
the command begins.

## Primitives

```bash
gmux -- <cmd> [args]         # run blocking; exits with the child's exit code
gmux -d -- <cmd> [args]      # run detached; prints the session id on stdout
gmux send <id> 'text' Enter  # type text and submit (Enter is explicit)
gmux send <id> C-c           # send a control key (interrupt), no text
gmux send --wait <id> 'text' Enter  # send AND block until the reply is done
gmux send --follow-up <id> 'text'   # queue a prompt for after the current turn
gmux send --steering <id> 'text'    # interject a prompt into the current turn
gmux wait <id>               # block until idle (agent turn done / shell at prompt)
gmux wait <id> --for-text S  # block until S appears in the output
gmux tail <id> [-n N]        # last N lines of output (ANSI stripped; default 100)
gmux ls [--json]             # list sessions (--json for machine parsing)
gmux kill <id>               # SIGTERM the runner
```

`ls` IDs are 8-character prefixes; pass them directly to
`send` / `wait` / `tail` / `kill`. Tip: `alias gm='gmux --'` makes
`gm pytest` shorthand for `gmux -- pytest`.

Because `gmux -- <cmd>` propagates the child's exit code, it composes:
`if gmux -- pytest -q; then ...`.

## Sending input and keys

`send` types literal text; any trailing token that is a key name is sent as a
key. **Enter is not implicit** — add it to submit:

```bash
gmux send $id 'pytest -q' Enter   # type and run
gmux send $id 'half a line'       # type without submitting
gmux send $id C-c                 # interrupt (Ctrl-C)
gmux send $id Escape              # send Escape
echo "$body" | gmux send $id Enter  # pipe stdin, then submit (Enter optional)
```

Key names follow tmux: `Enter`, `Tab`, `Escape`, `Up`/`Down`/`Left`/`Right`,
`C-c`, `C-d`, etc. For verbatim tmux compatibility there is also
`gmux send-keys -t <id> <keys...>` (with `-l` for literal text).

**Everything after the id is verbatim** — including dash-leading tokens, so
`gmux send $id -v` sends a literal `-v` with no `--` guard needed. `send`'s own
flags (`--wait`, `--timeout`, `--follow-up`, `--steering`) only work *before*
the id.

### Prompting a busy agent: `--follow-up` vs `--steering`

When the target is an AI agent mid-turn, plain `Enter` vs the adapter's
queued-submit keystroke mean different things. Instead of memorizing
per-agent keybinds, use the mode flags — gmux appends the right submit
keystroke for the session's adapter (mutually exclusive; no trailing key
tokens; text only):

```bash
gmux send --follow-up $id 'then also update the changelog'  # runs after this turn
gmux send --steering $id 'stop — wrong branch, use main'    # delivered right now
gmux send --wait --follow-up $id 'next task'  # waits for the queued turn's reply
```

On an idle agent both are a plain submit. For agents whose Enter already
queues while busy (claude, codex) and for shells, both flags map to `Enter`.

## Sequential orchestration

```bash
id=$(gmux -d -- pi "implement the feature")
gmux wait $id

gmux send --wait $id "$(cat review.txt)" Enter

gmux tail $id -n 100
```

**Prefer `gmux send --wait` over `gmux send … && gmux wait`** for "send a
prompt and wait for the reply": the two-command composition can observe the
*previous* turn's idle state and return before the agent has even started,
while `--wait` is race-free (the daemon arms the wait before delivering the
input and requires a fresh working→idle transition). `--wait` requires the
input to submit (a trailing `Enter` or a `\r` in piped stdin) and accepts
`--timeout N`; exit codes match `gmux wait` below.

## Parallel orchestration

```bash
ids=()
for ticket in fa-48 fa-49 fa-52; do
  ids+=( "$(gmux -d -- pi "Implement $ticket. Return when done.")" )
done

for id in "${ids[@]}"; do
  gmux wait "$id" --timeout 600 || echo "$id failed: $?"
done

for id in "${ids[@]}"; do
  echo "=== $id ==="
  gmux tail "$id" -n 100
done
```

## Waiting

`gmux wait <id>` blocks until the session goes **idle** — an agent finishing
its turn, a shell finishing its command and returning to a fresh prompt, or
a one-shot command's process exiting — optionally bounded by `--timeout N`.
Exit codes:

- `0` session reached idle (including a one-shot completing / a shell
  exiting at its prompt)
- `2` session exited with its turn still open (crash mid-command/mid-turn)
- `3` `--timeout` elapsed

Every session is waitable. **Shell sessions** get per-command idle from OSC
133 prompt marks (fish emits them by default; bash/zsh need shell
integration): `gmux send --wait <id> 'make build' Enter` blocks until the
command finishes and the prompt returns. Sessions that never emit marks —
one-shot `gmux -d -- <cmd>` runs, or shells without integration — are one
lifetime-long turn: `wait` blocks until the process exits (`gmux -d -- pnpm
test; gmux wait $id` waits for the test run). Careful: an interactive shell
without integration never exits on its own, so bound that wait with
`--timeout` or use an output condition below. Waits issued after the exit
answer the same as live ones.

To wait for specific **output** instead of idle, use `--for-text <substr>` or
`--for-regex <pattern>` (works for shell sessions too — no grep loop needed):

```bash
gmux wait $id --for-text 'listening on' --timeout 60
gmux wait $id --for-regex 'tests? passed: \d+' --timeout 120
```

Same exit codes (`0` matched, `2` session exited first, `3` timeout). Matching
is line-wise against the rendered terminal output (ANSI stripped, same text
`gmux tail` shows), including output that appeared before the wait started, so
the pattern must fit on one terminal line.

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
