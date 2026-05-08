// Package coalesce provides a typed trailing-edge coalescer that
// batches rapid producer updates into at-most-one delivery per
// window per subscriber, while still letting idle updates through
// immediately.
//
// The shape matches what ADR 0001 calls for: each kind of snapshot
// (sessions, world) gets one Coalescer; subscribers (the SSE
// /v1/events handler, peer mirroring) attach their own subscription
// and receive a "latest only" stream.
//
// Semantics for one subscriber:
//
//   - Push records the new latest value and pokes the subscriber.
//   - If no value has been emitted within `window`, the subscriber's
//     run loop emits immediately (leading-edge while idle).
//   - Otherwise it waits out the rest of the window then emits the
//     freshest value, coalescing every push that arrived in between.
//   - If the subscriber's receive channel is full (buffer 1), the
//     buffered value is dropped and replaced with the freshest one.
//     "Latest wins"; the subscriber never sees stale state.
//
// The coalescer is safe for concurrent use. Subscribers are
// independent: a slow receiver only affects its own delivery rate.
package coalesce

import (
	"sync"
	"time"
)

// Coalescer batches rapid Push() calls into at-most-one trailing-edge
// emission per window per subscriber. The zero value is not usable;
// callers must use New.
type Coalescer[T any] struct {
	window time.Duration

	mu   sync.Mutex
	subs []*sub[T]
}

// New creates a coalescer with the given trailing-edge window. A
// window of zero disables coalescing (every Push emits immediately
// to every subscriber).
func New[T any](window time.Duration) *Coalescer[T] {
	return &Coalescer[T]{window: window}
}

// Push records v as the new latest value and notifies every active
// subscriber. Cheap; safe to call from any goroutine. Push never
// blocks on subscribers.
func (c *Coalescer[T]) Push(v T) {
	c.mu.Lock()
	subs := append([]*sub[T](nil), c.subs...) // snapshot under lock
	c.mu.Unlock()
	for _, s := range subs {
		s.set(v)
	}
}

// Subscribe attaches a fresh subscriber. The returned channel
// receives coalesced values; callers must invoke cancel exactly once
// to detach. After cancel, the channel is closed.
func (c *Coalescer[T]) Subscribe() (<-chan T, func()) {
	s := &sub[T]{
		out:  make(chan T, 1),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	c.mu.Lock()
	c.subs = append(c.subs, s)
	c.mu.Unlock()

	go s.run(c.window)

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(s.done)
			c.mu.Lock()
			for i, x := range c.subs {
				if x == s {
					c.subs = append(c.subs[:i], c.subs[i+1:]...)
					break
				}
			}
			c.mu.Unlock()
		})
	}
	return s.out, cancel
}

// sub is one subscriber. `latest` + `hasNew` form the "latest only"
// buffer; `wake` notifies the run goroutine that latest changed;
// `out` is the receive channel handed to the caller.
type sub[T any] struct {
	mu     sync.Mutex
	latest T
	hasNew bool

	out  chan T
	wake chan struct{}
	done chan struct{}
}

// set records a new latest value and pokes the run goroutine. Never
// blocks: the wake channel is single-slot, and a queued wake is
// equivalent to a fresh one (the goroutine will read latest under
// its own lock).
func (s *sub[T]) set(v T) {
	s.mu.Lock()
	s.latest = v
	s.hasNew = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run is the per-subscriber pump. Single goroutine per subscriber so
// a slow receiver can't stall the producer or other subscribers.
func (s *sub[T]) run(window time.Duration) {
	defer close(s.out)

	var lastEmit time.Time
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}

		// Idle (or zero-window): emit immediately.
		elapsed := time.Since(lastEmit)
		if window <= 0 || elapsed >= window {
			s.emit()
			lastEmit = time.Now()
			continue
		}

		// Trailing-edge wait. Further wakes during the wait don't
		// reset the timer; they just refresh `latest`, which we'll
		// pick up when the timer fires.
		timer := time.NewTimer(window - elapsed)
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		s.emit()
		lastEmit = time.Now()
	}
}

// emit copies the buffered latest value to the output channel,
// dropping any older buffered value first. Returns silently if no
// new value has accumulated since the last emit (defensive; the run
// loop only calls emit after a wake).
func (s *sub[T]) emit() {
	s.mu.Lock()
	if !s.hasNew {
		s.mu.Unlock()
		return
	}
	v := s.latest
	s.hasNew = false
	s.mu.Unlock()

	// Latest-wins: if the previous emit hasn't been consumed yet,
	// drop it and take the newer value.
	select {
	case s.out <- v:
	default:
		select {
		case <-s.out:
		default:
		}
		// Buffer is now free (either we drained or another receiver
		// raced in and drained for us). Try once more; if it still
		// fails the receiver is reading concurrently, which is fine.
		select {
		case s.out <- v:
		default:
		}
	}
}
