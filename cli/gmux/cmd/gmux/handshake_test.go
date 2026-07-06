package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
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
	t.Cleanup(func() { capturedHandshakeFD = -1 })
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

	id, err := readHandshake(r, 2*time.Second)
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

	_, err := readHandshake(r, 2*time.Second)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err: got %v, want io.EOF", err)
	}
}

func TestReadHandshake_TimeoutWhenChildHangs(t *testing.T) {
	r, _ := pipePair(t) // w held open by cleanup, never written to

	start := time.Now()
	_, err := readHandshake(r, 50*time.Millisecond)
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

	_, err := readHandshake(r, 2*time.Second)
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

	_, err := readHandshake(r, 2*time.Second)
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

	id, readErr := readHandshake(r, 5*time.Second)
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
