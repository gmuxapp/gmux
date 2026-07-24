package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pipePair is a small helper: returns (r, w) from os.Pipe and
// registers a cleanup that closes whatever's still open.
func pipePair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// ── handshakeAckFD: the IO contract, no env, no fd aliasing ──

func TestHandshakeAckFD_WritesSessionIDOnSuccess(t *testing.T) {
	r, w := pipePair(t)
	handshakeAckFD(w, "abc-123", true)

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(data), "abc-123\n"; got != want {
		t.Fatalf("bytes: got %q, want %q", got, want)
	}
}

func TestHandshakeAckFD_ClosesWithoutWritingOnFailure(t *testing.T) {
	r, w := pipePair(t)
	handshakeAckFD(w, "abc-123", false)

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected zero bytes on failed ack, got %q", data)
	}
}

// TestHandshakeAckFD_ClosesFDOnSuccess pairs with the failure case to
// pin down the lifetime contract: regardless of outcome, the fd is
// closed by the time handshakeAckFD returns. Verified by reading until
// EOF on the matching read end and observing it terminates.
func TestHandshakeAckFD_ClosesFDOnSuccess(t *testing.T) {
	r, w := pipePair(t)
	handshakeAckFD(w, "abc-123", true)

	// If w were still open, ReadAll would block forever. The fact
	// that it returns is itself the assertion; data check is
	// belt-and-braces.
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "abc-123\n" {
		t.Fatalf("bytes: got %q, want %q", data, "abc-123\n")
	}
}

// ── captureHandshakeFD / handshakeAck: startup capture + dispatch ──

// resetCapture isolates the package-level capture state per test.
func resetCapture(t *testing.T) {
	t.Helper()
	capturedHandshakeFD = -1
	capturedHandshakeGateFD = -1
	capturedHandshakeHoldFD = -1
	capturedHandshakeDeadline = time.Time{}
	capturedHandshakeInvalid = false
	t.Cleanup(func() {
		capturedHandshakeFD = -1
		capturedHandshakeGateFD = -1
		capturedHandshakeHoldFD = -1
		capturedHandshakeDeadline = time.Time{}
		capturedHandshakeInvalid = false
	})
}

func TestCaptureHandshakeFD_NoopWhenEnvUnset(t *testing.T) {
	resetCapture(t)
	t.Setenv(handshakeFDEnv, "")
	captureHandshakeFD()
	if capturedHandshakeFD != -1 {
		t.Fatalf("expected no capture with empty env, got fd %d", capturedHandshakeFD)
	}
}

func TestCaptureHandshakeFD_RejectsInvalidEnv(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "1", "2", "-1"} {
		t.Run(v, func(t *testing.T) {
			resetCapture(t)
			t.Setenv(handshakeFDEnv, v)
			captureHandshakeFD()
			if capturedHandshakeFD != -1 {
				t.Fatalf("expected rejection for %q, got fd %d", v, capturedHandshakeFD)
			}
			if got := os.Getenv(handshakeFDEnv); got != "" {
				t.Fatalf("env not cleared for %q: got %q", v, got)
			}
		})
	}
}

// TestCaptureHandshakeFD_UnsetsEnvImmediately is the regression test
// for the env-leak crash: the env var must be gone BEFORE any child
// is spawned (i.e. right after capture), not merely after the ack.
// A leaked GMUX_HANDSHAKE_FD=3 makes a nested gmux inside the session
// close its own fd 3 — typically the Go runtime's epoll fd — which is
// a fatal `netpoll failed`.
func TestCaptureHandshakeFD_UnsetsEnvImmediately(t *testing.T) {
	resetCapture(t)
	_, w := pipePair(t)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(w.Fd())))

	captureHandshakeFD()

	if v := os.Getenv(handshakeFDEnv); v != "" {
		t.Fatalf("env not cleared after capture: got %q", v)
	}
	if capturedHandshakeFD != int(w.Fd()) {
		t.Fatalf("captured fd = %d, want %d", capturedHandshakeFD, int(w.Fd()))
	}
}

