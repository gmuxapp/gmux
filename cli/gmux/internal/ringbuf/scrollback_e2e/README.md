# Scrollback E2E Tests

End-to-end tests that verify the `TermWriter` scrollback buffer against
real recorded terminal sessions.

## What this tests

The scrollback buffer is the source of truth for session replay in gmux.
When a client connects (or reconnects), it replays the scrollback snapshot
to reconstruct the terminal state. The TermWriter processes raw PTY output,
handles screen clears, CR/LF normalization, and frame trimming before
storing data in the ring buffer.

These tests verify a single invariant:

> Replaying the scrollback snapshot through a real terminal emulator must
> produce the same visible screen as replaying the original raw PTY output.

The approach:

1. **Record** a real pi TUI session through a PTY, capturing every byte
   including all escape sequences.
2. **Feed** the recording through `TermWriter` and take a `Snapshot()`.
3. **Render** both the raw recording and the snapshot through a VT100
   terminal emulator (`vito/midterm`).
4. **Compare** the two screens row by row (all 40 rows, including empty
   ones, so row-position differences are caught).

Three test functions run against every `testdata/*.bin` fixture:

- **TestScrollbackMatchesScreen**: the core visual comparison, run in both
  `single_write` and `chunked_writes` (997-byte chunks) feed modes.
- **TestScrollbackChunkedMatchesSingleWrite**: asserts that chunked writes
  produce byte-identical snapshots to a single write. Catches bugs in the
  cross-chunk escape sequence reassembly.
- **TestScrollbackSmallerThanRaw**: sanity check that the clear actually
  discarded pre-clear data.

## Fixtures

Binary files in `testdata/` are raw PTY recordings. All `.bin` files are
auto-discovered by the tests.

| File                       | Description                                                |
|----------------------------|------------------------------------------------------------|
| `pi_session.bin`           | Short reply ("say hello"), thinking off, ~39 KB            |
| `pi_thinking_session.bin`  | Long reply with thinking (is_prime function), ~240 KB      |

Each recording contains the full TUI lifecycle: startup frames (BSU/ESU),
user input, streaming response with differential redraws, screen clear
(`ESC[2J ESC[H ESC[3J`), and the post-clear final render.

## Recording new fixtures

Build the recorder:

```
cd cli/gmux
go build -o record-pty ./internal/ringbuf/scrollback_e2e/cmd/record-pty
```

Record a session (the output path should be inside `testdata/` so the
tests pick it up automatically):

```
./record-pty \
    -prompt "Write a fizzbuzz function in Go" \
    internal/ringbuf/scrollback_e2e/testdata/my_session.bin \
    pi --no-session --no-tools --no-extensions --no-skills \
       --no-prompt-templates --model "anthropic/claude-sonnet-4-6" \
       --thinking medium
```

The recorder creates a 120x40 PTY, waits for the TUI to start, types the
prompt char-by-char, waits for the response to complete (detected by a
second screen clear), then sends Ctrl-C and saves the recording.

Flags:

- `-prompt "..."` -- text to type after TUI startup
- `-wait 90s` -- max time to wait for response
- `-startup 3s` -- time to wait for TUI initialization
- `-rows 40` / `-cols 120` -- PTY dimensions

## Running the tests

```
cd cli/gmux
go test -v ./internal/ringbuf/scrollback_e2e/
```

When a test fails, it prints both screens with row numbers and the
differing lines highlighted, making it straightforward to see what the
scrollback replay gets wrong.
