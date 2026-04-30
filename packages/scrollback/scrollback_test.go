package scrollback

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writerForTest(t *testing.T) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "deeper", ActiveName)
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, path
}

// readBack returns the bytes a fresh Reader over the given dir
// would emit. Test helper.
func readBack(t *testing.T, dir string) []byte {
	t.Helper()
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return b
}

// TestWriteRoundTrip is the central correctness claim of the
// package: bytes Written are returned by a Reader in the same
// order. Pin everything else against this.
func TestWriteRoundTrip(t *testing.T) {
	w, path := writerForTest(t)

	chunks := [][]byte{
		[]byte("hello "),
		[]byte("world\n"),
		[]byte("\x1b[31mred\x1b[0m\n"),
	}
	var want []byte
	for _, c := range chunks {
		if _, err := w.Write(c); err != nil {
			t.Fatalf("Write: %v", err)
		}
		want = append(want, c...)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := readBack(t, filepath.Dir(path))
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch.\nwant: %q\ngot:  %q", want, got)
	}
}

// TestRotationAtCap verifies the contract that a Write crossing the
// cap rotates first: the previous file holds everything written
// before the rotation, the active file holds the post-rotation
// chunk. Reader concatenation produces the full byte stream.
func TestRotationAtCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ActiveName)
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.max = 100 // shrink for tests; production uses MaxBytes

	pre := bytes.Repeat([]byte("A"), 90)
	post := bytes.Repeat([]byte("B"), 30) // pre+post=120 > 100, triggers rotation

	if _, err := w.Write(pre); err != nil {
		t.Fatalf("Write pre: %v", err)
	}
	if _, err := w.Write(post); err != nil {
		t.Fatalf("Write post: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	prevBytes, err := os.ReadFile(filepath.Join(dir, PreviousName))
	if err != nil {
		t.Fatalf("read previous: %v", err)
	}
	if !bytes.Equal(prevBytes, pre) {
		t.Errorf("previous file: want %q, got %q", pre, prevBytes)
	}
	activeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if !bytes.Equal(activeBytes, post) {
		t.Errorf("active file: want %q, got %q", post, activeBytes)
	}

	if got := readBack(t, dir); !bytes.Equal(got, append(pre, post...)) {
		t.Errorf("reader concatenation: want %q, got %q", append(pre, post...), got)
	}
}

// TestMultipleRotationsKeepOnlyLastTwoSlices is the bound on disk
// usage: across many rotations, only the most recent rotated
// previous + active are kept. Older slices are dropped. Without
// this, a long-running session would accumulate unbounded history.
func TestMultipleRotationsKeepOnlyLastTwoSlices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ActiveName)
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.max = 50

	// Write enough to trigger 3 rotations: A's, then B's, then C's,
	// then D's. After this, prev should hold C's, active should
	// hold D's. A and B should be lost.
	slices := [][]byte{
		bytes.Repeat([]byte("A"), 40),
		bytes.Repeat([]byte("B"), 40), // rotates, prev=A
		bytes.Repeat([]byte("C"), 40), // rotates, prev=B (overwrites A)
		bytes.Repeat([]byte("D"), 40), // rotates, prev=C (overwrites B)
	}
	for _, s := range slices {
		if _, err := w.Write(s); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := readBack(t, dir)
	want := append(slices[2], slices[3]...) // C + D
	if !bytes.Equal(got, want) {
		t.Errorf("after 3 rotations: want %q, got %q", want, got)
	}
}

// TestWriteAfterCloseIsNoOp documents that Close is the terminal
// state; further Writes don't error (matches the best-effort
// contract from the hot path) but they also don't reopen the file.
func TestWriteAfterCloseIsNoOp(t *testing.T) {
	w, path := writerForTest(t)
	if _, err := w.Write([]byte("before")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	n, err := w.Write([]byte("after"))
	if err != nil {
		t.Errorf("Write after Close: want nil err, got %v", err)
	}
	if n != len("after") {
		t.Errorf("Write after Close: want n=%d (best-effort), got %d", len("after"), n)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "before" {
		t.Errorf("file should hold pre-close bytes only: got %q", got)
	}
}

// TestCloseIdempotent: gmuxd, run.go, and ptyserver could each call
// Close on shutdown paths. Idempotence is the contract.
func TestCloseIdempotent(t *testing.T) {
	w, _ := writerForTest(t)
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: want nil, got %v", err)
	}
}

// TestOpenTruncatesExisting locks down the truncate-on-Open
// contract. A re-opened active file starts fresh; the previous
// runner's tail is overwritten. This is the design choice that
// keeps "scrollback is per-runner" honest.
func TestOpenTruncatesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ActiveName)
	if err := os.WriteFile(path, []byte("stale data from previous runner"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := w.Write([]byte("fresh")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("active should hold only post-Open bytes: got %q", got)
	}
}

// TestOpenCreatesParentDir verifies the runner doesn't have to
// create the per-session dir before opening the writer.
// sessionmeta might create it later via Write; the writer is the
// first to need it.
func TestOpenCreatesParentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	path := filepath.Join(dir, ActiveName)
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if mode := info.Mode().Perm(); mode != dirMode {
		t.Errorf("parent dir mode: want %o, got %o", dirMode, mode)
	}
}

