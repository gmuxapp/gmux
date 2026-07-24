package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
//   - Parent (spawnDetached): creates three pipes — control write end
//     (ExtraFiles[0] → child fd 3), gate read end (ExtraFiles[1] → child
//     fd 4), hold read end (ExtraFiles[2] → child fd 5); passes the
//     absolute startup deadline via env; calls cmd.Start; then reads
//     TARGET-PGID publication and registration ack from the control pipe,
//     writes a 'G' gate token to unblock the user command on success, and
//     closes the hold write end when cleanup is complete.
//   - Child runner (captureHandshakeFD → handshakeAck): captures and
//     clears all three env vars and fds at startup (setting O_CLOEXEC so
//     no exec'd process inherits them); derives a shared deadline context;
//     passes fd 3 and fd 4 to the target wrapper via ExtraFiles so it can
//     publish its PGID and receive the gate token; acks after
//     registerWithGmuxd — writes "<session-id>\n" to fd 3 on success or
//     closes without writing on failure. Calls waitForHandshakeRelease on
//     fd 5 at the very end so the parent can synchronise target cleanup.
//   - Target wrapper (__detached-target / runDetachedTarget): writes
//     "TARGET <pid>\n" to fd 3, reads a 'G' byte from fd 4 before
//     exec'ing the user command. Exit codes: 125 gate/protocol error,
//     126 exec error, 127 command not found.
//
// Failure modes the parent disambiguates:
//   - io.EOF                        → child exited before acking
//   - os.ErrDeadlineExceeded        → child wedged between register and ack
//   - "empty session id from child" → child wrote only whitespace
//   - "invalid session id …"        → child wrote a malformed id
//   - "handshake frame too large"   → child sent an oversized line
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
const (
	handshakeFDEnv       = "GMUX_HANDSHAKE_FD"
	handshakeGateFDEnv   = "GMUX_HANDSHAKE_GATE_FD"
	handshakeHoldFDEnv   = "GMUX_HANDSHAKE_HOLD_FD"
	handshakeDeadlineEnv = "GMUX_HANDSHAKE_DEADLINE"
	targetControlFDEnv   = "GMUX_TARGET_CONTROL_FD"
	targetGateFDEnv      = "GMUX_TARGET_GATE_FD"
)

// capturedHandshakeFD is the fd number recorded by captureHandshakeFD,
// or -1 when this process has no handshake parent. The deadline is captured
// at the same time so both internal variables are gone before a target forks.
var (
	capturedHandshakeFD       = -1
	capturedHandshakeGateFD   = -1
	capturedHandshakeHoldFD   = -1
	capturedHandshakeDeadline time.Time
	capturedHandshakeInvalid  bool
)

