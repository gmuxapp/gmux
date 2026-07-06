package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Handshake protocol used by spawnDetached's detached (-d) path so the
// parent process can return a deterministic session id to its caller
// (typically a script doing id=$(gmux -d -- foo)) without
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
// The env var is consumed at process startup (captureHandshakeFD),
// NOT at ack time. The runner forwards os.Environ to its PTY child,
// and the child is spawned before the ack — so a lazily-read env var
// leaks to every process inside the session. A nested `gmux` (e.g.
// `gmux edit` invoked as $EDITOR inside the session) would then
// interpret the stale "3" against its own fd table, where fd 3 is
// typically the Go runtime's epoll fd — writing to and closing it is
// an instant `netpoll failed` crash. Capturing + unsetting first
// thing in main() makes the inheritance impossible.
const handshakeFDEnv = "GMUX_HANDSHAKE_FD"

// capturedHandshakeFD is the fd number recorded by captureHandshakeFD,
// or -1 when this process has no handshake parent. Consumed (and
// reset) by the single handshakeAck call.
var capturedHandshakeFD = -1

// captureHandshakeFD reads GMUX_HANDSHAKE_FD, validates it, records
// the fd for the eventual handshakeAck, and ALWAYS unsets the env var
// so no child of this process can inherit it. Must be called at the
// top of main(), before anything can fork.
//
// Validation is deliberately paranoid: besides the numeric/range
// checks, the fd must actually be a pipe. This guards the rolling-
// upgrade window where an old runner (which leaked the env var to its
// session) is still alive: a nested gmux would otherwise ack against
// whatever its own fd 3 happens to be, and closing an arbitrary fd
// (e.g. the runtime's epoll fd) is fatal.
func captureHandshakeFD() {
	defer func() { _ = os.Unsetenv(handshakeFDEnv) }()
	fdStr := os.Getenv(handshakeFDEnv)
	if fdStr == "" {
		return
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil || fd < 3 {
		return
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return
	}
	capturedHandshakeFD = fd
}

// handshakeAck signals registration outcome to the parent process via
// the fd recorded by captureHandshakeFD. No-op when no handshake
// parent exists (the common case).
//
// Errors are swallowed: a write failure leaves the parent's read
// returning EOF, which is already the failure signal. The child
// continues running regardless, because the session itself is
// independent of whether the parent ever learns about it.
func handshakeAck(sessionID string, registered bool) {
	if capturedHandshakeFD < 0 {
		return
	}
	f := os.NewFile(uintptr(capturedHandshakeFD), "gmux-handshake")
	capturedHandshakeFD = -1
	if f == nil {
		return
	}
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
