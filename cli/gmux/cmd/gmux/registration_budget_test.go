package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(&emptyReader{})}
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestRegistrationRetriesWithinDeadlineAndClassifiesStatuses(t *testing.T) {
	var mu sync.Mutex
	statuses := []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusOK}
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		i := calls
		calls++
		return response(statuses[i]), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := registerWithClient(ctx, client, "s", "/tmp/s", time.Millisecond); got != registerOK {
		t.Fatalf("outcome %v", got)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}

	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusFound, http.StatusNoContent} {
		calls = 0
		client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(status), nil })
		if got := registerWithClient(ctx, client, "s", "/tmp/s", time.Millisecond); got != registerFatal {
			t.Errorf("status %d = %v", status, got)
		}
		if calls != 1 {
			t.Errorf("status %d calls = %d", status, calls)
		}
	}
}

func TestRegistrationCancellationReachesRequestAndBackoff(t *testing.T) {
	seenCanceled := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		close(seenCanceled)
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := registerWithClient(ctx, client, "s", "/tmp/s", time.Second); got != registerUnavailable {
		t.Fatalf("outcome %v", got)
	}
	select {
	case <-seenCanceled:
	default:
		t.Fatal("transport did not observe cancellation")
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("request cancellation was not prompt")
	}

	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("transient") })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel2()
	start = time.Now()
	registerWithClient(ctx2, client, "s", "/tmp/s", time.Second)
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("backoff ignored cancellation")
	}
}

func TestHandshakeDeadlineCaptureAndUnset(t *testing.T) {
	resetCapture(t)
	capturedHandshakeDeadline = time.Time{}
	capturedHandshakeInvalid = false
	_, w := pipePair(t)   // write end → control
	gateR, _ := pipePair(t) // read end → gate (child reads gate token)
	holdR, _ := pipePair(t) // read end → hold (child drains until parent closes)
	deadline := time.Now().Add(time.Second).Truncate(time.Nanosecond)
	t.Setenv(handshakeFDEnv, strconv.Itoa(int(w.Fd())))
	t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
	t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
	t.Setenv(handshakeDeadlineEnv, strconv.FormatInt(deadline.UnixNano(), 10))
	captureHandshakeFD()
	if !capturedHandshakeDeadline.Equal(deadline) || capturedHandshakeInvalid {
		t.Fatalf("deadline = %v invalid=%v", capturedHandshakeDeadline, capturedHandshakeInvalid)
	}
	if os.Getenv(handshakeFDEnv) != "" || os.Getenv(handshakeDeadlineEnv) != "" {
		t.Fatal("internal env leaked")
	}
	ctx, cancel, owned := handshakeContext()
	defer cancel()
	if !owned {
		t.Fatal("handshake not owned")
	}
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("context deadline %v %v", got, ok)
	}
}

func TestHandshakeMalformedAndExpiredDeadlineCancel(t *testing.T) {
	for _, value := range []string{"bad", strconv.FormatInt(time.Now().Add(-time.Second).UnixNano(), 10)} {
		t.Run(value, func(t *testing.T) {
			resetCapture(t)
			capturedHandshakeDeadline = time.Time{}
			capturedHandshakeInvalid = false
			_, w := pipePair(t)      // write end → control
			gateR, _ := pipePair(t) // read end → gate
			holdR, _ := pipePair(t) // read end → hold
			t.Setenv(handshakeFDEnv, strconv.Itoa(int(w.Fd())))
			t.Setenv(handshakeGateFDEnv, strconv.Itoa(int(gateR.Fd())))
			t.Setenv(handshakeHoldFDEnv, strconv.Itoa(int(holdR.Fd())))
			t.Setenv(handshakeDeadlineEnv, value)
			captureHandshakeFD()
			ctx, cancel, owned := handshakeContext()
			defer cancel()
			if !owned || ctx.Err() == nil {
				t.Fatal("invalid deadline did not cancel owned launch")
			}
			if os.Getenv(handshakeDeadlineEnv) != "" {
				t.Fatal("deadline env leaked")
			}
		})
	}
}

func TestAwaitDetachedHandshakeOwnsProcessUntilResult(t *testing.T) {
	if os.Getenv("GMUX_SUPERVISOR_HELPER") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}
	start := func() (*exec.Cmd, *os.File, *os.File) {
		t.Helper()
		r, w, _ := os.Pipe()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAwaitDetachedHandshakeOwnsProcessUntilResult$")
		cmd.Env = append(os.Environ(), "GMUX_SUPERVISOR_HELPER=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, r, w
	}
	t.Run("timeout terminates and reaps", func(t *testing.T) {
		cmd, r, w := start()
		defer r.Close()
		defer w.Close()
		_, gate := pipePair(t)
		_, hold := pipePair(t)
		_, err := awaitDetachedHandshake(cmd, r, gate, hold, time.Now().Add(30*time.Millisecond), 100*time.Millisecond)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err %v", err)
		}
		if cmd.ProcessState == nil {
			t.Fatal("child not reaped")
		}
		if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("child still live: %v", err)
		}
	})
	for _, tc := range []struct {
		name  string
		write string
	}{
		{"eof terminates and reaps", ""},
		{"partial ack near deadline terminates and reaps", "sess-accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, r, w := start()
			if tc.write != "" {
				_, _ = io.WriteString(w, tc.write)
			}
			_ = w.Close()
			_, gate := pipePair(t)
			_, hold := pipePair(t)
			_, err := awaitDetachedHandshake(cmd, r, gate, hold, time.Now().Add(30*time.Millisecond), 100*time.Millisecond)
			_ = r.Close()
			if err == nil || cmd.ProcessState == nil {
				t.Fatalf("err=%v reaped=%v", err, cmd.ProcessState != nil)
			}
			if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("child still live: %v", err)
			}
		})
	}
	t.Run("success releases", func(t *testing.T) {
		cmd, r, w := start()
		pid := cmd.Process.Pid
		defer r.Close()
		go func() { _, _ = io.WriteString(w, fmt.Sprintf("TARGET %d\nsess-ok\n", pid)); _ = w.Close() }()
		_, gate := pipePair(t)
		_, hold := pipePair(t)
		id, err := awaitDetachedHandshake(cmd, r, gate, hold, time.Now().Add(time.Second), 100*time.Millisecond)
		if err != nil || id != "sess-ok" {
			t.Fatalf("id=%q err=%v", id, err)
		}
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("successful child not alive: %v", err)
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("success waiter did not reap fast exit")
	})
}

