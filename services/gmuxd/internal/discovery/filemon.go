// Package discovery — file monitor component.
//
// FileMonitor watches every adapter's session-file root tree with inotify
// (via fsnotify) and feeds .jsonl create/write/remove events to the
// conversations index. It is purely a discovery feeder: live session state
// (title, status, slug, attribution) is reported authoritatively by each
// adapter's agent hook over the runner socket (ADR 0011/0013), so the daemon
// no longer parses session files or attributes them here.
//
// Watching strategy:
//   - Each adapter's SessionRootDir() is watched, plus every subdirectory
//     under it (codex nests YYYY/MM/DD), so new session files are seen
//     wherever a tool writes them.
//   - New subdirectories are watched as they're created, with a catch-up
//     scan to close the `mkdir x && touch x/y.jsonl` race.
package discovery

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
)

// FileMonitor watches adapter session-file roots and feeds the conversations
// index. It holds no per-session state.
type FileMonitor struct {
	watcher *fsnotify.Watcher
	index   *conversations.Index // optional; nil in unit tests

	// rootToAdapter maps each adapter's SessionRootDir() to its adapter.
	// Built once at construction; read-only after NewFileMonitor returns.
	rootToAdapter map[string]adapter.Adapter

	mu          sync.Mutex
	watchedDirs map[string]bool // all dirs currently watched (roots + subdirs)
	rootDirs    map[string]bool // session root dirs being watched
}

// NewFileMonitor returns a monitor watching every file-backed adapter's root.
func NewFileMonitor() *FileMonitor {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("filemon: failed to create watcher: %v", err)
	}
	rootToAdapter := make(map[string]adapter.Adapter)
	for _, a := range adapters.AllAdapters() {
		sf, ok := a.(adapter.SessionFiler)
		if !ok {
			continue
		}
		if root := sf.SessionRootDir(); root != "" {
			rootToAdapter[root] = a
		}
	}
	return &FileMonitor{
		watcher:       w,
		rootToAdapter: rootToAdapter,
		watchedDirs:   make(map[string]bool),
		rootDirs:      make(map[string]bool),
	}
}

// SetConvIndex wires the conversations index to receive ScanFile and
// RemoveByPath calls on .jsonl events. Must be called before Run starts; not
// safe to swap concurrently. Tests that don't exercise the index can leave it
// unset (calls become no-ops).
func (fm *FileMonitor) SetConvIndex(ix *conversations.Index) {
	fm.index = ix
}

// WatchRoots installs always-on fsnotify watches for every adapter
// SessionRootDir() and every existing subdirectory under it. Walks the tree
// depth-first so codex's date-nested layout (YYYY/MM/DD) is fully covered.
// Idempotent and safe to call once at gmuxd startup before Run begins.
//
// We mkdir any missing root because fsnotify can only watch existing
// directories, and we want to detect when a user starts using an adapter
// mid-session without forcing a daemon restart. Side effect: gmuxd creates an
// empty `~/.pi/agent/sessions/` (etc.) for every configured adapter, even ones
// the user has never used. Acceptable in exchange for not requiring a restart
// on first use.
//
// New subdirectories created later are picked up by handleFSEvent's Create
// handler, which adds a watch on any subdir created under an already-watched
// dir.
func (fm *FileMonitor) WatchRoots() {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for root := range fm.rootToAdapter {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			if err := os.MkdirAll(root, 0o755); err != nil {
				log.Printf("filemon: mkdir %s: %v", root, err)
				continue
			}
		}
		fm.ensureRootWatchLocked(root)
		fm.watchTreeLocked(root)
	}
}

// watchTreeLocked walks the directory tree under root and adds a watch on
// every subdirectory. Errors on individual entries are logged but don't abort
// the walk.
func (fm *FileMonitor) watchTreeLocked(root string) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() {
			return nil
		}
		if path == root {
			return nil // already watched as root
		}
		fm.addWatchLocked(path)
		return nil
	})
}

// adapterForPath returns the adapter responsible for path by matching its
// directory against known SessionRootDir prefixes. Returns nil if no adapter
// claims the path.
func (fm *FileMonitor) adapterForPath(path string) adapter.Adapter {
	dir := filepath.Dir(path)
	for root, a := range fm.rootToAdapter {
		if isUnderRoot(dir, root) {
			return a
		}
	}
	return nil
}