// TestCaptureHandshakeFD_RejectsNonPipeFD guards the rolling-upgrade
// window: an OLD runner may still leak GMUX_HANDSHAKE_FD=3 into its
// session, where a NEW nested gmux's fd 3 is something unrelated
// (epoll fd, an open file, ...). Capture must refuse any fd that
// isn't a pipe, and must not close or touch it.
func TestCaptureHandshakeFD_RejectsNonPipeFD(t *testing.T) {
	resetCapture(t)
	f, err := os.Open(os.DevNull) // char device, not a FIFO
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer f.Close()
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(f.Fd())))

	captureHandshakeFD()
	handshakeAck("sess-xyz", true) // must be a no-op

	if capturedHandshakeFD != -1 {
		t.Fatalf("non-pipe fd was captured: %d", capturedHandshakeFD)
	}
	// The fd must still be usable — proving nothing closed it.
	if _, err := f.Stat(); err != nil {
		t.Fatalf("fd was damaged by capture/ack: %v", err)
	}
}

// TestHandshakeAck_SecondCallIsNoOp: the captured fd is consumed by
// the first ack; a second call within the same process writes
// nothing (one ack per pipe).
func TestHandshakeAck_SecondCallIsNoOp(t *testing.T) {
	resetCapture(t)
	r, w := pipePair(t)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(w.Fd())))
	captureHandshakeFD()

	handshakeAck("first-id", true)
	handshakeAck("second-id", true) // would re-write if fd still captured

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(data), "first-id\n"; got != want {
		t.Fatalf("bytes: got %q, want %q (second call should be no-op)", got, want)
	}
}

// TestHandshakeAck_NoOpWithoutCapture pins down the common-case fast
// path: handshakeAck returns immediately when no handshake parent
// was captured, touching no fds.
func TestHandshakeAck_NoOpWithoutCapture(t *testing.T) {
	resetCapture(t)
	handshakeAck("abc", true)
	handshakeAck("abc", false)
}

// ── readHandshake: parent-side framing ──

func TestReadHandshake_RoundTripWithHandshakeAckFD(t *testing.T) {
	r, w := pipePair(t)
	go handshakeAckFD(w, "sess-xyz", true)

	id, err := readHandshake(r, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatalf("readHandshake: %v", err)
	}
	if id != "sess-xyz" {
		t.Fatalf("id: got %q, want %q", id, "sess-xyz")
	}
}

func TestReadHandshake_ChildFailedReturnsEOF(t *testing.T) {
	r, w := pipePair(t)
	handshakeAckFD(w, "ignored", false) // close-without-write

	_, err := readHandshake(r, time.Now().Add(2*time.Second))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err: got %v, want io.EOF", err)
	}
}

func TestReadHandshake_TimeoutWhenChildHangs(t *testing.T) {
	r, _ := pipePair(t) // w held open by cleanup, never written to

	start := time.Now()
	_, err := readHandshake(r, time.Now().Add(50*time.Millisecond))
	elapsed := time.Since(start)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err: got %v, want os.ErrDeadlineExceeded", err)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("deadline fired too early: %v", elapsed)
	}
}

func TestReadHandshake_EmptyLineRejected(t *testing.T) {
	r, w := pipePair(t)
	go func() {
		_, _ = w.Write([]byte("\n"))
		_ = w.Close()
	}()

	_, err := readHandshake(r, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatalf("expected error for empty id, got nil")
	}
}

// TestReadHandshake_PartialWriteRejected is the strict-framing
// regression: a child that writes "sess-abc" (no newline) and dies
// must NOT result in the parent reporting "sess-abc" as a valid id.
// The protocol is "<id>\n" written atomically; partial bytes are a
// failure.
func TestReadHandshake_PartialWriteRejected(t *testing.T) {
	r, w := pipePair(t)
	go func() {
		_, _ = w.Write([]byte("sess-abc")) // no newline
		_ = w.Close()
	}()

	_, err := readHandshake(r, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatalf("expected error for partial write, got nil")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err: got %v, want io.EOF (bufio surfaces EOF when delim missing)", err)
	}
}

