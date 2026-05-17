package coalesce

import (
	"sync"
	"testing"
	"time"
)

// recv waits up to timeout for a value on ch. Returns (value, true)
// on success, (zero, false) on timeout. Test helper, not exported.
func recv[T any](t *testing.T, ch <-chan T, timeout time.Duration) (T, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			var zero T
			return zero, false
		}
		return v, true
	case <-time.After(timeout):
		var zero T
		return zero, false
	}
}

// expectNothing asserts that no value arrives within `quiet`.
func expectNothing[T any](t *testing.T, ch <-chan T, quiet time.Duration) {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed unexpectedly during quiet window")
		}
		t.Fatalf("expected no value within %v, got %v", quiet, v)
	case <-time.After(quiet):
	}
}

func TestIdlePushDeliversImmediately(t *testing.T) {
	c := New[int](50 * time.Millisecond)
	ch, cancel := c.Subscribe()
	defer cancel()

	start := time.Now()
	c.Push(7)
	v, ok := recv(t, ch, 25*time.Millisecond)
	if !ok {
		t.Fatalf("expected immediate emit, got timeout")
	}
	if v != 7 {
		t.Fatalf("expected 7, got %v", v)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Fatalf("idle emit took %v, expected <25ms", elapsed)
	}
}

func TestBurstCoalescesToTrailingLatest(t *testing.T) {
	c := New[int](40 * time.Millisecond)
	ch, cancel := c.Subscribe()
	defer cancel()

	// First push: leading-edge, immediate.
	c.Push(1)
	v, ok := recv(t, ch, 25*time.Millisecond)
	if !ok || v != 1 {
		t.Fatalf("leading-edge emit: got %v ok=%v, want 1 true", v, ok)
	}

	// Burst three more pushes within the window.
	c.Push(2)
	c.Push(3)
	c.Push(4)

	// Nothing should arrive sooner than ~window after the leading emit.
	expectNothing(t, ch, 20*time.Millisecond)

	// Trailing-edge: a single emission with the latest value (4),
	// not 2 or 3.
	v, ok = recv(t, ch, 100*time.Millisecond)
	if !ok {
		t.Fatalf("trailing-edge emit: timeout")
	}
	if v != 4 {
		t.Fatalf("expected coalesced trailing emit of latest=4, got %v", v)
	}

	// And no further emit afterwards.
	expectNothing(t, ch, 60*time.Millisecond)
}

func TestSlowReceiverGetsLatestOnly(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	ch, cancel := c.Subscribe()
	defer cancel()

	// Push, don't drain. Run loop emits 1 into the buffer.
	c.Push(1)
	// Wait for the goroutine to actually buffer the value.
	time.Sleep(20 * time.Millisecond)

	// More pushes while the receiver is still slow. Each trailing-
	// edge emit overwrites the buffered value.
	for i := 2; i <= 10; i++ {
		c.Push(i)
		time.Sleep(15 * time.Millisecond)
	}

	// Now drain. We should see exactly the latest pushed value (10),
	// not stale entries.
	v, ok := recv(t, ch, 50*time.Millisecond)
	if !ok {
		t.Fatalf("expected a buffered value, got timeout")
	}
	if v != 10 {
		t.Fatalf("expected latest-wins buffer to hold 10, got %v", v)
	}
}

func TestCancelClosesChannel(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	ch, cancel := c.Subscribe()

	cancel()

	// Channel should close promptly.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected closed channel, got value")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("cancel did not close the channel")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	_, cancel := c.Subscribe()
	cancel()
	cancel() // must not panic
}

func TestPushAfterCancelDoesNotPanic(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	_, cancel := c.Subscribe()
	cancel()
	// Goroutine has exited; Push must not panic on a removed sub.
	c.Push(1)
	c.Push(2)
}

func TestMultipleSubscribersReceiveIndependently(t *testing.T) {
	c := New[int](20 * time.Millisecond)
	ch1, cancel1 := c.Subscribe()
	defer cancel1()
	ch2, cancel2 := c.Subscribe()
	defer cancel2()

	c.Push(42)

	v1, ok1 := recv(t, ch1, 50*time.Millisecond)
	v2, ok2 := recv(t, ch2, 50*time.Millisecond)
	if !ok1 || !ok2 {
		t.Fatalf("expected both subscribers to receive: ok1=%v ok2=%v", ok1, ok2)
	}
	if v1 != 42 || v2 != 42 {
		t.Fatalf("expected both=42, got v1=%v v2=%v", v1, v2)
	}
}

func TestSlowSubscriberDoesNotBlockFastOne(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	slow, cancelSlow := c.Subscribe()
	defer cancelSlow()
	fast, cancelFast := c.Subscribe()
	defer cancelFast()

	// Don't read from slow. Fast should still get every coalesced emit.
	_ = slow

	for i := 1; i <= 5; i++ {
		c.Push(i)
		v, ok := recv(t, fast, 80*time.Millisecond)
		if !ok {
			t.Fatalf("push %d: fast subscriber timed out", i)
		}
		if v != i {
			t.Fatalf("push %d: fast got %v, expected %v", i, v, i)
		}
	}
}

func TestZeroWindowEmitsEveryPushImmediately(t *testing.T) {
	c := New[int](0)
	ch, cancel := c.Subscribe()
	defer cancel()

	// With window=0 we want pass-through. Drain concurrently so the
	// latest-wins buffer doesn't swallow values.
	got := make([]int, 0, 5)
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			v, ok := recv(t, ch, 100*time.Millisecond)
			if !ok {
				return
			}
			mu.Lock()
			got = append(got, v)
			mu.Unlock()
		}
	}()

	for i := 1; i <= 5; i++ {
		c.Push(i)
		time.Sleep(20 * time.Millisecond) // give the receiver time to drain
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 5 {
		t.Fatalf("zero-window: expected 5 emits, got %d (%v)", len(got), got)
	}
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("zero-window: emit %d = %v, want %v", i, v, i+1)
		}
	}
}

func TestNoPushNoEmit(t *testing.T) {
	c := New[int](10 * time.Millisecond)
	ch, cancel := c.Subscribe()
	defer cancel()

	// Without any Push, the channel must stay quiet.
	expectNothing(t, ch, 50*time.Millisecond)
}

func TestPushAfterLongIdleEmitsImmediately(t *testing.T) {
	c := New[int](20 * time.Millisecond)
	ch, cancel := c.Subscribe()
	defer cancel()

	c.Push(1)
	if v, ok := recv(t, ch, 30*time.Millisecond); !ok || v != 1 {
		t.Fatalf("first push: got %v ok=%v", v, ok)
	}

	// Long quiet period.
	time.Sleep(80 * time.Millisecond)

	// Next push should be treated as idle → leading-edge, immediate.
	start := time.Now()
	c.Push(2)
	v, ok := recv(t, ch, 15*time.Millisecond)
	if !ok || v != 2 {
		t.Fatalf("post-idle push: got %v ok=%v after %v", v, ok, time.Since(start))
	}
}