// TestFileMode verifies the file ends up owner-only on disk. We
// persist raw terminal output that may include sensitive command
// substitutions, paths, prompts containing API keys, etc.
func TestFileMode(t *testing.T) {
	w, path := writerForTest(t)
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != fileMode {
		t.Errorf("file mode: want %o, got %o", fileMode, mode)
	}
}

// TestWriteIsConcurrencySafe drives parallel writes through the
// Writer to assert the mutex actually serializes them. Failure mode
// without the mutex: torn writes interleave bytes from different
// goroutines, byte counts diverge from input.
func TestWriteIsConcurrencySafe(t *testing.T) {
	w, _ := writerForTest(t)

	const goroutines = 8
	const writesPerGoroutine = 50
	const chunkLen = 64

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		marker := byte('A' + g)
		go func() {
			defer wg.Done()
			chunk := bytes.Repeat([]byte{marker}, chunkLen)
			for i := 0; i < writesPerGoroutine; i++ {
				if _, err := w.Write(chunk); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Total bytes should be exactly goroutines * writesPerGoroutine * chunkLen.
	got := readBack(t, filepath.Dir(w.path))
	want := goroutines * writesPerGoroutine * chunkLen
	if len(got) != want {
		t.Errorf("total bytes: want %d, got %d", want, len(got))
	}
	// And no chunk should have been split: every run of identical
	// bytes should be a multiple of chunkLen. (Technically the
	// scheduler could interleave at chunk boundaries, but never
	// within a single Write call.)
	checkChunkBoundaries(t, got, chunkLen)
}

func checkChunkBoundaries(t *testing.T, got []byte, chunkLen int) {
	t.Helper()
	i := 0
	for i < len(got) {
		c := got[i]
		j := i
		for j < len(got) && got[j] == c {
			j++
		}
		if (j-i)%chunkLen != 0 {
			t.Errorf("torn write at offset %d: %d consecutive %q bytes (not a multiple of %d)",
				i, j-i, c, chunkLen)
			return
		}
		i = j
	}
}

// TestOpenReaderMissing returns the os.ErrNotExist sentinel so
// gmuxd's broker handler can map it to a clean 404.
func TestOpenReaderMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenReader(dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing scrollback: want os.ErrNotExist, got %v", err)
	}
}

// TestOpenReaderActiveOnly: a fresh runner that hasn't rotated yet.
// Reader returns just the active file.
func TestOpenReaderActiveOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ActiveName), []byte("active"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := readBack(t, dir)
	if string(got) != "active" {
		t.Errorf("want %q, got %q", "active", got)
	}
}

// TestOpenReaderPreviousOnly: a runner that rotated and then died
// before writing anything to the new active file. Reader should
// still return previous.
func TestOpenReaderPreviousOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PreviousName), []byte("prev"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := readBack(t, dir)
	if string(got) != "prev" {
		t.Errorf("want %q, got %q", "prev", got)
	}
}

// TestReaderEmitsPreviousBeforeActive nails the chronological
// ordering. Without it, a replayed session would render its
// rotation history out of order and the cursor would land in the
// wrong place.
func TestReaderEmitsPreviousBeforeActive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ActiveName), []byte("LATER"), 0o600); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PreviousName), []byte("EARLIER"), 0o600); err != nil {
		t.Fatalf("seed previous: %v", err)
	}
	got := string(readBack(t, dir))
	if got != "EARLIERLATER" {
		t.Errorf("want %q, got %q", "EARLIERLATER", got)
	}
}

// TestReaderCloseClosesAllUnderlying verifies the multiReadCloser
// Close fans out: leaks here would surface as ENOSPC under heavy
// session churn on machines with low fd limits.
func TestReaderCloseClosesAllUnderlying(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{ActiveName, PreviousName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	r, err := OpenReader(dir)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Second Close must not panic; underlying *os.File returns
	// "file already closed", multiReadCloser surfaces the first.
	err = r.Close()
	if err != nil && !strings.Contains(err.Error(), "already closed") {
		t.Errorf("second Close: want either nil or 'already closed' err, got %v", err)
	}
}