// notifyConvIndex dispatches a .jsonl filesystem event to the conversations
// index. No-op if the index isn't wired (test mode).
func (fm *FileMonitor) notifyConvIndex(event fsnotify.Event) {
	if fm.index == nil {
		return
	}
	if !strings.HasSuffix(event.Name, ".jsonl") {
		return
	}
	switch {
	case event.Has(fsnotify.Remove), event.Has(fsnotify.Rename):
		fm.index.RemoveByPath(event.Name)
	case event.Has(fsnotify.Create), event.Has(fsnotify.Write):
		a := fm.adapterForPath(event.Name)
		if a == nil {
			return
		}
		fm.index.ScanFile(a, event.Name)
	}
}

// Run processes inotify events until stop is closed, feeding the conversations
// index. No network or per-session work happens here.
func (fm *FileMonitor) Run(stop <-chan struct{}) {
	if fm.watcher == nil {
		<-stop
		return
	}
	defer fm.watcher.Close()

	for {
		select {
		case <-stop:
			return

		case event, ok := <-fm.watcher.Events:
			if !ok {
				return
			}
			fm.handleFSEvent(event)

		case err, ok := <-fm.watcher.Errors:
			if !ok {
				return
			}
			// Typically inotify queue overflow or transient EINTR. We log and
			// continue; the index is reconciled on the next gmuxd restart via
			// the bootstrap scan.
			log.Printf("filemon: watcher error: %v", err)
		}
	}
}

// handleFSEvent dispatches a single fsnotify event: it watches newly-created
// subdirectories (with catch-up) and forwards every .jsonl event to the index.
func (fm *FileMonitor) handleFSEvent(event fsnotify.Event) {
	if event.Has(fsnotify.Create) {
		fm.mu.Lock()
		var catchUp []indexWork
		dir := filepath.Dir(event.Name)
		if fm.watchedDirs[dir] {
			catchUp = fm.handleNewSubdirLocked(event.Name)
		}
		fm.mu.Unlock()

		// Run catch-up ScanFile calls outside fm.mu: the walk stays locked
		// (it modifies watchedDirs) but the per-file JSONL parse shouldn't.
		for _, w := range catchUp {
			fm.index.ScanFile(w.adapter, w.path)
		}
	}

	// The conversations index stays in sync with disk for every .jsonl event,
	// regardless of whether any session is alive.
	fm.notifyConvIndex(event)
}

// indexWork is a deferred ScanFile call that handleNewSubdirLocked returns to
// its caller. Decoupling collection (under fm.mu) from parsing (after release)
// keeps a large catch-up from blocking other fm.mu users.
type indexWork struct {
	adapter adapter.Adapter
	path    string
}

// handleNewSubdirLocked is called when a Create event fires inside a watched
// dir. Any new subdirectory is watched, and any pre-existing .jsonl files in
// it are returned as deferred ScanFile work for the caller to run after
// releasing fm.mu.
//
// Catch-up exists to close the `mkdir x && touch x/y.jsonl` race where a file
// lands in a fresh subdir between the dir's creation and our watch taking
// effect. We recurse so a deep subtree created by `mkdir -p YYYY/MM/DD` is
// fully covered.
func (fm *FileMonitor) handleNewSubdirLocked(path string) []indexWork {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return nil
	}
	fm.addWatchLocked(path)

	if fm.index == nil {
		return nil
	}
	var work []indexWork
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != path {
				fm.addWatchLocked(p)
			}
			return nil
		}
		if !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if a := fm.adapterForPath(p); a != nil {
			work = append(work, indexWork{adapter: a, path: p})
		}
		return nil
	})
	return work
}

// --- Watch management ---

func (fm *FileMonitor) ensureRootWatchLocked(root string) {
	if fm.rootDirs[root] {
		return
	}
	fm.addWatchLocked(root)
	fm.rootDirs[root] = true
}

func (fm *FileMonitor) addWatchLocked(dir string) {
	if fm.watcher == nil || fm.watchedDirs[dir] {
		return
	}
	if err := fm.watcher.Add(dir); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("filemon: watch %s: %v", dir, err)
		}
		return
	}
	fm.watchedDirs[dir] = true
}

// isUnderRoot reports whether dir is root itself or a subdirectory of root.
func isUnderRoot(dir, root string) bool {
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}
