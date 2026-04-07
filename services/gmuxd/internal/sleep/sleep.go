// Package sleep detects system suspend/resume transitions.
//
// Call [NewWatcher] to start monitoring. Subscribe with [Watcher.C] to
// receive a signal after the system wakes from sleep.
//
// The portable implementation ticks every 10s and compares wall-clock
// timestamps. Platform-specific backends (e.g. D-Bus on Linux) can be
// added behind build tags.
package sleep

import (
	"context"
	"log"
	"time"
)

// Watcher monitors for system sleep/wake transitions.
type Watcher struct {
	ch     chan struct{}
	cancel context.CancelFunc
}

// NewWatcher starts a background goroutine that detects system sleep.
// Call Stop when done.
func NewWatcher() *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		ch:     make(chan struct{}, 1),
		cancel: cancel,
	}
	go w.run(ctx)
	return w
}

// C returns a channel that receives a value after each wake from sleep.
// The channel has a buffer of 1; slow consumers won't block the watcher.
func (w *Watcher) C() <-chan struct{} { return w.ch }

// Stop terminates the watcher goroutine.
func (w *Watcher) Stop() { w.cancel() }

func (w *Watcher) run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer close(w.ch)
	for {
		// Strip monotonic reading so Sub() uses wall-clock time.
		// The monotonic clock pauses during suspend on Linux/macOS,
		// so time.Since() on a normal time.Now() would not detect
		// the jump.
		last := time.Now().Round(0)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if elapsed := time.Now().Round(0).Sub(last); elapsed > 30*time.Second {
				log.Printf("sleep: system wake detected (%.0fs gap)", elapsed.Seconds())
				select {
				case w.ch <- struct{}{}:
				default: // don't block if previous signal not consumed
				}
			}
		}
	}
}
