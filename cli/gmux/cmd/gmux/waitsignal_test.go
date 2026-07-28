package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A truncated answer still prints on stdout — dropping the tail would be worse
// than a short one — and the fact that it is short is reported on stderr, where
// the account belongs. Silence here is the failure: a caller would treat a capped
// answer as the whole one.
func TestTruncatedAnswerIsReportedOnStderr(t *testing.T) {
	sess := cliSession{ID: "0123456789abcdef"}
	var stdout bytes.Buffer
	stderr := captureStderr(t, func() {
		if code := reportWaitResult(sess, "gmux wait",
			waitResult{Reason: "idle", Outcome: "completed", Output: "half an answer", Truncated: true},
			false, false, false, &stdout); code != waitExitOK {
			t.Fatalf("exit=%d", code)
		}
	})
	if got := stdout.String(); got != "half an answer\n" {
		t.Fatalf("stdout=%q", got)
	}
	if !strings.Contains(stderr, "truncated") || !strings.Contains(stderr, "gmux agent logs --agent -n 1") {
		t.Fatalf("stderr=%q", stderr)
	}

	// An untruncated answer keeps stderr empty: quiet success is the contract.
	stdout.Reset()
	stderr = captureStderr(t, func() {
		reportWaitResult(sess, "gmux wait",
			waitResult{Reason: "idle", Outcome: "completed", Output: "the answer"},
			false, false, false, &stdout)
	})
	if stderr != "" {
		t.Fatalf("a clean completion wrote to stderr: %q", stderr)
	}
}

// The interrupted-wait notice, driven for real, N times: the helper installs a
// handler, the child signals itself while "waiting", and the child must PRINT the
// notice AND DIE with the shell's 128+N.
//
// The property under test is "the process dies", not "the notice prints" — the
// shipped regression this pins printed the notice perfectly and then left SIGINT
// set to SIG_IGN, so the wait became uninterruptible and no later ^C could end
// it. A single run certified that bug (the good outcome was a timing race), so
// this runs the child repeatedly and, in one variant, delivers a SECOND signal
// after the notice to prove the disposition really is the default again.
func TestInterruptedWaitPrintsTheNoticeAndDies(t *testing.T) {
	switch os.Getenv("GMUX_TEST_WAIT_SIGNAL") {
	case "self":
		// The ordinary shape: blocked "in a wait", signalled once.
		defer noticeInterruptedWait(os.Stderr, "0123456789abcdef")()
		raise(t, syscall.SIGINT)
		block()
	case "term":
		defer noticeInterruptedWait(os.Stderr, "0123456789abcdef")()
		raise(t, syscall.SIGTERM)
		block()
	case "ignored":
		// What a shell does to a background child, before gmux runs.
		signal.Ignore(syscall.SIGINT)
		defer noticeInterruptedWait(os.Stderr, "0123456789abcdef")()
		raise(t, syscall.SIGINT)
		// Report the disposition the re-raise will face, so a green run cannot
		// silently stop reproducing the scenario.
		go func() {
			time.Sleep(100 * time.Millisecond)
			fmt.Fprintf(os.Stderr, "SIGINT-ignored=%v\n", sigIgnored(syscall.SIGINT))
		}()
		block()
	case "twice":
		// Prove the disposition is genuinely restored: the child ignores the
		// FIRST signal's death path by keeping the handler for a moment, then a
		// second signal must still be able to kill it. Both are delivered; the
		// process must not survive them.
		defer noticeInterruptedWait(os.Stderr, "0123456789abcdef")()
		raise(t, syscall.SIGINT)
		time.Sleep(50 * time.Millisecond)
		raise(t, syscall.SIGINT)
		block()
	}

	for _, tc := range []struct {
		mode string
		sig  syscall.Signal
		runs int
	}{
		{"self", syscall.SIGINT, 5},
		{"term", syscall.SIGTERM, 2},
		{"twice", syscall.SIGINT, 3},
	} {
		for i := 0; i < tc.runs; i++ {
			out, code, signaled := runSignalChild(t, tc.mode)
			for _, want := range []string{"only the wait stopped", "the session keeps running", "re-arm with 'gmux wait"} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s run %d: notice missing %q; stderr=%q", tc.mode, i, want, out)
				}
			}
			// Death, however it arrives: killed by the signal (the ordinary
			// path) or the backstop's 128+N exit. Never a survivor.
			if signaled != tc.sig && code != 128+int(tc.sig) {
				t.Fatalf("%s run %d: child exited %d (signal %v), want death by %v / exit %d; stderr=%q",
					tc.mode, i, code, signaled, tc.sig, 128+int(tc.sig), out)
			}
		}
	}
}

