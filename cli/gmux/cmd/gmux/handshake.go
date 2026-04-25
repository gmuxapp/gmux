package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Handshake protocol used by spawnDetached's --no-attach path so the
// parent process can return a deterministic session id to its caller
// (typically a script doing id=$(gmux --no-attach foo)) without
// polling /v1/sessions or guessing.
//
// Wire:
//   - Parent: os.Pipe(); attach write end as cmd.ExtraFiles[0] so the
//     child sees it as fd 3; set GMUX_HANDSHAKE_FD=3 in the child env;
//     cmd.Start; close parent's copy of the write end; readHandshake
//     on the read end with a 5s deadline.
//   - Child: after registerWithGmuxd, call handshakeAck. On registered
//     success it writes "<session-id>\n" to fd 3 and closes the fd;
//     on registered failure it closes without writing — the parent
//     sees EOF with zero bytes and surfaces a clean error.
//
// Failure modes the parent disambiguates:
//   - io.EOF                     → child exited before acking
//   - os.ErrDeadlineExceeded     → child wedged between register and ack
//   - "empty session id…"        → child wrote only whitespace
//
// After dispatching the ack, handshakeAck unsets GMUX_HANDSHAKE_FD so
// the (now-closed) fd doesn't get re-interpreted by any nested fork
// the runner spawns later (the user's command, or a `gmux` invoked
// from within it).
const handshakeFDEnv = "GMUX_HANDSHAKE_FD"

// handshakeAck signals registration outcome to the parent process via
// the file descriptor named in GMUX_HANDSHAKE_FD. No-op when the env
// var is unset (the common case: this gmux wasn't spawned with a
// handshake parent).
//
// Errors are swallowed: a malformed env var, a stale fd, or a write
// failure leaves the parent's read returning EOF, which is already
// the failure signal. The child continues running regardless, because
// the session itself is independent of whether the parent ever
// learns about it.
func handshakeAck(sessionID string, registered bool) {
	f := openHandshakeFD()
	if f == nil {
		return
	}
	// Clear the env var so the (about-to-be-closed) fd isn't
	// re-interpreted by any process the runner forks later. The
	// runner forwards os.Environ to user commands, and a nested
	// `gmux` would otherwise call handshakeAck against a fd that
	// in its own fd table is either closed or pointing at
	// something unrelated.
	_ = os.Unsetenv(handshakeFDEnv)
	handshakeAckFD(f, sessionID, registered)
}

// handshakeAckFD is the fd-driven core of the ack: writes the
// outcome to f and closes it. Split out from handshakeAck so tests
// can exercise the IO contract without the env-lookup glue, which
// avoids the fd-aliasing footgun that comes from os.NewFile-ing a
// fd a test still owns through a different *os.File.
func handshakeAckFD(f *os.File, sessionID string, registered bool) {
	defer f.Close()
	if registered {
		fmt.Fprintln(f, sessionID)
	}
}

// openHandshakeFD returns a *os.File wrapping GMUX_HANDSHAKE_FD, or
// nil if the env var is unset, malformed, or names one of fds 0-2
// (which are stdin/stdout/stderr and never a valid handshake
// channel).
func openHandshakeFD() *os.File {
	fdStr := os.Getenv(handshakeFDEnv)
	if fdStr == "" {
		return nil
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil || fd < 3 {
		return nil
	}
	return os.NewFile(uintptr(fd), "gmux-handshake")
}

// readHandshake blocks on r until the child writes its session id
// followed by a newline (success), closes its end without writing
// (failure → io.EOF), or the deadline fires
// (os.ErrDeadlineExceeded). Returns the trimmed session id on
// success.
//
// Strict framing: the protocol mandates "<id>\n" written atomically.
// Any read error is treated as failure, even if partial bytes
// arrived. This protects against a script consuming a half-written
// id from a child that crashed mid-write.
func readHandshake(r *os.File, timeout time.Duration) (string, error) {
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(line)
	if id == "" {
		return "", errors.New("empty session id from child")
	}
	return id, nil
}
