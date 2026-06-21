// Package filewatch is a small reusable recursive file-tree watcher. Adapters
// whose conversations live in on-disk files use it to implement
// adapter.ConversationSource without each reimplementing inotify bookkeeping.
package filewatch

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// Event reports a change to a watched file.
type Event struct {
	Path    string
	Removed bool // true for remove/rename; false for create/write
}

// Snapshot walks root recursively and calls emit(path) for every existing file
// whose name ends in suffix. Used for the initial enumeration before Watch.
func Snapshot(root, suffix string, emit func(path string)) {
	if root == "" {
		return
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), suffix) {
			emit(path)
		}
		return nil
	})
}

// Watch installs recursive fsnotify watches under root (creating root if it
// doesn't exist) and calls emit for create/write (Removed=false) and
// remove/rename (Removed=true) of files ending in suffix, until ctx is
// cancelled. New subdirectories are watched as they appear, with a catch-up
// pass that closes the `mkdir x && touch x/y` race. Returns ctx.Err() on
// cancellation.
//
// All callbacks fire from the single goroutine that calls Watch.
func Watch(ctx context.Context, root, suffix string, emit func(Event)) error {
	if root == "" {
		return nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	// fsnotify can only watch existing dirs; create root so a tool first used
	// mid-session is still observed without a daemon restart.
	if _, err := os.Stat(root); os.IsNotExist(err) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			log.Printf("filewatch: mkdir %s: %v", root, err)
		}
	}

	tw := &treeWatcher{w: w, suffix: suffix, emit: emit, watched: map[string]bool{}}
	tw.addTree(root)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			tw.handle(ev)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			// Typically inotify queue overflow or transient EINTR; the index is
			// reconciled from the next Snapshot on restart.
			log.Printf("filewatch: %s: %v", root, err)
		}
	}
}

// treeWatcher is owned by a single Watch goroutine; no locking needed.
type treeWatcher struct {
	w       *fsnotify.Watcher
	suffix  string
	emit    func(Event)
	watched map[string]bool
}

func (t *treeWatcher) addWatch(dir string) {
	if t.watched[dir] {
		return
	}
	if err := t.w.Add(dir); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("filewatch: watch %s: %v", dir, err)
		}
		return
	}
	t.watched[dir] = true
}

// addTree adds a watch on root and every subdirectory under it.
func (t *treeWatcher) addTree(root string) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		t.addWatch(path)
		return nil
	})
}

func (t *treeWatcher) handle(ev fsnotify.Event) {
	// A newly-created directory: watch it and catch up any files that already
	// landed inside (recurse for `mkdir -p a/b/c`).
	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			t.addTree(ev.Name)
			filepath.WalkDir(ev.Name, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if strings.HasSuffix(p, t.suffix) {
					t.emit(Event{Path: p})
				}
				return nil
			})
			return
		}
	}

	if !strings.HasSuffix(ev.Name, t.suffix) {
		return
	}
	switch {
	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		t.emit(Event{Path: ev.Name, Removed: true})
	case ev.Has(fsnotify.Create), ev.Has(fsnotify.Write):
		t.emit(Event{Path: ev.Name})
	}
}