// ── End-to-end: real subprocess + cmd.ExtraFiles ──
//
// The in-process tests above can't catch bugs in the actual cross-
// process plumbing: ExtraFiles ↔ fd-3 mapping, env inheritance,
// setsid + ExtraFiles compatibility. This test re-execs the test
// binary with the same env+fd layout that spawnDetached uses in
// production, has the child call handshakeAck, and verifies the
// parent reads the id back through readHandshake. A failure here
// flags a regression in any of those production-only mechanics.
//
// The child branch is the if-block at the top of the function:
// when the test binary is re-executed with GMUX_HANDSHAKE_TEST_CHILD=1,
// it pretends to be a runner that just finished a successful
// register, calls handshakeAck, and exits.
func TestPipeHandshakeRoundTripViaSubprocess(t *testing.T) {
	const childIDEnv = "GMUX_HANDSHAKE_TEST_CHILD"
	const expectedID = "sess-fromchild"

	if os.Getenv(childIDEnv) == "1" {
		// Mirror production: main() captures at startup, the runner
		// acks after registration.
		captureHandshakeFD()
		handshakeAck(expectedID, true)
		os.Exit(0)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestPipeHandshakeRoundTripViaSubprocess$", "-test.v")
	cmd.Env = append(os.Environ(),
		childIDEnv+"=1",
		handshakeFDEnv+"=3",
	)
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// Parent's copy of the write end must close so EOF on read
	// means "child failed". Same pattern as production.
	if err := w.Close(); err != nil {
		t.Fatalf("close parent write end: %v", err)
	}

	id, readErr := readHandshake(r, time.Now().Add(5*time.Second))
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Fatalf("child exited non-zero: %v", waitErr)
	}
	if readErr != nil {
		t.Fatalf("readHandshake: %v", readErr)
	}
	if id != expectedID {
		t.Fatalf("id: got %q, want %q", id, expectedID)
	}
}

// ── F1: CloseOnExec regression ──

// TestCaptureHandshakeFD_SetsCloseOnExec verifies that captureHandshakeFD
// sets O_CLOEXEC on all three captured fds. The test first clears CLOEXEC to
// mirror what happens when the runner receives the fds via exec.Cmd.ExtraFiles
// (dup2 in the child clears CLOEXEC on the new fd).
func TestCaptureHandshakeFD_SetsCloseOnExec(t *testing.T) {
	resetCapture(t)
	_, control := pipePair(t) // handshake write end
	gateR, _ := pipePair(t)  // gate read end
	holdR, _ := pipePair(t)  // hold read end

	// Clear CLOEXEC to simulate receiving via ExtraFiles (dup2 clears it).
	for _, fd := range []uintptr{control.Fd(), gateR.Fd(), holdR.Fd()} {
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, 0); errno != 0 {
			t.Fatalf("F_SETFD clear: %v", errno)
		}
	}

	deadline := time.Now().Add(time.Second).Truncate(time.Nanosecond)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(control.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()

	if capturedHandshakeFD < 0 || capturedHandshakeGateFD < 0 || capturedHandshakeHoldFD < 0 {
		t.Fatalf("capture failed: control=%d gate=%d hold=%d",
			capturedHandshakeFD, capturedHandshakeGateFD, capturedHandshakeHoldFD)
	}
	for _, tc := range []struct {
		name string
		fd   int
	}{
		{"control", capturedHandshakeFD},
		{"gate", capturedHandshakeGateFD},
		{"hold", capturedHandshakeHoldFD},
	} {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(tc.fd), syscall.F_GETFD, 0)
		if errno != 0 {
			t.Errorf("%s fd %d: F_GETFD: %v", tc.name, tc.fd, errno)
			continue
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			t.Errorf("%s fd %d: O_CLOEXEC not set (F_GETFD flags=%#x)", tc.name, tc.fd, flags)
		}
	}
}

