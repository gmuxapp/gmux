package central

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// A window far longer than any test runtime: passes behind it never fire on
// their own, which makes coalescing/flush assertions deterministic.
const testHugeWindow = time.Hour

// TestWindowQuietPathComposesImmediately: the FIRST dirty signal after start
// (idle) must compose without waiting out the window, even with a huge one.
func TestWindowQuietPathComposesImmediately(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithMinComposeInterval(testHugeWindow))
	startComposer(t, c)

	c.MarkDirty(true, false)
	recvBatch(t, sink.out) // would time out (5s << 1h) if the window applied
	if calls := reader.calls(); len(calls) != 1 {
		t.Fatalf("read passes=%d, want 1", len(calls))
	}
}

// TestWindowQuietPathAfterIdleComposesImmediately: once the window has
// elapsed since the previous pass, the next dirty signal is not delayed.
func TestWindowQuietPathAfterIdleComposesImmediately(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	window := 50 * time.Millisecond
	c := New(reader, nil, sink, WithMinComposeInterval(window))
	startComposer(t, c)

	c.MarkDirty(true, false)
	recvBatch(t, sink.out)
	time.Sleep(3 * window) // go idle past the window

	start := time.Now()
	c.MarkDirty(true, false)
	recvBatch(t, sink.out)
	if elapsed := time.Since(start); elapsed >= window {
		t.Fatalf("quiet-path compose took %v, want < window (%v)", elapsed, window)
	}
}

// TestWindowBurstCoalescesBehindWindow: dirt arriving inside the window
// after a pass must NOT trigger an immediate recompose.
func TestWindowBurstCoalescesBehindWindow(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithMinComposeInterval(testHugeWindow))
	startComposer(t, c)

	c.MarkDirty(true, false)
	recvBatch(t, sink.out)
	for i := 0; i < 50; i++ {
		c.MarkDirty(true, false)
	}
	expectNoBatch(t, sink.out, 100*time.Millisecond)
	if calls := reader.calls(); len(calls) != 1 {
		t.Fatalf("read passes=%d, want 1 (burst held behind window)", len(calls))
	}
}

// TestWindowTrailingEdgeComposesWithMergedDirt: dirt held behind the window
// must compose when the window elapses, merging every kind dirtied while
// the window was open — no lost final state.
func TestWindowTrailingEdgeComposesWithMergedDirt(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithMinComposeInterval(75*time.Millisecond))
	startComposer(t, c)

	c.MarkDirty(true, false)
	first := recvBatch(t, sink.out)
	if first.Sessions == nil || first.Projects != nil {
		t.Fatalf("first batch=%#v", first)
	}
	// Two kinds dirtied inside the window: the trailing pass must carry both.
	c.MarkDirty(true, false)
	c.MarkDirty(false, true)
	second := recvBatch(t, sink.out)
	if second.Sessions == nil || second.Projects == nil {
		t.Fatalf("trailing pass lost merged dirt: %#v", second)
	}
	calls := reader.calls()
	if len(calls) != 2 || !calls[1].IncludeSessions || !calls[1].IncludeProjects {
		t.Fatalf("reads=%#v, want trailing cross-kind read", calls)
	}
	expectNoBatch(t, sink.out, 50*time.Millisecond)
}

