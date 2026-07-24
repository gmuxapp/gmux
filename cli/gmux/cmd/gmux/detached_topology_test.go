package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessGroupCleanupWaitsForDescendants(t *testing.T) {
	role := os.Getenv("GMUX_GROUP_ROLE")
	if role == "leader" {
		child := exec.Command(os.Args[0], "-test.run=^TestProcessGroupCleanupWaitsForDescendants$")
		child.Env = append(os.Environ(), "GMUX_GROUP_ROLE=descendant")
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		_ = os.WriteFile(os.Getenv("GMUX_GROUP_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		os.Exit(0)
	}
	if role == "descendant" {
		signalIgnore(syscall.SIGHUP, syscall.SIGTERM)
		for {
			time.Sleep(time.Second)
		}
	}
	pidFile := t.TempDir() + "/descendant.pid"
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessGroupCleanupWaitsForDescendants$")
	cmd.Env = append(os.Environ(), "GMUX_GROUP_ROLE=leader", "GMUX_GROUP_PIDFILE="+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	descendant, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	terminateProcessGroup(pgid, 30*time.Millisecond)
	if err := syscall.Kill(descendant, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("HUP-ignoring descendant alive: %v", err)
	}
}

func TestDetachedTopologyCleanup(t *testing.T) {
	role := os.Getenv("GMUX_TOPOLOGY_ROLE")
	if role == "runner" {
		signalIgnore(syscall.SIGTERM, syscall.SIGHUP)
		cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedTopologyCleanup$")
		cmd.Env = append(os.Environ(), "GMUX_TOPOLOGY_ROLE=target")
		cmd.ExtraFiles = []*os.File{os.NewFile(3, "control"), os.NewFile(4, "gate")}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(91)
		}
		if os.Getenv("GMUX_TOPOLOGY_EOF") == "1" {
			_ = os.NewFile(3, "control").Close()
		}
		for {
			time.Sleep(time.Second)
		}
	}
	if role == "target" {
		signalIgnore(syscall.SIGTERM, syscall.SIGHUP)
		pid := os.Getpid()
		_ = os.WriteFile(os.Getenv("GMUX_TOPOLOGY_PIDFILE"), []byte(strconv.Itoa(pid)), 0o600)
		control := os.NewFile(3, "control")
		gate := os.NewFile(4, "gate")
		_, _ = fmt.Fprintf(control, "TARGET %d\n", pid)
		_ = control.Close()
		var token [1]byte
		_, err := io.ReadFull(gate, token[:])
		if err != nil {
			os.Exit(0)
		}
		// If allowed to start, create a same-group descendant that ignores HUP
		// and TERM; parent cleanup must use group disappearance, not leader exit.
		child := exec.Command(os.Args[0], "-test.run=^TestDetachedTopologyCleanup$")
		child.Env = append(os.Environ(), "GMUX_TOPOLOGY_ROLE=descendant")
		_ = child.Start()
		os.Exit(0)
	}
	if role == "descendant" {
		signalIgnore(syscall.SIGTERM, syscall.SIGHUP)
		for {
			time.Sleep(time.Second)
		}
	}

	for _, tc := range []struct {
		name string
		eof  bool
	}{{"deadline", false}, {"fatal EOF", true}} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := t.TempDir() + "/target.pid"
			controlR, controlW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			gateR, gateW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedTopologyCleanup$")
			cmd.Env = append(os.Environ(), "GMUX_TOPOLOGY_ROLE=runner", "GMUX_TOPOLOGY_PIDFILE="+pidFile)
			if tc.eof {
				cmd.Env = append(cmd.Env, "GMUX_TOPOLOGY_EOF=1")
			}
			cmd.ExtraFiles = []*os.File{controlW, gateR}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = controlW.Close()
			_ = gateR.Close()
			holdR, hold, _ := os.Pipe()
			_ = holdR.Close()
			_, err = awaitDetachedHandshake(cmd, controlR, gateW, hold, time.Now().Add(50*time.Millisecond), 250*time.Millisecond)
			_ = controlR.Close()
			if err == nil {
				t.Fatal("expected failure")
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("runner alive: %v", err)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("target alive: %v", err)
			}
		})
	}
}