// TestCaptureHandshakeFD_ExecDoesNotLeakFDs is the production-seam exec
// regression for F1. It simulates the runner receiving the three handshake
// fds via exec.Cmd.ExtraFiles (dup2, no CLOEXEC in child), calling
// captureHandshakeFD (which must set CLOEXEC on all three), and then
// exec'ing a grandchild process without ExtraFiles — simulating startGmuxd.
// The grandchild must not see any of the three handshake fds as open pipes.
func TestCaptureHandshakeFD_ExecDoesNotLeakFDs(t *testing.T) {
	const roleEnv = "GMUX_FDLEAK_ROLE"
	switch os.Getenv(roleEnv) {
	case "runner":
		// Simulate the runner: fds 3/4/5 arrived via ExtraFiles without
		// CLOEXEC. captureHandshakeFD must set it before any exec.
		captureHandshakeFD()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureHandshakeFD_ExecDoesNotLeakFDs$")
		cmd.Env = append(os.Environ(), roleEnv+"=grandchild")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "grandchild: %s", stderr.String())
			os.Exit(1)
		}
		os.Exit(0)
	case "grandchild":
		// Check that none of fds 3, 4, 5 are open pipes (pipes are the only
		// type that handshake fds can be; Go runtime internal fds are not).
		var leaked []string
		for _, fd := range []int{3, 4, 5} {
			var st syscall.Stat_t
			if err := syscall.Fstat(fd, &st); err == nil {
				if st.Mode&syscall.S_IFMT == syscall.S_IFIFO {
					leaked = append(leaked, strconv.Itoa(fd))
				}
			}
		}
		if len(leaked) > 0 {
			fmt.Fprintln(os.Stderr, "leaked pipe fds: "+strings.Join(leaked, ","))
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Test body: create the three handshake pipes and spawn the runner via
	// ExtraFiles, mirroring spawnDetached's setup.
	// fd 3 = handshakeW (control write end), fd 4 = gateR (gate read end),
	// fd 5 = holdR (hold read end) — matching the protocol direction contract.
	handshakeR, handshakeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer handshakeR.Close()
	gateR, gateW, err := os.Pipe() // gateR = read end → fd 4 in child
	if err != nil {
		t.Fatal(err)
	}
	defer gateR.Close()
	defer gateW.Close()
	holdR, holdW, err := os.Pipe() // holdR = read end → fd 5 in child
	if err != nil {
		t.Fatal(err)
	}
	defer holdR.Close()
	defer holdW.Close()

	deadline := time.Now().Add(time.Second)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureHandshakeFD_ExecDoesNotLeakFDs$")
	cmd.ExtraFiles = []*os.File{handshakeW, gateR, holdR} // fd 3=control write, 4=gate read, 5=hold read
	cmd.Env = append(os.Environ(),
		roleEnv+"=runner",
		handshakeFDEnv+"=3",
		handshakeGateFDEnv+"=4",
		handshakeHoldFDEnv+"=5",
		handshakeDeadlineEnv+"="+strconv.FormatInt(deadline.UnixNano(), 10),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Close parent's copies so the only references are via the child's ExtraFiles.
	_ = handshakeW.Close()
	_ = gateR.Close()
	_ = holdR.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("fd leak detected: %s", stderr.String())
	}
}

// ── Topology validation: non-FIFO / alias / swapped-direction ──

// fdFlagsGet returns the F_GETFD flags for fd, or -1 on error.
func fdFlagsGet(fd uintptr) int {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		return -1
	}
	return int(flags)
}

