// Package scrollback persists a session's raw PTY output stream so a
// dead session can still serve its terminal history.
//
// File layout under the per-session state dir
// ($XDG_STATE_HOME/gmux/sessions/<id>/):
//
//	scrollback     append-only active file, capped at MaxBytes
//	scrollback.0   previous active file (rotated; overwritten on each rotation)
//
// When a Write would push the active file past MaxBytes, the active
// file is renamed onto scrollback.0 (atomically, on the same fs) and
// a fresh active file is opened. Total on-disk usage per session is
// therefore bounded at 2 * MaxBytes.
//
// Format: raw PTY bytes, exactly as the child process emitted them.
// No framing, no encoding. xterm.js consumes them by feeding chunks
// straight to Terminal.write().
//
// This package is the single source of truth for the on-disk
// contract: the runner imports the Writer half, gmuxd imports the
// Reader half.
package scrollback

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// ActiveName is the basename of the active scrollback file, the
	// one currently being appended to.
	ActiveName = "scrollback"

	// PreviousName is the basename of the rotated previous file. It
	// is overwritten on each rotation; only the most recent rotation
	// is preserved.
	PreviousName = "scrollback.0"

	// MaxBytes is the soft cap at which the active file is rotated.
	// 1 MiB comfortably exceeds xterm's default 5000-line buffer
	// (~400 KiB at 80 cols) so Resume sessions get a full replay,
	// while keeping per-session disk usage bounded at 2 MiB.
	MaxBytes int64 = 1 << 20

	// dirMode and fileMode mirror sessionmeta's modes so the
	// per-session directory stays owner-only.
	dirMode  = 0o700
	fileMode = 0o600
)

// Writer appends raw PTY bytes to the active scrollback file with
// size-based rotation. Safe for concurrent calls; rotation is atomic
// (rename(2)) and never loses data already written to the rotated
// file.
//
// IO failures are sticky: once any write or rotation errors out, the
// Writer enters a failed state and subsequent Writes are silently
// dropped (returning len(p), nil so callers don't have to special-case
// scrollback failures in their hot path). The first error is
// preserved and surfaced by Close.
type Writer struct {
	mu      sync.Mutex
	path    string
	max     int64
	f       *os.File
	written int64
	failErr error
	closed  bool
}

// Open creates or opens the active scrollback file at path. The
// parent directory is created with mode 0o700 if missing.
//
// Both the active file and any rotated previous file are cleared:
// scrollback's unit is the runner, not the session. A resumed
// session is a fresh runner with empty in-memory vt state, so its
// persisted scrollback starts fresh too. Leaving the rotated
// previous file in place would let it splice into the new run's
// readback through OpenReader (which concatenates previous +
// active), producing a confusing mix of old and new bytes after a
// resume that crossed the rotation boundary. The persisted history
// of the previous (dead) runner is therefore overwritten on
// resume; users who want to keep that history should peek before
// resuming.
func Open(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("scrollback: mkdir %s: %w", filepath.Dir(path), err)
	}
	// Drop any rotated previous file from a prior runner; see doc
	// comment. Best-effort: if the unlink fails for any reason
	// other than not-exist, the active-file truncate below still
	// produces a working writer, just with stale rotated bytes
	// lingering on disk — not a correctness regression vs. before
	// this fix.
	prev := filepath.Join(filepath.Dir(path), PreviousName)
	if err := os.Remove(prev); err != nil && !os.IsNotExist(err) {
		// Logged at debug-level by callers if needed; not fatal.
		_ = err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return nil, fmt.Errorf("scrollback: open %s: %w", path, err)
	}
	return &Writer{path: path, max: MaxBytes, f: f}, nil
}