// awaitParkedInWindowWait proves the composer is inside sleepInterruptible:
// the previously marked dirt has been TAKEN (both dirty flags clear under
// c.mu), no wake is pending, exactly `reads` store reads have happened, and
// nothing new was emitted — the only remaining place the loop can be is the
// window wait.
func awaitParkedInWindowWait(t *testing.T, c *Composer, reader *fakeReader, reads int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		clean := !c.dirtySessions && !c.dirtyProjects && len(c.wake) == 0
		c.mu.Unlock()
		if clean && len(reader.calls()) == reads {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("composer never parked inside the window wait")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestWindowDirtMarkedDuringWaitMergesIntoTrailingPass: dirt of ANOTHER kind
// marked while the composer is provably parked inside the window wait must
// be merged into the trailing pass (one cross-kind read within one window),
// not deferred to a separate pass a window later. Guards the post-wait
// takeDirty merge in Run.
func TestWindowDirtMarkedDuringWaitMergesIntoTrailingPass(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	window := 300 * time.Millisecond
	c := New(reader, nil, sink, WithMinComposeInterval(window))
	startComposer(t, c)

	c.MarkDirty(true, false)
	first := recvBatch(t, sink.out)
	if first.Sessions == nil || first.Projects != nil {
		t.Fatalf("first batch=%#v", first)
	}

	// Sessions dirt taken immediately after the pass parks the loop in the
	// window wait; prove it before marking the other kind.
	c.MarkDirty(true, false)
	awaitParkedInWindowWait(t, c, reader, 1)
	c.MarkDirty(false, true) // arrives strictly inside the wait

	start := time.Now()
	second := recvBatch(t, sink.out)
	if second.Sessions == nil || second.Projects == nil {
		t.Fatalf("during-wait dirt not merged into trailing pass: %#v", second)
	}
	if elapsed := time.Since(start); elapsed > 2*window {
		t.Fatalf("trailing pass took %v, want within one window (%v)", elapsed, window)
	}
	calls := reader.calls()
	if len(calls) != 2 || !calls[1].IncludeSessions || !calls[1].IncludeProjects {
		t.Fatalf("reads=%#v, want exactly one trailing cross-kind read", calls)
	}
	expectNoBatch(t, sink.out, 50*time.Millisecond)
}

// TestWindowShutdownFlushesPendingDirt: Close during the coalescing wait
// must compose-and-emit the pending dirt before Close returns.
func TestWindowShutdownFlushesPendingDirt(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithMinComposeInterval(testHugeWindow))
	startComposer(t, c)

	c.MarkDirty(true, false)
	recvBatch(t, sink.out)
	c.MarkDirty(false, true) // held behind the 1h window
	expectNoBatch(t, sink.out, 50*time.Millisecond)

	c.Close() // must flush the pending projects pass before returning
	select {
	case b := <-sink.out:
		if b.Projects == nil {
			t.Fatalf("flush batch=%#v, want projects", b)
		}
	default:
		t.Fatal("Close dropped dirt pending behind the window")
	}
}

// TestWindowContextCancelFlushesPendingDirt: production shutdown cancels the
// Run context before Close; the flush must still happen (on its own context)
// and Run must return.
func TestWindowContextCancelFlushesPendingDirt(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithMinComposeInterval(testHugeWindow))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(c.Close)

	c.MarkDirty(true, false)
	recvBatch(t, sink.out)
	c.MarkDirty(true, false)
	expectNoBatch(t, sink.out, 50*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	select {
	case b := <-sink.out:
		if b.Sessions == nil {
			t.Fatalf("flush batch=%#v, want sessions", b)
		}
	default:
		t.Fatal("context cancel dropped dirt pending behind the window")
	}
}

// TestWindowZeroDisablesCoalescing: the default (no option) and an explicit
// non-positive window keep pre-window immediacy.
func TestWindowZeroDisablesCoalescing(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithMinComposeInterval(-1))
	startComposer(t, c)
	for i := 0; i < 3; i++ {
		c.MarkDirty(true, false)
		recvBatch(t, sink.out)
	}
	if calls := reader.calls(); len(calls) != 3 {
		t.Fatalf("read passes=%d, want 3 (window disabled)", len(calls))
	}
}

// TestWindowFlushReadsRuntimeOverlay: the shutdown flush is a real pass, not
// a replay — it must observe the current runtime/store state.
func TestWindowFlushReadsRuntimeOverlay(t *testing.T) {
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{Sessions: []centralstore.SessionView{{Session: centralstore.Session{ID: "s1"}}}}, nil
	}}
	var alive atomic.Bool
	runtime := RuntimeSourceFunc(func() map[centralstore.SessionID]RuntimeFacts {
		if !alive.Load() {
			return nil
		}
		return map[centralstore.SessionID]RuntimeFacts{"s1": {PID: 42}}
	})
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, runtime, sink, WithMinComposeInterval(testHugeWindow))
	startComposer(t, c)

	c.MarkDirty(true, false)
	if b := recvBatch(t, sink.out); b.Sessions.Sessions[0].Alive {
		t.Fatal("first pass should see dead s1")
	}
	alive.Store(true)
	c.MarkDirty(true, false)
	// Let the composer consume the wake and park inside the window wait:
	// the flush guarantee covers dirt held BEHIND the window. (Dirt whose
	// wake was never consumed keeps the pre-existing Close semantics —
	// see TestCloseBeforePendingWakeNeverEmits.)
	expectNoBatch(t, sink.out, 50*time.Millisecond)
	c.Close()
	select {
	case b := <-sink.out:
		if !b.Sessions.Sessions[0].Alive {
			t.Fatal("flush pass replayed stale runtime overlay")
		}
	default:
		t.Fatal("Close dropped the pending pass")
	}
}