// TestCaptureHandshakeFD_ValidatesGateAsReadableFIFO verifies that a
// non-FIFO fd passed as the gate is rejected without touching the fd:
// CloseOnExec is not called on it, capturedHandshakeGateFD is never set,
// and the fd remains open and undamaged after captureHandshakeFD returns.
func TestCaptureHandshakeFD_ValidatesGateAsReadableFIFO(t *testing.T) {
	resetCapture(t)
	_, control := pipePair(t)  // write end → control
	holdR, _ := pipePair(t)   // read end → hold

	nonPipe, err := os.Open(os.DevNull) // char device, not a FIFO
	if err != nil {
		t.Fatal(err)
	}
	defer nonPipe.Close()

	flagsBefore := fdFlagsGet(nonPipe.Fd())

	deadline := time.Now().Add(time.Second)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(control.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(nonPipe.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()

	// Control fd is captured so a failure ack can reach the parent promptly.
	if capturedHandshakeFD < 0 {
		t.Error("control fd not captured (needed for prompt failure ack)")
	}
	// Gate fd must NOT be recorded or touched.
	if !capturedHandshakeInvalid {
		t.Error("handshake not marked invalid for non-FIFO gate")
	}
	if capturedHandshakeGateFD >= 0 {
		t.Errorf("capturedHandshakeGateFD=%d recorded for non-FIFO gate", capturedHandshakeGateFD)
	}
	// The non-pipe fd must be undamaged: still open and FD flags unchanged
	// (CloseOnExec must not have been called on an unverified fd).
	if _, err := nonPipe.Stat(); err != nil {
		t.Errorf("non-FIFO gate fd was damaged (Stat failed): %v", err)
	}
	if got := fdFlagsGet(nonPipe.Fd()); got != flagsBefore {
		t.Errorf("non-FIFO gate FD flags changed: before=%#x after=%#x", flagsBefore, got)
	}
}

// TestCaptureHandshakeFD_ValidatesHoldAsReadableFIFO mirrors the gate test
// for the hold fd.
func TestCaptureHandshakeFD_ValidatesHoldAsReadableFIFO(t *testing.T) {
	resetCapture(t)
	_, control := pipePair(t) // write end → control
	gateR, _ := pipePair(t)  // read end → gate

	nonPipe, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer nonPipe.Close()

	flagsBefore := fdFlagsGet(nonPipe.Fd())

	deadline := time.Now().Add(time.Second)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(control.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(nonPipe.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()

	if capturedHandshakeFD < 0 {
		t.Error("control fd not captured (needed for prompt failure ack)")
	}
	if !capturedHandshakeInvalid {
		t.Error("handshake not marked invalid for non-FIFO hold")
	}
	if capturedHandshakeHoldFD >= 0 {
		t.Errorf("capturedHandshakeHoldFD=%d recorded for non-FIFO hold", capturedHandshakeHoldFD)
	}
	if _, err := nonPipe.Stat(); err != nil {
		t.Errorf("non-FIFO hold fd damaged (Stat failed): %v", err)
	}
	if got := fdFlagsGet(nonPipe.Fd()); got != flagsBefore {
		t.Errorf("non-FIFO hold FD flags changed: before=%#x after=%#x", flagsBefore, got)
	}
}

// TestCaptureHandshakeFD_RequiresDistinctFDs verifies all three alias pairs:
// gate==control, gate==hold, control==hold. Every pair must be rejected.
func TestCaptureHandshakeFD_RequiresDistinctFDs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		aliasFDEnv   string // the env var to set to the same fd as another
		aliasWith    string // which env var to alias with ("control", "gate", "hold")
	}{
		{"gate==control", handshakeGateFDEnv, "control"},
		{"hold==control", handshakeHoldFDEnv, "control"},
		{"gate==hold", handshakeGateFDEnv, "hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCapture(t)
			_, control := pipePair(t)  // write end → control
			gateR, _ := pipePair(t)   // read end → gate
			holdR, _ := pipePair(t)   // read end → hold

			aliasTarget := func(which string) *os.File {
				switch which {
				case "control":
					return control
				case "gate":
					return gateR
				default:
					return holdR
				}
			}

			t.Setenv(handshakeFDEnv, strconv.Itoa(int(control.Fd())))
			t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
			t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
			// Override the aliased fd to match its pair.
			t.Setenv(tc.aliasFDEnv, strconv.Itoa(int(aliasTarget(tc.aliasWith).Fd())))
			t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(time.Now().Add(time.Second).UnixNano(), 10))
			captureHandshakeFD()

			if capturedHandshakeFD < 0 {
				t.Error("control fd not captured (needed for prompt failure ack)")
			}
			if !capturedHandshakeInvalid {
				t.Errorf("handshake not marked invalid for %s alias", tc.name)
			}
			if capturedHandshakeGateFD >= 0 {
				t.Errorf("capturedHandshakeGateFD=%d despite %s alias", capturedHandshakeGateFD, tc.name)
			}
		})
	}
}

