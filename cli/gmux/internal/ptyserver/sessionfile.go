package ptyserver

import (
	"os"
	"strings"
	"sync"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
)

// sessionFileReader parses the agent's session file inside the runner, so the
// runner — not the daemon — owns the derived session state (status, title,
// slug, unread). It is driven by agent-shim write events (ADR 0011 phase 1):
// the shim says which file changed, the reader reads that file itself (disk
// stays the loss-proof source of truth) from a tracked offset, and applies
// the adapter's parse results to session.State, which already emits the
// status/meta events the daemon consumes.
//
// Only constructed for adapters that can both parse lines and read session
// metadata; otherwise newSessionFileReader returns nil and the daemon's
// fallback file monitoring still applies.
type sessionFileReader struct {
	state  *session.State
	parser adapter.FileMonitor  // ParseNewLines
	filer  adapter.SessionFiler // ParseSessionFile

	mu     sync.Mutex
	path   string
	offset int64
}

func newSessionFileReader(state *session.State, a adapter.Adapter) *sessionFileReader {
	parser, ok := a.(adapter.FileMonitor)
	if !ok {
		return nil
	}
	filer, ok := a.(adapter.SessionFiler)
	if !ok {
		return nil
	}
	return &sessionFileReader{state: state, parser: parser, filer: filer}
}

// onWrite handles an agent-shim write event for path. A new path is a
// (re)attribution (/resume): the offset resets and the whole file is parsed;
// otherwise only the bytes appended since the last read are parsed.
func (r *sessionFileReader) onWrite(path string) {
	if r == nil || path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	readAll := path != r.path
	if readAll {
		r.path = path
		r.offset = 0
		r.syncMetadata(path)
	}

	lines, n := readNewLines(path, r.offset)
	r.offset += n
	for _, ev := range r.parser.ParseNewLines(lines, path) {
		if ev.Title != "" {
			r.state.SetAdapterTitle(ev.Title)
		}
		if ev.Status != nil {
			r.state.SetStatus(ev.Status)
		}
		// Unread is a delta signal: applying historical lines on the
		// initial read would mark a resumed session unread spuriously.
		if ev.Unread != nil && !readAll {
			r.state.SetUnread(*ev.Unread)
		}
	}
}

// syncMetadata derives slug + title from the file header on (re)attribution.
// Slug comes only from here (ParseNewLines doesn't carry it); title is a
// fallback — ParseNewLines also emits it once the first user message lands.
func (r *sessionFileReader) syncMetadata(path string) {
	info, err := r.filer.ParseSessionFile(path)
	if err != nil || info == nil || info.ID == "" {
		return
	}
	slug := info.Slug
	if slug == "" {
		slug = adapter.Slugify(info.ID)
	}
	r.state.SetSlug(slug)
	if info.Title != "" {
		r.state.SetAdapterTitle(info.Title)
	}
}

// readNewLines reads from offset to EOF and returns the non-empty JSONL lines
// plus the number of bytes consumed. Mirrors the daemon's fallback reader;
// the shim fires after the agent's write completes, so lines are whole.
func readNewLines(path string, offset int64) ([]string, int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= offset {
		return nil, 0
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil, 0
		}
	}
	buf := make([]byte, info.Size()-offset)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return nil, 0
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(buf[:n]), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, int64(n)
}
