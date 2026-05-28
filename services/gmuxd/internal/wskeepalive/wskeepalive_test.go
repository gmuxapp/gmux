package wskeepalive

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakePinger records ping calls and can either return immediately with a
// scripted error or block until its context is done (to simulate a ping
// that is in flight when cancellation arrives). It signals each ping on
// the buffered `pinged` channel so tests can synchronize deterministically
// instead of sleeping.
type fakePinger struct {
	calls  atomic.Int32
	err    atomic.Pointer[error]
	block  atomic.Bool
	pinged chan struct{}
}

func newFakePinger() *fakePinger {
	return &fakePinger{pinged: make(chan struct{}, 1)}
}

func (f *fakePinger) Ping(ctx context.Context) error {
	f.calls.Add(1)
	select {
	case f.pinged <- struct{}{}: // best-effort signal; never blocks the loop
	default:
	}
	if f.block.Load() {
		<-ctx.Done() // mimic a ping waiting for a pong until cancelled
		return ctx.Err()
	}
	if e := f.err.Load(); e != nil {
		return *e
	}
	return nil
}

func (f *fakePinger) setErr(err error) { f.err.Store(&err) }

// waitForPing blocks until at least one ping has happened, or fails the
// test. Deterministic alternative to sleeping for "some pings to occur".
func (f *fakePinger) waitForPing(t *testing.T) {
	t.Helper()
	select {
	case <-f.pinged:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first ping")
	}
}

func TestRun_PingsUntilContextCancelled(t *testing.T) {
	p := newFakePinger()
	ctx, cancel := context.WithCancel(context.Background())

	var deadCalled atomic.Bool
	done := make(chan struct{})
	go func() {
		Run(ctx, p, time.Millisecond, 50*time.Millisecond, func() { deadCalled.Store(true) })
		close(done)
	}()

	// Deterministically wait for a real ping rather than sleeping.
	p.waitForPing(t)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	if p.calls.Load() == 0 {
		t.Fatal("expected at least one ping while running")
	}
	if deadCalled.Load() {
		t.Fatal("onDead must not fire on a healthy connection")
	}
}

func TestRun_FailedPingTriggersOnDead(t *testing.T) {
	p := newFakePinger()
	p.setErr(errors.New("pong timeout"))

	var deadCalled atomic.Bool
	done := make(chan struct{})
	go func() {
		Run(context.Background(), p, time.Millisecond, 50*time.Millisecond, func() { deadCalled.Store(true) })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after a failed ping")
	}
	if !deadCalled.Load() {
		t.Fatal("onDead should fire when a ping fails on a live context")
	}
}

// TestRun_InFlightPingCancelledIsNotDead exercises the real race the
// shutdown guard protects against: a ping is *in flight* (blocked waiting
// for a pong) when another goroutine cancels the parent context. The ping
// then fails with a context error — but because it's teardown, not a NAT
// drop, onDead must NOT fire.
func TestRun_InFlightPingCancelledIsNotDead(t *testing.T) {
	p := newFakePinger()
	p.block.Store(true) // Ping blocks until its ctx is cancelled

	ctx, cancel := context.WithCancel(context.Background())

	var deadCalled atomic.Bool
	done := make(chan struct{})
	go func() {
		// Generous per-ping timeout so the failure is caused by our
		// cancel below, not by the timeout firing first.
		Run(ctx, p, time.Millisecond, 5*time.Second, func() { deadCalled.Store(true) })
		close(done)
	}()

	// Wait until a ping is actually in flight (blocked), then cancel
	// concurrently so the cancellation interrupts the blocked Ping.
	p.waitForPing(t)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after concurrent cancel of an in-flight ping")
	}
	if deadCalled.Load() {
		t.Fatal("onDead must not fire when an in-flight ping is interrupted by shutdown")
	}
}