// ── Drain-channel bounded wait (seam tests) ──

// TestDrainErrorChanIsTimeBounded verifies that drainErrorChan returns within
// the grace period even when the done channel never closes. This is the
// deterministic seam test for the post-SIGKILL bounded-wait invariant:
// a real process in uninterruptible kernel sleep can hold up SIGKILL
// indefinitely, so the parent must not block forever.
func TestDrainErrorChanIsTimeBounded(t *testing.T) {
	done := make(chan error) // never closes
	grace := 40 * time.Millisecond
	start := time.Now()
	drainErrorChan(done, grace)
	if elapsed := time.Since(start); elapsed > grace*5 {
		t.Errorf("drainErrorChan blocked %v, want ≤%v", elapsed, grace*5)
	}
}

func TestDrainErrorChanReturnsFastWhenClosed(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	// External-timeout approach: a goroutine runs the drain and closes
	// returned when done. time.After provides the hard bound rather than
	// a wall-clock elapsed check, which is robust to CI scheduler jitter
	// while still catching a mutation that blocks on an unconditional receive.
	returned := make(chan struct{})
	go func() { drainErrorChan(done, time.Second); close(returned) }()
	select {
	case <-returned:
	case <-time.After(250 * time.Millisecond):
		t.Error("drainErrorChan did not return promptly with pre-closed channel (want <<250ms)")
	}
}

func TestDrainStructChanIsTimeBounded(t *testing.T) {
	done := make(chan struct{}) // never closes
	grace := 40 * time.Millisecond
	start := time.Now()
	drainStructChan(done, grace)
	if elapsed := time.Since(start); elapsed > grace*5 {
		t.Errorf("drainStructChan blocked %v, want ≤%v", elapsed, grace*5)
	}
}

func TestDrainStructChanReturnsFastWhenClosed(t *testing.T) {
	done := make(chan struct{})
	close(done)
	returned := make(chan struct{})
	go func() { drainStructChan(done, time.Second); close(returned) }()
	select {
	case <-returned:
	case <-time.After(250 * time.Millisecond):
		t.Error("drainStructChan did not return promptly with pre-closed channel (want <<250ms)")
	}
}

// ── F2: foreground registration budget ──

// TestForegroundRegistrationBudgetIsShorterThanDetached pins the deliberate
// budget split: foreground/nested launches must use a shorter best-effort
// window than explicit -d so a daemon-unavailable scenario doesn't produce
// a visible 30 s hang at the shell.
func TestForegroundRegistrationBudgetIsShorterThanDetached(t *testing.T) {
	if foregroundRegistrationBudget >= detachedStartupBudget {
		t.Fatalf("foregroundRegistrationBudget (%v) must be shorter than detachedStartupBudget (%v)",
			foregroundRegistrationBudget, detachedStartupBudget)
	}
}

// TestForegroundRegistrationCompletesQuicklyWhenDaemonUnavailable verifies
// that registerWithGmuxd returns within foregroundRegistrationBudget when the
// daemon socket does not exist. This is the no-daemon fast path that the
// budget split makes user-visible: with the old 30 s budget, a foreground
// `gmux -- true` would hang for 30 s; with the 3 s budget it exits promptly.
func TestForegroundRegistrationCompletesQuicklyWhenDaemonUnavailable(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome) // no daemon socket under this dir

	ctx, cancel := context.WithTimeout(context.Background(), foregroundRegistrationBudget)
	defer cancel()
	start := time.Now()
	got := registerWithGmuxd(ctx, "sess-fg-budget-test", "/tmp/fg-budget-test.sock")
	elapsed := time.Since(start)

	if got != registerUnavailable {
		t.Errorf("expected registerUnavailable with no daemon, got %v", got)
	}
	// Must finish within the documented budget plus one backoff tick (500 ms).
	// The budget is 3 s; anything substantially over that would indicate the
	// code reverted to the 30 s detachedStartupBudget.
	if elapsed > foregroundRegistrationBudget+500*time.Millisecond {
		t.Errorf("registration took %v, want ≤ %v (foregroundRegistrationBudget + 500ms slack)",
			elapsed, foregroundRegistrationBudget+500*time.Millisecond)
	}
}
