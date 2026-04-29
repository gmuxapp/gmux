// Package clipfile materializes clipboard binary payloads to files on
// the local filesystem so a TUI running in a gmux session can read them
// by path. Image paste in gmux-web POSTs bytes to the gmuxd that owns
// the session; that gmuxd uses a Writer to land the bytes in its own
// os.TempDir() and returns the absolute path. The path is then typed
// into the PTY as if the user had typed it.
//
// Devcontainer and network-peer sessions are handled by the existing
// peer-forwarding HTTP layer: the request is forwarded to the gmuxd
// running inside the container or on the peer machine, and that gmuxd
// uses its own Writer against its own os.TempDir(). There is no
// container- or peer-aware code in this package.
package clipfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Writer materializes clipboard bytes to a file and returns the
// absolute path the caller should type into the PTY.
type Writer interface {
	Write(payload []byte, mime string) (path string, err error)
}

// LocalWriter writes into a fixed directory (typically os.TempDir()).
// Files are named paste-N.<ext> with N the next free positive integer
// for the recognized paste-* filename pattern in the directory.
// Concurrent writers race-safely via O_CREAT|O_EXCL retry.
type LocalWriter struct {
	dir string
}

// NewLocalWriter returns a Writer rooted at dir.
func NewLocalWriter(dir string) *LocalWriter {
	return &LocalWriter{dir: dir}
}

// pasteFilenameRe matches paste-<positive int>.<non-empty ext> with no
// path separators. Defining this once and reusing it for both
// recognition and parsing avoids inconsistency between scan and
// write.
var pasteFilenameRe = regexp.MustCompile(`^paste-([1-9][0-9]*)\.([A-Za-z0-9]+)$`)

func isPasteFilename(name string) bool {
	return pasteFilenameRe.MatchString(name)
}

// pasteFilenameN returns the integer N from paste-N.<ext>, or 0 if name
// doesn't match the pattern.
func pasteFilenameN(name string) int {
	m := pasteFilenameRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// extForMIME maps a Content-Type to a file extension. Unknown MIMEs
// fall back to "bin"; an empty MIME is treated the same as unknown.
//
// The list covers what a real browser clipboard typically carries:
//   - PNG/JPEG: every screenshot tool's default
//   - HEIC/HEIF: iPhone screenshot syncing through iCloud paste
//   - WebP/AVIF: modern web image copies
//   - GIF/BMP/TIFF: older tooling, Preview's TIFF export
//   - SVG: graphics apps copy as image/svg+xml
//   - PDF: Preview "copy page as PDF"
//   - MP4: short video clips
func extForMIME(mime string) string {
	// Strip parameters (e.g. "image/png; charset=binary").
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(strings.ToLower(mime))
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/avif":
		return "avif"
	case "image/heic":
		return "heic"
	case "image/heif":
		return "heif"
	case "image/bmp":
		return "bmp"
	case "image/tiff":
		return "tiff"
	case "image/svg+xml":
		return "svg"
	case "application/pdf":
		return "pdf"
	case "video/mp4":
		return "mp4"
	default:
		return "bin"
	}
}

// Write materializes payload into dir as paste-N.<ext>, picking N
// atomically via O_CREAT|O_EXCL retry. Returns the absolute path.
func (w *LocalWriter) Write(payload []byte, mime string) (string, error) {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return "", fmt.Errorf("clipfile: ensure dir: %w", err)
	}
	ext := extForMIME(mime)

	start, err := w.nextN()
	if err != nil {
		return "", err
	}

	// Bounded retry: if every candidate up to start+maxRetry collides,
	// something is very wrong (another process spamming pastes faster
	// than we can scan). Surface as an error rather than spinning.
	const maxRetry = 1024
	for i := 0; i < maxRetry; i++ {
		n := start + i
		name := fmt.Sprintf("paste-%d.%s", n, ext)
		path := filepath.Join(w.dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
				continue
			}
			return "", fmt.Errorf("clipfile: create %s: %w", name, err)
		}
		if _, werr := f.Write(payload); werr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("clipfile: write %s: %w", name, werr)
		}
		if cerr := f.Close(); cerr != nil {
			return "", fmt.Errorf("clipfile: close %s: %w", name, cerr)
		}
		abs, aerr := filepath.Abs(path)
		if aerr != nil {
			return "", fmt.Errorf("clipfile: abs %s: %w", name, aerr)
		}
		return abs, nil
	}
	return "", fmt.Errorf("clipfile: too many collisions starting from paste-%d", start)
}

// nextN returns the integer to try first: max recognized N in dir, plus 1.
// Empty dir returns 1.
func (w *LocalWriter) nextN() (int, error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, fmt.Errorf("clipfile: read dir: %w", err)
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n := pasteFilenameN(e.Name()); n > max {
			max = n
		}
	}
	return max + 1, nil
}