// Write appends p to the active file, rotating first if the write
// would exceed the cap. Returns len(p), nil even on IO failure: the
// scrollback is best-effort, and callers in the PTY hot path
// shouldn't have to handle disk-full as a fatal condition. Use
// Close's return value to detect persisted IO errors.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed || w.failErr != nil || len(p) == 0 {
		return len(p), nil
	}

	if w.written+int64(len(p)) > w.max {
		if err := w.rotateLocked(); err != nil {
			w.failErr = err
			return len(p), nil
		}
	}

	n, err := w.f.Write(p)
	w.written += int64(n)
	if err != nil {
		w.failErr = fmt.Errorf("scrollback: write: %w", err)
	}
	return len(p), nil
}

// rotateLocked closes the active file, renames it onto PreviousName
// (atomic on the same filesystem), and opens a fresh active file.
// Caller holds w.mu.
func (w *Writer) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("scrollback: close before rotate: %w", err)
	}
	prev := filepath.Join(filepath.Dir(w.path), PreviousName)
	if err := os.Rename(w.path, prev); err != nil {
		return fmt.Errorf("scrollback: rotate %s -> %s: %w", w.path, prev, err)
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("scrollback: reopen %s: %w", w.path, err)
	}
	w.f = f
	w.written = 0
	return nil
}

// Close flushes and closes the active file. Idempotent. Returns the
// first IO error observed across all writes/rotations, or nil if
// every operation succeeded.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return w.failErr
	}
	w.closed = true
	if err := w.f.Close(); err != nil && w.failErr == nil {
		w.failErr = fmt.Errorf("scrollback: close: %w", err)
	}
	w.f = nil
	return w.failErr
}

// OpenReader returns an io.ReadCloser over the persisted scrollback
// for the session whose per-session dir is dir. The reader emits the
// previous file (if it exists) followed by the active file (if it
// exists) so bytes appear in chronological order.
//
// Returns os.ErrNotExist when neither file is present.
//
// Concurrency caveat: a Writer rotating between this function's
// open(prev) and open(active) syscalls can leave the reader with a
// stale prev fd plus a fresh-empty active fd, missing the chunk
// that just rotated into the new prev. The race window is small
// (one rename(2) call) and the loss is bounded at MaxBytes.
// Scrollback is best-effort by design; callers that need exact
// snapshots should coordinate with the writer out-of-band.
func OpenReader(dir string) (io.ReadCloser, error) {
	prev := filepath.Join(dir, PreviousName)
	active := filepath.Join(dir, ActiveName)

	var files []*os.File
	for _, p := range []string{prev, active} {
		f, err := os.Open(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Close anything already opened before bailing.
			for _, opened := range files {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("scrollback: open %s: %w", p, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, os.ErrNotExist
	}

	readers := make([]io.Reader, len(files))
	closers := make([]io.Closer, len(files))
	for i, f := range files {
		readers[i] = f
		closers[i] = f
	}
	return &multiReadCloser{r: io.MultiReader(readers...), closers: closers}, nil
}

// TailBytes reads all bytes from r and returns the trailing portion
// containing at most n lines (newline-terminated, or the file's
// trailing chunk if the last line is unterminated).
//
// Semantics:
//
//   - n <= 0 returns nil with no read; callers asking for zero lines
//     get an empty answer cheaply.
//   - A line is delimited by '\n'. A trailing chunk after the final
//     '\n' counts as a (partial) line; this matches what users
//     intuitively call "the last line" when a session's output
//     stops mid-write.
//   - '\r' is preserved inline; the on-disk scrollback uses CRLF and
//     we don't second-guess what the PTY emitted.
//
// Memory: bounded by the size of r. Scrollback files are capped at
// 2 * MaxBytes, so this is safe for use by the gmuxd broker. Don't
// hand it an unbounded stream.
func TailBytes(r io.Reader, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// SplitAfter keeps the '\n' attached to each line. A file ending
	// in '\n' produces a trailing empty element; drop it so a request
	// for the last N lines doesn't burn one of N on emptiness.
	lines := bytes.SplitAfter(buf, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return bytes.Join(lines, nil), nil
}

type multiReadCloser struct {
	r       io.Reader
	closers []io.Closer
}

func (m *multiReadCloser) Read(p []byte) (int, error) { return m.r.Read(p) }

func (m *multiReadCloser) Close() error {
	var first error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
