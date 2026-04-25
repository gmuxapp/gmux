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

// ── openHandshakeFD / handshakeAck: env-lookup + dispatch glue ──

func TestOpenHandshakeFD_NilWhenEnvUnset(t *testing.T) {
	t.Setenv(handshakeFDEnv, "")
	if f := openHandshakeFD(); f != nil {
		t.Fatalf("expected nil with empty env, got %+v", f)
	}
}

func TestOpenHandshakeFD_NilForInvalidEnv(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "1", "2", "-1"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(handshakeFDEnv, v)
			if f := openHandshakeFD(); f != nil {
				t.Fatalf("expected nil for %q, got %+v", v, f)
			}
		})
	}
}

// TestHandshakeAck_UnsetsEnvAfterDispatch is the regression test for
// the env-leak fix: after handshakeAck runs once, GMUX_HANDSHAKE_FD
// must be unset so a fork inheriting os.Environ doesn't try to write
// to the (now-closed) fd.
func TestHandshakeAck_UnsetsEnvAfterDispatch(t *testing.T) {
	_, w := pipePair(t)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(w.Fd())))

	handshakeAck("sess-xyz", true)

	if v := os.Getenv(handshakeFDEnv); v != "" {
		t.Fatalf("env not cleared after ack: got %q", v)
	}
}

// TestHandshakeAck_SecondCallIsNoOp follows from
// UnsetsEnvAfterDispatch: a second call within the same process
// finds an empty env and writes nothing. Captures the property from
// the consumer's perspective (one ack per pipe).
func TestHandshakeAck_SecondCallIsNoOp(t *testing.T) {
	r, w := pipePair(t)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(w.Fd())))

	handshakeAck("first-id", true)
	handshakeAck("second-id", true) // would re-write if env still set

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(data), "first-id\n"; got != want {
		t.Fatalf("bytes: got %q, want %q (second call should be no-op)", got, want)
	}
}

// TestHandshakeAck_NoOpWithoutEnv pins down the common-case fast
// path: handshakeAck returns immediately when the env var is absent,
// touching no fds. Asserted by the absence of side effects: the test
// should run cleanly with no goroutines blocked.
func TestHandshakeAck_NoOpWithoutEnv(t *testing.T) {
	t.Setenv(handshakeFDEnv, "")
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
