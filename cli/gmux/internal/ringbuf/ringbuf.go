// Package ringbuf provides a thread-safe circular byte buffer for terminal
// scrollback. Writes always succeed (old data is overwritten). Snapshot
// returns the current contents in order.
package ringbuf

import "sync"

// RingBuf is a fixed-size circular byte buffer.
type RingBuf struct {
	mu   sync.Mutex
	buf  []byte
	size int
	pos  int    // next write position
	full bool   // true once we've wrapped around
}

// New creates a ring buffer with the given capacity in bytes.
func New(size int) *RingBuf {
	return &RingBuf{
		buf:  make([]byte, size),
		size: size,
	}
}

// Write appends data to the ring buffer, overwriting oldest data if full.
func (r *RingBuf) Write(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for len(data) > 0 {
		avail := r.size - r.pos
		n := copy(r.buf[r.pos:r.pos+avail], data)
		r.pos += n
		data = data[n:]
		if r.pos >= r.size {
			r.pos = 0
			r.full = true
		}
	}
}

// Snapshot returns the current buffer contents in chronological order.
// Returns a new slice (safe to use after the call).
func (r *RingBuf) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		// Haven't wrapped yet — just return what we have
		out := make([]byte, r.pos)
		copy(out, r.buf[:r.pos])
		return out
	}

	// Wrapped — oldest data starts at r.pos, newest ends at r.pos-1
	out := make([]byte, r.size)
	n := copy(out, r.buf[r.pos:])
	copy(out[n:], r.buf[:r.pos])
	return out
}

// Len returns the number of bytes currently stored.
func (r *RingBuf) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.full {
		return r.size
	}
	return r.pos
}

// Full returns true if the buffer has wrapped around at least once,
// meaning the snapshot may start at an arbitrary byte boundary.
func (r *RingBuf) Full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.full
}

// Reset clears the buffer.
func (r *RingBuf) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pos = 0
	r.full = false
}
