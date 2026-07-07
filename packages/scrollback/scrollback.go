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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
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

// RenderScrollbackSize is the scrollback ring kept by the virtual
// terminal when replaying raw PTY bytes. Matches the runner's live
// scrollback so a freshly-replayed emulator reaches the same end
// state for any session whose total output stays under the on-disk
// cap (2 * MaxBytes). Past that point both the live grid and a
// replay diverge from "all output ever produced", in the same way.
const RenderScrollbackSize = 2000

// RenderTail replays raw PTY bytes through a fresh terminal emulator
// at (cols, rows) and returns the last n lines of the resulting
// scrollback + visible screen as plain text (no ANSI), one line per
// element. Trailing blank rows of the visible screen are trimmed so an
// idle TUI doesn't pad the output with empty rows.
//
// This is the format `gmux --tail` returns. The runner used to render
// directly from its live cell grid; rendering from disk here produces
// the same shape of output, with the added benefit that dead sessions
// (whose runner is gone) work the same way.
//
// (cols, rows) control line wrapping in the emulator. The emulator
// requires positive dimensions; non-positive values fall back to 80x24,
// which matches the runner's own default for non-interactive launches.
//
// n <= 0 returns nil with no read; callers asking for zero lines get an
// empty answer cheaply. Memory is bounded by the size of r plus the
// emulator's working state; scrollback files are capped at 2 * MaxBytes,
// so this is safe to call against the broker's on-disk reader.
func RenderTail(r io.Reader, cols, rows, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	e := vt.NewEmulator(cols, rows)
	e.SetScrollbackSize(RenderScrollbackSize)
	// The emulator writes back responses (e.g. DSR cursor position
	// reports) into a pipe. Nothing reads them here, and a blocked
	// write would deadlock our Write below. Drain in the background,
	// and close the emulator on the way out so the drain goroutine
	// sees EOF and exits — without the Close it blocks on the pipe
	// forever, leaking one goroutine per render. One-shot tail
	// requests barely noticed; the output-condition wait re-renders
	// on a ticker and would accumulate them.
	drainDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, e)
		close(drainDone)
	}()
	defer func() {
		_ = e.Close()
		<-drainDone
	}()
	if _, err := e.Write(raw); err != nil {
		return nil, fmt.Errorf("replay through emulator: %w", err)
	}

	lines := extractLines(e)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// extractLines reads the scrollback ring followed by the visible
// screen out of e as plain text. Trailing blank rows of the visible
// screen are dropped because an idle TUI pads the bottom of its
// viewport with blanks that aren't useful in a log-style tail.
func extractLines(e *vt.Emulator) []string {
	var lines []string
	if sb := e.Scrollback(); sb != nil {
		for _, line := range sb.Lines() {
			lines = append(lines, plainLine(line))
		}
	}

	w, h := e.Width(), e.Height()
	screenLines := make([]string, h)
	for y := 0; y < h; y++ {
		row := make(uv.Line, w)
		for x := 0; x < w; x++ {
			if c := e.CellAt(x, y); c != nil {
				row[x] = *c
			}
		}
		screenLines[y] = plainLine(row)
	}
	end := len(screenLines)
	for end > 0 && strings.TrimSpace(screenLines[end-1]) == "" {
		end--
	}
	return append(lines, screenLines[:end]...)
}

// plainLine renders a terminal line as plain text (no ANSI styling),
// right-trimming trailing spaces so short lines don't emit padding.
func plainLine(line uv.Line) string {
	var b strings.Builder
	for _, c := range line {
		if c.Content == "" {
			b.WriteString(" ")
		} else {
			b.WriteString(c.Content)
		}
	}
	return strings.TrimRight(b.String(), " ")
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
