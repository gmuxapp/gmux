# Scrollback E2E Tests

End-to-end tests that verify the `TermWriter` scrollback buffer against
real recorded terminal sessions.

## What this tests

The scrollback buffer is the source of truth for session replay in gmux.
When a client connects (or reconnects), it replays the scrollback snapshot
to reconstruct the terminal state.

The core invariant:

> Replaying the scrollback snapshot through a real terminal emulator must
> produce the same visible screen as replaying the original raw PTY output.

The tests record a real pi TUI session through a PTY, feed the recording
through `TermWriter`, then render both the original and the snapshot
through a VT100 emulator (`vito/midterm`) and compare row by row.

## Fixtures

Binary files in `testdata/` are raw PTY recordings, auto-discovered by
the tests. Drop a new `.bin` file in and it gets picked up automatically.

## Recording new fixtures

Build the recorder:

```
cd cli/gmux
go build -o record-pty ./internal/ringbuf/scrollback_e2e/cmd/record-pty
```

Record a session:

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

Run `record-pty -help` for all flags.