func signalIgnore(signals ...syscall.Signal) {
	for _, sig := range signals {
		signal.Ignore(sig)
	}
}

// ── F3: bounded post-SIGKILL loop ──

// TestTerminateProcessGroupBoundedAfterKill verifies that terminateProcessGroup
// returns within a bounded time even when the group does not exit on SIGHUP.
// A process that ignores SIGHUP/SIGTERM falls through to the SIGKILL path;
// the test confirms that path has a deadline and does not spin forever.
func TestTerminateProcessGroupBoundedAfterKill(t *testing.T) {
	if os.Getenv("GMUX_PGBOUND_ROLE") == "victim" {
		signalIgnore(syscall.SIGHUP, syscall.SIGTERM)
		for {
			time.Sleep(time.Second)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminateProcessGroupBoundedAfterKill$")
	cmd.Env = append(os.Environ(), "GMUX_PGBOUND_ROLE=victim")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	grace := 100 * time.Millisecond
	start := time.Now()
	terminateProcessGroup(pgid, grace)
	elapsed := time.Since(start)
	// SIGHUP ignored → first loop exhausts grace. SIGKILL succeeds immediately.
	// Total should be well within 2×grace + scheduling slack.
	if elapsed > 4*grace {
		t.Errorf("terminateProcessGroup took %v, want ≤ %v (bounded loop)", elapsed, 4*grace)
	}
	_ = cmd.Wait()
}

// ── F5: control-protocol validation ──

// TestAwaitDetachedHandshakeProtocolValidation checks that malformed frames
// (empty session id, invalid session id, oversized line) cause a prompt error
// from awaitDetachedHandshake rather than waiting out the full deadline.
func TestAwaitDetachedHandshakeProtocolValidation(t *testing.T) {
	if os.Getenv("GMUX_PROTO_HELPER") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}
	startHelper := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAwaitDetachedHandshakeProtocolValidation$")
		cmd.Env = append(os.Environ(), "GMUX_PROTO_HELPER=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	// deadline for awaitDetachedHandshake; a correct fix makes errors
	// arrive well before this.
	const longDeadline = 2 * time.Second

	t.Run("empty_session_id_does_not_wait_out_deadline", func(t *testing.T) {
		cmd := startHelper(t)
		r, w, _ := os.Pipe()
		defer r.Close()
		go func() {
			_, _ = fmt.Fprintf(w, "TARGET %d\n\n", cmd.Process.Pid)
			_ = w.Close()
		}()
		_, gate := pipePair(t)
		_, hold := pipePair(t)
		_, err := awaitDetachedHandshake(cmd, r, gate, hold, time.Now().Add(longDeadline), 200*time.Millisecond)
		if err == nil {
			t.Fatal("expected error for empty session id")
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("empty id triggered deadline timeout instead of prompt error: %v", err)
		}
	})

	t.Run("invalid_session_id_does_not_wait_out_deadline", func(t *testing.T) {
		cmd := startHelper(t)
		r, w, _ := os.Pipe()
		defer r.Close()
		go func() {
			// Not a valid sess-<alphanumeric> id
			_, _ = fmt.Fprintf(w, "TARGET %d\nnot!a!valid!sess!id\n", cmd.Process.Pid)
			_ = w.Close()
		}()
		_, gate := pipePair(t)
		_, hold := pipePair(t)
		_, err := awaitDetachedHandshake(cmd, r, gate, hold, time.Now().Add(longDeadline), 200*time.Millisecond)
		if err == nil {
			t.Fatal("expected error for invalid session id")
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("invalid id triggered deadline timeout instead of prompt error: %v", err)
		}
	})

	t.Run("oversized_newline_free_stream_rejected_by_cap_not_timeout", func(t *testing.T) {
		// This sub-test is the mutation-resistance probe: the reader must
		// return a frame-too-large error promptly via ReadSlice/ErrBufferFull,
		// NOT by waiting until the pipe closes or the deadline fires.
		//
		// Mutation test: removing the bounded reader (replacing with
		// bufio.ReadString on an unbounded reader) causes this sub-test to
		// block until the 5 s deadline, then return os.ErrDeadlineExceeded,
		// making both the "err == nil" check and the "ErrDeadlineExceeded"
		// check fail together.
		cmd := startHelper(t)
		r, w, _ := os.Pipe()
		defer r.Close()
		go func() {
			_, _ = fmt.Fprintf(w, "TARGET %d\n", cmd.Process.Pid)
			// Write a continuous stream with no newline. The bounded reader must
			// return ErrBufferFull before consuming all of it; an unbounded reader
			// would block here until the deadline fires (5 s below).
			for {
				if _, err := w.Write(bytes.Repeat([]byte("x"), maxHandshakeFrameBytes)); err != nil {
					return
				}
			}
		}()
		_, gate := pipePair(t)
		_, hold := pipePair(t)
		start := time.Now()
		_, err := awaitDetachedHandshake(cmd, r, gate, hold, time.Now().Add(5*time.Second), 200*time.Millisecond)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected error for oversized newline-free stream")
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("frame cap caused 5 s deadline timeout instead of prompt ErrBufferFull: %v", err)
		}
		// The bounded reader returns within one buffer-fill worth of IO,
		// which is microseconds on a local pipe. Any wall time well under
		// the 5 s deadline proves the cap fired, not the deadline.
		if elapsed > time.Second {
			t.Errorf("took %v; bounded frame cap should reject in <<1s", elapsed)
		}
	})
}

// TestAwaitDetachedHandshakeRescanFrameCapBounded is the mutation-specific
// rescan test. It exercises the failure-cleanup rescan path — entered when
// targetPGID is still 0 after the main loop errors — with a newline-free
// oversized stream and verifies the bounded reader caps consumption without
// waiting for the rescan grace deadline.
//
// Mutation to kill: in the rescan, replace readHandshakeFrame(reader) with a
// call that uses a fresh bufio.NewReader(r) (unbounded). That makes the
// rescan block until r’s read deadline (grace = 2 s) fires, raising elapsed
// well above the 500 ms threshold and failing the test.
func TestAwaitDetachedHandshakeRescanFrameCapBounded(t *testing.T) {
	if os.Getenv("GMUX_SUPERVISOR_HELPER") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAwaitDetachedHandshakeRescanFrameCapBounded$")
	cmd.Env = append(os.Environ(), "GMUX_SUPERVISOR_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	// Pre-populate the pipe buffer before calling awaitDetachedHandshake so
	// there is no race between writer and reader goroutines:
	//
	//  (1) An invalid session ID line (no "TARGET" prefix) causes the main
	//      loop to error immediately with targetPGID still 0, which directs
	//      the cleanup path into the rescan branch.
	//
	//  (2) A newline-free blob > maxHandshakeFrameBytes bytes follows. The
	//      bounded ReadSlice reader must return ErrBufferFull promptly; an
	//      unbounded bufio.NewReader reader would block until the 2 s
	//      SetReadDeadline fires.
	_, _ = fmt.Fprintln(w, "not-a-valid-session-id-for-rescan!!!")
	_, _ = w.Write(bytes.Repeat([]byte("R"), 4*maxHandshakeFrameBytes))
	// w is kept open so the pipe does not signal EOF; the rescan reader
	// must rely on ErrBufferFull, not on pipe closure.

	_, gate := pipePair(t)
	_, hold := pipePair(t)

	const grace = 2 * time.Second // rescan SetReadDeadline deadline
	start := time.Now()
	_, err = awaitDetachedHandshake(cmd, r, gate, hold,
		time.Now().Add(5*time.Second), // generous main-loop deadline
		grace)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected protocol error")
	}
	// With the bounded reader the rescan returns as soon as the internal
	// buffer fills (one ReadSlice call, microseconds). The dominant time is
	// subprocess death after SIGTERM, which is typically <50 ms.
	//
	// Mutation threshold: grace/4 = 500 ms. A mutated unbounded rescan
	// blocks for the full grace (2 s), placing elapsed well above 500 ms.
	if elapsed > grace/4 {
		t.Errorf("rescan took %v, want <%v — bounded ReadSlice/ErrBufferFull must "+
			"reject the newline-free stream without waiting the grace deadline",
			elapsed, grace/4)
	}
}