// captureHandshakeFD reads GMUX_HANDSHAKE_FD and the two companion fds,
// validates all three and ALWAYS unsets the env vars so no child of this
// process can inherit them. Must be called at the top of main(), before
// anything can fork.
//
// Validation is deliberately paranoid:
//
//   - All three fds must be genuine pipes (Fstat → S_IFIFO); accepting a
//     socket or file fd as gate/hold would silently duplicate it into the
//     target wrapper via ExtraFiles and close it in the runner afterward,
//     corrupting an unrelated live fd.
//   - All three must be distinct; aliasing creates multiple *os.File
//     finalizers on one raw fd, which is the aliasing footgun documented
//     in the comment above.
//   - Access directions are verified via F_GETFL (POSIX, portable to both
//     Linux and Darwin): control must be a write-end, gate and hold must
//     be read-ends. This rejects swapped-endpoint misconfigurations before
//     any fd is recorded, wrapped, or exec'd into a subprocess.
//
// If gate or hold validation fails but the control fd is a valid pipe, the
// control fd is recorded so handshakeAck can send an EOF-producing failure
// ack to the parent immediately rather than waiting for process exit.
func captureHandshakeFD() {
	defer func() {
		_ = os.Unsetenv(handshakeFDEnv)
		_ = os.Unsetenv(handshakeGateFDEnv)
		_ = os.Unsetenv(handshakeHoldFDEnv)
		_ = os.Unsetenv(handshakeDeadlineEnv)
	}()
	fdStr := os.Getenv(handshakeFDEnv)
	if fdStr == "" {
		return
	}

	// Validate the control fd as a writable FIFO before recording anything.
	controlFD, err := strconv.Atoi(fdStr)
	if err != nil || controlFD < 3 {
		return
	}
	if !isFIFO(controlFD) {
		return
	}

	// Record the control fd and protect it now: if gate/hold validation
	// fails below we can still send a prompt failure ack via handshakeAck.
	capturedHandshakeFD = controlFD
	// O_CLOEXEC: exec'd processes (e.g. autostarted gmuxd) must not inherit
	// this fd. exec.Cmd ExtraFiles re-dups with dup2 in the child regardless,
	// so the intended target-wrapper handoff is unaffected.
	syscall.CloseOnExec(controlFD)

	// Parse gate and hold WITHOUT recording or touching them yet.
	gateFD, gateErr := strconv.Atoi(os.Getenv(handshakeGateFDEnv))
	holdFD, holdErr := strconv.Atoi(os.Getenv(handshakeHoldFDEnv))
	if gateErr != nil || gateFD < 3 || holdErr != nil || holdFD < 3 {
		capturedHandshakeInvalid = true
		return
	}

	// Require all three fds to be distinct. Aliasing means multiple owning
	// *os.File finalizers on one raw fd and can silently close an unrelated
	// live fd when either wrapper is garbage-collected.
	if controlFD == gateFD || controlFD == holdFD || gateFD == holdFD {
		capturedHandshakeInvalid = true
		return
	}

	// Validate gate and hold as readable FIFOs. Do NOT call CloseOnExec or
	// record them before this check: calling CloseOnExec on an arbitrary live
	// fd (e.g. a socket) would silently modify its FD flags.
	if !isFIFO(gateFD) || !isFIFO(holdFD) {
		capturedHandshakeInvalid = true
		return
	}

	// Validate access directions. control must be a write-end; gate and hold
	// must be read-ends. Mismatches (e.g. swapped endpoints) are rejected
	// here before any fd is recorded, wrapped, or passed to a subprocess.
	if !isPipeWriteEnd(controlFD) || !isPipeReadEnd(gateFD) || !isPipeReadEnd(holdFD) {
		capturedHandshakeInvalid = true
		return
	}

	// All topology checks passed: record and protect the remaining fds.
	capturedHandshakeGateFD = gateFD
	capturedHandshakeHoldFD = holdFD
	syscall.CloseOnExec(gateFD)
	syscall.CloseOnExec(holdFD)

	nanos, err := strconv.ParseInt(os.Getenv(handshakeDeadlineEnv), 10, 64)
	if err != nil || nanos <= 0 {
		capturedHandshakeInvalid = true
		return
	}
	capturedHandshakeDeadline = time.Unix(0, nanos)
	capturedHandshakeInvalid = !capturedHandshakeDeadline.After(time.Now())
}

// isFIFO reports whether fd is an open anonymous or named pipe.
func isFIFO(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFIFO
}

// isPipeReadEnd reports whether fd is the read end of a pipe (O_RDONLY or
// O_RDWR). Uses F_GETFL which is POSIX and portable to Linux and Darwin.
func isPipeReadEnd(fd int) bool {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
	if errno != 0 {
		return false
	}
	acc := flags & 3 // O_ACCMODE mask; O_RDONLY=0, O_WRONLY=1, O_RDWR=2
	return acc == syscall.O_RDONLY || acc == syscall.O_RDWR
}

// isPipeWriteEnd reports whether fd is the write end of a pipe (O_WRONLY or
// O_RDWR).
func isPipeWriteEnd(fd int) bool {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
	if errno != 0 {
		return false
	}
	acc := flags & 3
	return acc == syscall.O_WRONLY || acc == syscall.O_RDWR
}

func handshakeContext() (context.Context, context.CancelFunc, bool) {
	if capturedHandshakeFD < 0 {
		ctx, cancel := context.WithCancel(context.Background())
		return ctx, cancel, false
	}
	if capturedHandshakeInvalid || capturedHandshakeDeadline.IsZero() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, func() {}, true
	}
	ctx, cancel := context.WithDeadline(context.Background(), capturedHandshakeDeadline)
	return ctx, cancel, true
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

// waitForHandshakeRelease drains the hold pipe until the parent closes its
// write end, then returns. The runner calls this at the very end of runSession
// (after deregister) so the parent can synchronise its target-group cleanup
// with the runner's exit — avoidng a race where the runner process table entry
// disappears before the parent has finished signalling the target group.
func waitForHandshakeRelease() {
	if capturedHandshakeHoldFD < 0 {
		return
	}
	f := os.NewFile(uintptr(capturedHandshakeHoldFD), "gmux-handshake-hold")
	capturedHandshakeHoldFD = -1
	if f == nil {
		return
	}
	_, _ = io.Copy(io.Discard, f)
	_ = f.Close()
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
func readHandshake(r *os.File, deadline time.Time) (string, error) {
	if err := r.SetReadDeadline(deadline); err != nil {
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
