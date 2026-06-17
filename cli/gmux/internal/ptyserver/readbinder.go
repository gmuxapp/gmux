package ptyserver

import (
	"sync"
	"time"
)

// readBinder turns the shim's raw session-file *read* events into an
// attribution signal, disambiguating a bind from a bulk scan by access
// pattern rather than by which fs method produced the read (ADR 0011).
//
// A read means "the agent opened/loaded this conversation file." Two very
// different things do that:
//
//   - Bind: /resume-select or a `--session X` launch reads exactly ONE file,
//     then works with it. We want to attribute instantly, before the first
//     write.
//   - Scan: the /resume picker lists conversations by reading MANY files in
//     a quick burst. Attributing here would churn to whichever was read last.
//
// Method can't tell them apart (a harness may use readFile for both), so we
// key on the pattern: collect reads in a short window; if exactly one
// distinct file was read, it's a bind; if several, it's a scan and we ignore
// it. Writes bypass this entirely — they're authoritative.
type readBinder struct {
	window time.Duration
	onBind func(path string)

	mu      sync.Mutex
	pending map[string]struct{}
	timer   *time.Timer
}

func newReadBinder(window time.Duration, onBind func(path string)) *readBinder {
	return &readBinder{
		window:  window,
		onBind:  onBind,
		pending: make(map[string]struct{}),
	}
}

// observe records a read of path and (re)arms the window. The decision is
// made once reads go quiet for window.
func (b *readBinder) observe(path string) {
	if b == nil || path == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[path] = struct{}{}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.window, b.flush)
}

func (b *readBinder) flush() {
	b.mu.Lock()
	paths := b.pending
	b.pending = make(map[string]struct{})
	b.mu.Unlock()

	// Exactly one distinct file in the window → a bind. More than one → a
	// scan (picker); ignore it and wait for a write to settle attribution.
	if len(paths) != 1 {
		return
	}
	var only string
	for p := range paths {
		only = p
	}
	b.onBind(only)
}

func (b *readBinder) stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
}