// TestCaptureHandshakeFD_ValidatesGateDirection verifies that passing the
// write end (instead of read end) as the gate fd is rejected, and that the
// write-end pipe fd is left undamaged (no CloseOnExec, still open).
func TestCaptureHandshakeFD_ValidatesGateDirection(t *testing.T) {
	resetCapture(t)
	_, control := pipePair(t) // write end → control (correct)
	_, gateW := pipePair(t)  // write end → gate (WRONG: should be read end)
	holdR, _ := pipePair(t)  // read end → hold (correct)

	flagsBefore := fdFlagsGet(gateW.Fd())

	deadline := time.Now().Add(time.Second)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(control.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateW.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()

	if !capturedHandshakeInvalid {
		t.Error("handshake not marked invalid for write-end gate")
	}
	if capturedHandshakeGateFD >= 0 {
		t.Errorf("capturedHandshakeGateFD=%d despite wrong direction", capturedHandshakeGateFD)
	}
	// The write-end fd must be undamaged.
	if _, err := gateW.Stat(); err != nil {
		t.Errorf("write-end gate fd damaged: %v", err)
	}
	if got := fdFlagsGet(gateW.Fd()); got != flagsBefore {
		t.Errorf("write-end gate FD flags changed: before=%#x after=%#x", flagsBefore, got)
	}
}

// TestCaptureHandshakeFD_ValidatesControlDirection verifies that passing the
// read end (instead of write end) as the control fd is caught: the handshake
// is marked invalid and gate/hold are not recorded.
func TestCaptureHandshakeFD_ValidatesControlDirection(t *testing.T) {
	resetCapture(t)
	controlR, _ := pipePair(t) // read end → control (WRONG: should be write end)
	gateR, _ := pipePair(t)   // read end → gate (correct)
	holdR, _ := pipePair(t)   // read end → hold (correct)

	deadline := time.Now().Add(time.Second)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(controlR.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()

	// The control fd IS a FIFO so capturedHandshakeFD is set (allowing a
	// failure ack). The direction check must still mark the handshake invalid
	// and leave gate/hold unrecorded.
	if !capturedHandshakeInvalid {
		t.Error("handshake not marked invalid for read-end control")
	}
	if capturedHandshakeGateFD >= 0 {
		t.Errorf("capturedHandshakeGateFD=%d recorded despite invalid control direction", capturedHandshakeGateFD)
	}
	if capturedHandshakeHoldFD >= 0 {
		t.Errorf("capturedHandshakeHoldFD=%d recorded despite invalid control direction", capturedHandshakeHoldFD)
	}
}

// TestCaptureHandshakeFD_ValidatesHoldDirection verifies that passing the
// write end (instead of read end) as the hold fd is rejected without touching
// the write-end fd (no CloseOnExec, still open).
func TestCaptureHandshakeFD_ValidatesHoldDirection(t *testing.T) {
	resetCapture(t)
	_, control := pipePair(t)  // write end → control (correct)
	gateR, _ := pipePair(t)   // read end → gate (correct)
	_, holdW := pipePair(t)   // write end → hold (WRONG: should be read end)

	flagsBefore := fdFlagsGet(holdW.Fd())

	deadline := time.Now().Add(time.Second)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(control.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdW.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()

	if !capturedHandshakeInvalid {
		t.Error("handshake not marked invalid for write-end hold")
	}
	if capturedHandshakeHoldFD >= 0 {
		t.Errorf("capturedHandshakeHoldFD=%d despite wrong direction", capturedHandshakeHoldFD)
	}
	// The write-end hold fd must be undamaged.
	if _, err := holdW.Stat(); err != nil {
		t.Errorf("write-end hold fd damaged: %v", err)
	}
	if got := fdFlagsGet(holdW.Fd()); got != flagsBefore {
		t.Errorf("write-end hold FD flags changed: before=%#x after=%#x", flagsBefore, got)
	}
}

// TestMalformedGateHandshakeSendsPromptFailure is a production-seam subprocess
// test proving that a runner with a non-FIFO gate fd sends the failure ack
// promptly (not at the 30 s deadline) and never issues the gate token, so no
// user command can be exec'd.
func TestMalformedGateHandshakeSendsPromptFailure(t *testing.T) {
	const roleEnv = "GMUX_MALFGATE_ROLE"
	switch os.Getenv(roleEnv) {
	case "runner":
		// Simulate the runner: fds 3/4/5 arrive via ExtraFiles.
		// fd 4 is a tempfile (non-FIFO), so captureHandshakeFD marks invalid.
		captureHandshakeFD()
		// handshakeAck closes capturedHandshakeFD (fd 3 = control write end),
		// letting the parent read EOF on its handshakeRead — prompt failure.
		handshakeAck("", false)
		os.Exit(0)
	}

	// Parent side: create the three pipes and one tempfile for the bogus gate.
	handshakeR, handshakeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer handshakeR.Close()
	gateR, gateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateR.Close()
	defer gateW.Close()
	holdR, holdW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer holdR.Close()
	defer holdW.Close()

	// Use a regular file instead of a pipe as the gate fd — not a FIFO.
	bogusGate, err := os.CreateTemp(t.TempDir(), "bogus-gate")
	if err != nil {
		t.Fatal(err)
	}
	defer bogusGate.Close()

	deadline := time.Now().Add(2 * time.Second)
	cmd := exec.Command(os.Args[0], "-test.run=^TestMalformedGateHandshakeSendsPromptFailure$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{handshakeW, bogusGate, holdR} // fd 3=control, 4=bogusGate, 5=hold
	cmd.Env = append(os.Environ(),
		roleEnv+"=runner",
		handshakeFDEnv+"=3",
		handshakeGateFDEnv+"=4",
		handshakeHoldFDEnv+"=5",
		handshakeDeadlineEnv+"="+strconv.FormatInt(deadline.UnixNano(), 10),
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Close parent's ExtraFiles copies after the child is forked.
	_ = handshakeW.Close()
	_ = bogusGate.Close()
	_ = holdR.Close()

	_, err = awaitDetachedHandshake(cmd, handshakeR, gateW, holdW, deadline, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for malformed gate fd")
	}
	// The failure ack (EOF on handshakeR) must arrive promptly — not at the
	// 2 s deadline — proving the runner detected the invalid gate and sent
	// the ack immediately instead of proceeding with the handshake.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("malformed gate caused deadline timeout instead of prompt failure ack: %v", err)
	}
	// The gate must not have received the 'G' token: awaitDetachedHandshake
	// closed gateW without writing on failure, so gateR sees only EOF.
	gateR.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var tok [1]byte
	if n, _ := gateR.Read(tok[:]); n > 0 {
		t.Errorf("gate received unexpected byte %q — no user command should start", tok[:n])
	}
}