// runSignalChild re-execs this test binary in the given self-signalling mode and
// reports its stderr, exit code, and the signal that killed it (0 if none).
func runSignalChild(t *testing.T, mode string) (string, int, syscall.Signal) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestInterruptedWaitPrintsTheNoticeAndDies", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "GMUX_TEST_WAIT_SIGNAL="+mode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("%s: child survived its signal; stderr=%q", mode, stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("%s: run: %v", mode, err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("%s: no wait status", mode)
	}
	if status.Signaled() {
		return stderr.String(), 128 + int(status.Signal()), status.Signal()
	}
	return stderr.String(), status.ExitStatus(), 0
}

// raise delivers a signal to this process, as a terminal's ^C would.
func raise(t *testing.T, sig syscall.Signal) {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		os.Exit(3)
	}
	_ = p.Signal(sig)
}

// block parks the child the way a real blocking wait would. Reaching the end of
// it means the signal was swallowed, so it exits with a code no assertion
// accepts rather than hanging until the test timeout.
func block() {
	time.Sleep(10 * time.Second)
	os.Exit(4)
}

// TestInterruptedWaitNoticeDelegatesToTheDeathPath pins the wiring: on a real
// delivered signal the handler prints the notice and then hands off to the step
// that restores the default disposition and re-raises. Seamed so the assertion
// can be made without the test binary dying.
func TestInterruptedWaitNoticeDelegatesToTheDeathPath(t *testing.T) {
	prev := dieFromSignal
	got := make(chan os.Signal, 1)
	dieFromSignal = func(sig os.Signal) { got <- sig }
	t.Cleanup(func() { dieFromSignal = prev })

	var stderr bytes.Buffer
	stop := noticeInterruptedWait(&stderr, "0123456789abcdef")
	defer stop()
	raise(t, syscall.SIGINT) // caught by the handler's Notify, so nothing dies

	select {
	case sig := <-got:
		if sig != syscall.SIGINT {
			t.Fatalf("death path invoked with %v", sig)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notice handler never reached the death path")
	}
	if !strings.Contains(stderr.String(), "only the wait stopped") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// TestInterruptedWaitDiesEvenWhenTheSignalIsIgnored is the deterministic pin for
// the regression, and it reproduces the review's exact environment: SIGINT
// already set to SIG_IGN when the notice handler is installed, which is what a
// shell does to a background child (`cmd &`, `setsid bash -c '… &'`).
//
// In that state, undoing our own handler restores SIG_IGN — measured below
// against the kernel's own view — so the re-raise is DISCARDED, and a
// re-raise-only implementation prints the notice and then waits forever: the
// shipped bug, "notice, then the ^C is gone and the terminal is stuck".
//
// This is deterministic where a plain death test is not: with SIGINT ignored the
// good outcome cannot come from signal delivery at all, so only the explicit exit
// can produce it. A re-raise-only version fails here every time, while it passes
// an ordinary death test most of the time — which is exactly how the bug shipped
// green.
func TestInterruptedWaitDiesEvenWhenTheSignalIsIgnored(t *testing.T) {
	for i := 0; i < 3; i++ {
		out, code, signaled := runSignalChild(t, "ignored")
		if !strings.Contains(out, "only the wait stopped") {
			t.Fatalf("run %d: notice missing; stderr=%q", i, out)
		}
		if !strings.Contains(out, "SIGINT-ignored=true") {
			t.Fatalf("run %d: the child did not reproduce an ignored SIGINT, so it proves nothing; stderr=%q", i, out)
		}
		if code != 128+int(syscall.SIGINT) {
			t.Fatalf("run %d: child exited %d (signal %v), want %d — an ignored SIGINT must still end the wait",
				i, code, signaled, 128+int(syscall.SIGINT))
		}
	}
}

// sigIgnored reports whether this process currently IGNORES sig, read from the
// kernel's own view (/proc/self/status SigIgn) rather than from anything Go says
// about itself. This is the evidence the review used to catch the shipped bug.
func sigIgnored(sig syscall.Signal) bool {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "SigIgn:") {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "SigIgn:")), 16, 64)
		if err != nil {
			return false
		}
		return mask&(1<<uint(sig-1)) != 0
	}
	return false
}
