package clipfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// pngBytes returns a minimal valid PNG signature plus a few payload bytes.
// Real PNG decoding isn't required; we only verify byte-for-byte storage.
func pngBytes() []byte {
	return []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x02, 0x03}
}

// TestLocalWriter_FirstWriteIsPaste1 is the tracer bullet: a single write
// into an empty tmpdir produces paste-1.png with the exact bytes we passed.
func TestLocalWriter_FirstWriteIsPaste1(t *testing.T) {
	dir := t.TempDir()
	w := NewLocalWriter(dir)

	payload := pngBytes()
	path, err := w.Write(payload, "image/png")
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	wantName := "paste-1.png"
	if filepath.Base(path) != wantName {
		t.Errorf("filename = %q, want %q", filepath.Base(path), wantName)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("dir = %q, want %q", filepath.Dir(path), dir)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path = %q, want absolute", path)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("file contents = %v, want %v", got, payload)
	}
}

// Sanity check on filename predicate so later tests can rely on it without
// duplicating the regex.
func TestPasteFilenameRecognition(t *testing.T) {
	cases := map[string]bool{
		"paste-1.png":      true,
		"paste-42.jpg":     true,
		"paste-9999.webp":  true,
		"paste-1.bin":      true,
		"paste-0.png":      false, // we start at 1
		"paste-.png":       false,
		"paste-1.":         false,
		"paste-1":          false,
		"paste-abc.png":    false,
		"otherpaste-1.png": false,
		"paste-1.png.bak":  false,
	}
	for name, want := range cases {
		if got := isPasteFilename(name); got != want {
			t.Errorf("isPasteFilename(%q) = %v, want %v", name, got, want)
		}
	}
	// Defensive: ensure we don't accidentally match a path that has separators.
	if isPasteFilename(strings.Join([]string{"sub", "paste-1.png"}, string(filepath.Separator))) {
		t.Errorf("isPasteFilename should reject paths with separators")
	}
}

// Sequential writes increment N. Independent of MIME (mixing pngs and
// jpegs still picks the next-free integer regardless of extension).
func TestLocalWriter_SequentialWritesIncrementN(t *testing.T) {
	dir := t.TempDir()
	w := NewLocalWriter(dir)

	cases := []struct {
		mime string
		want string
	}{
		{"image/png", "paste-1.png"},
		{"image/jpeg", "paste-2.jpg"},
		{"image/png", "paste-3.png"},
		{"image/webp", "paste-4.webp"},
		{"application/pdf", "paste-5.pdf"},
	}
	for _, c := range cases {
		path, err := w.Write([]byte("x"), c.mime)
		if err != nil {
			t.Fatalf("Write(%s): %v", c.mime, err)
		}
		if got := filepath.Base(path); got != c.want {
			t.Errorf("Write(%s) = %q, want %q", c.mime, got, c.want)
		}
	}
}

// Unrelated files in the dir don't influence numbering. Subdirectories,
// non-paste files, malformed paste-* names, and paste-0 are all ignored.
func TestLocalWriter_IgnoresUnrelatedEntries(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate with noise that must not affect numbering.
	noise := []string{
		"README.md",
		"paste-0.png",      // 0 isn't a valid N
		"paste-abc.png",    // non-numeric
		"paste-1.png.bak",  // suffix after ext
		"otherpaste-9.png", // wrong prefix
		"paste-1",          // no extension
	}
	for _, n := range noise {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("noise"), 0o644); err != nil {
			t.Fatalf("prep %s: %v", n, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "paste-99.png"), 0o755); err != nil {
		t.Fatalf("prep dir: %v", err)
	}

	w := NewLocalWriter(dir)
	path, err := w.Write([]byte("x"), "image/png")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := filepath.Base(path); got != "paste-1.png" {
		t.Errorf("Write = %q, want paste-1.png", got)
	}
}

// Recognized paste-* files anchor the counter regardless of extension.
func TestLocalWriter_AnchorsOnExistingMaxN(t *testing.T) {
	dir := t.TempDir()
	// Existing paste-3.jpg means next write picks paste-4 even though
	// the new MIME is png.
	if err := os.WriteFile(filepath.Join(dir, "paste-3.jpg"), []byte("old"), 0o644); err != nil {
		t.Fatalf("prep: %v", err)
	}
	w := NewLocalWriter(dir)
	path, err := w.Write([]byte("new"), "image/png")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := filepath.Base(path); got != "paste-4.png" {
		t.Errorf("Write = %q, want paste-4.png", got)
	}
}

// Concurrent writers across many goroutines must produce distinct
// paths and must not overwrite each other. This is the regression net
// for the O_EXCL retry loop.
func TestLocalWriter_ConcurrentWritesAreDistinct(t *testing.T) {
	dir := t.TempDir()
	w := NewLocalWriter(dir)

	const n = 50
	paths := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("payload-%d", i))
			path, err := w.Write(payload, "image/png")
			paths[i] = path
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}

	// All paths must be distinct.
	seen := make(map[string]int, n)
	for i, p := range paths {
		if prev, ok := seen[p]; ok {
			t.Errorf("path collision: i=%d and i=%d both got %q", prev, i, p)
		}
		seen[p] = i
	}

	// Each file must contain the payload of the goroutine that claimed
	// it: no overwrites or torn writes.
	for i, p := range paths {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", p, err)
		}
		want := fmt.Sprintf("payload-%d", i)
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}

	// Numbers should be exactly 1..n with no gaps. Gaps would suggest
	// the retry loop is skipping or the scan is racy in a way we'd want
	// to know about.
	nums := make([]int, 0, n)
	for _, p := range paths {
		nums = append(nums, pasteFilenameN(filepath.Base(p)))
	}
	sort.Ints(nums)
	for i, num := range nums {
		if num != i+1 {
			t.Errorf("after sort, nums[%d] = %d, want %d (full sequence: %v)", i, num, i+1, nums)
			break
		}
	}
}

// MIME-to-extension mapping covers every entry the documentation
// promises and falls back to "bin" for anything else. Tolerates
// case and parameter variations the way Content-Type headers vary
// in the wild.
func TestExtForMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":              "png",
		"image/jpeg":             "jpg",
		"image/jpg":              "jpg", // non-standard but seen in the wild
		"image/gif":              "gif",
		"image/webp":             "webp",
		"image/avif":             "avif",
		"image/heic":             "heic",
		"image/heif":             "heif",
		"image/bmp":              "bmp",
		"image/tiff":             "tiff",
		"image/svg+xml":          "svg",
		"application/pdf":        "pdf",
		"video/mp4":              "mp4",
		"application/zip":        "bin", // truly unknown falls back
		"audio/mpeg":             "bin",
		"":                       "bin",
		"  IMAGE/PNG  ":          "png", // case + whitespace tolerance
		"image/png; charset=foo": "png", // params stripped
	}
	for mime, want := range cases {
		if got := extForMIME(mime); got != want {
			t.Errorf("extForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}
