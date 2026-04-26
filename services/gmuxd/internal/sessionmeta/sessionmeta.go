// Package sessionmeta persists per-session metadata to disk so dead
// sessions survive a gmuxd restart and so future scrollback artifacts
// (S3) live alongside their owning session's record.
//
// gmuxd writes a meta.json file under
// $XDG_STATE_HOME/gmux/sessions/<id>/ every time a locally-owned
// session lands as Alive=false in the store. That covers the
// obvious Alive=true→false transitions (SSE exit, socket-gone,
// kill-unreachable) plus the less-obvious case of fast-exiting
// commands whose runner finishes before gmuxd's queryMeta arrives:
// the runner's /meta then reports alive=false directly, so the
// session lands as dead via Register's fresh-upsert path without
// any transition.
//
// On startup, Sweep loads each meta.json back into the store as
// Alive=false so previously-seen sessions remain visible in the
// sidebar across restarts. On Dismiss / Resume merge / slug
// takeover, the per-session directory is removed.
//
// Peer-owned sessions are excluded: the hub re-receives them from
// the spoke on reconnect, so persisting on the hub would create
// duplicate ghosts. Write is a no-op for sess.Peer != "".
//
// The package writes with mode 0o600 inside a 0o700 parent so
// scrollback (added in S3) and any future per-session secrets stay
// readable only by the gmux user.
package sessionmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

const (
	metaFile       = "meta.json"
	scrollbackFile = "scrollback" // owned by the runner; sweep recognizes it as non-orphan
	dirMode        = 0o700
	fileMode       = 0o600
)

// DefaultDir is the production state-dir for session metadata.
// $XDG_STATE_HOME/gmux/sessions, derived from paths.StateDir().
func DefaultDir() string {
	return filepath.Join(paths.StateDir(), "sessions")
}

// Store binds a base directory to the file-IO operations. One Store
// is constructed at gmuxd startup with DefaultDir(); tests construct
// their own with a temp dir.
type Store struct {
	dir string
}

// New returns a Store rooted at dir. The directory is created lazily
// on first Write; no IO happens at construction.
func New(dir string) *Store { return &Store{dir: dir} }

// Dir is the base directory under which per-session subdirectories
// live. Exposed for tests and for diagnostic logging.
func (s *Store) Dir() string { return s.dir }

// SessionDir is the per-session subdirectory: <Dir()>/<id>/.
func (s *Store) SessionDir(id string) string {
	return filepath.Join(s.dir, id)
}

// persistedSession aliases store.Session to bypass the API
// MarshalJSON method, which strips internal fields like ShellTitle
// and AdapterTitle. The default reflection-based marshaller
// respects the json tags on the original struct — including the
// ones for those internal fields — so no manual mirroring is
// needed and adding a new field to store.Session automatically
// flows into persistence.
type persistedSession store.Session

// Write atomically persists s to <dir>/<s.ID>/meta.json. Skips
// peer-owned sessions: the hub doesn't authoritatively own those
// records and re-receives them from the spoke on reconnect.
//
// Atomic via temp file + rename. The temp file is created in the
// same directory so rename(2) is on the same filesystem.
func (s *Store) Write(sess store.Session) error {
	if sess.Peer != "" {
		return nil // not ours to persist
	}
	if sess.ID == "" {
		return errors.New("sessionmeta: cannot persist session with empty id")
	}

	sessDir := s.SessionDir(sess.ID)
	if err := os.MkdirAll(sessDir, dirMode); err != nil {
		return fmt.Errorf("sessionmeta: mkdir %s: %w", sessDir, err)
	}

	data, err := json.MarshalIndent(persistedSession(sess), "", "  ")
	if err != nil {
		return fmt.Errorf("sessionmeta: marshal %s: %w", sess.ID, err)
	}

	final := filepath.Join(sessDir, metaFile)
	tmp, err := os.CreateTemp(sessDir, metaFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("sessionmeta: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("sessionmeta: write tmp: %w", err)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("sessionmeta: chmod tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("sessionmeta: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		cleanupTmp()
		return fmt.Errorf("sessionmeta: rename %s → %s: %w", tmpPath, final, err)
	}
	return nil
}

// Read returns the persisted session for id, or (zero, error) if
// the directory or meta.json is missing or unparseable.
func (s *Store) Read(id string) (store.Session, error) {
	path := filepath.Join(s.SessionDir(id), metaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return store.Session{}, err
	}
	var ps persistedSession
	if err := json.Unmarshal(data, &ps); err != nil {
		return store.Session{}, fmt.Errorf("sessionmeta: parse %s: %w", path, err)
	}
	return store.Session(ps), nil
}

// Remove deletes the entire per-session directory. Idempotent: a
// missing directory is not an error. Used for dismiss and for the
// resume-merge cleanup, where the dead session's record is replaced
// by a fresh live registration.
func (s *Store) Remove(id string) error {
	if id == "" {
		return nil
	}
	sessDir := s.SessionDir(id)
	if err := os.RemoveAll(sessDir); err != nil {
		return fmt.Errorf("sessionmeta: remove %s: %w", sessDir, err)
	}
	return nil
}

// WatchRemovals consumes events until the channel closes, removing
// the on-disk record for every session-remove event. Wired at
// gmuxd startup against store.Subscribe so the persister cleans up
// after every store removal, not just the dismiss / resume-merge
// paths that have explicit Remove calls.
//
// Catches the slug-takeover case: when a fresh live session
// shadows a dead one with the same (kind, peer, slug), the store
// silently evicts the dead record and broadcasts session-remove.
// Without this loop those orphan meta.json files accumulate — the
// next Sweep would re-load them and the next slug collision would
// re-orphan, indefinitely.
//
// Errors from Remove are logged; failure to clean up one entry
// doesn't stop the loop. Returns when the events channel closes
// (i.e., when the store subscription is cancelled at shutdown).
func (s *Store) WatchRemovals(events <-chan store.Event) {
	for ev := range events {
		if ev.Type != "session-remove" {
			continue
		}
		if err := s.Remove(ev.ID); err != nil {
			log.Printf("sessionmeta: cleanup remove %s: %v", ev.ID, err)
		}
	}
}

// Sweep loads every persisted session from disk. Each is returned
// with Alive=false; callers (gmuxd at startup) Upsert them so the
// sidebar shows previously-seen sessions before any live runners
// register.
//
// Cleanup performed during the sweep:
//   - directories with no meta.json (orphan scrollback or empty
//     dirs) are logged and removed
//   - directories whose meta.json fails to parse are logged and
//     left in place (operator may want to inspect)
//   - non-directory entries under the base dir are ignored
//
// Returns nil error when the base dir doesn't exist (clean install).
func (s *Store) Sweep() ([]store.Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionmeta: read %s: %w", s.dir, err)
	}

	var loaded []store.Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		sessDir := s.SessionDir(id)
		metaPath := filepath.Join(sessDir, metaFile)

		if _, err := os.Stat(metaPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				log.Printf("sessionmeta: orphan dir %s (no %s); removing", sessDir, metaFile)
				_ = os.RemoveAll(sessDir)
				continue
			}
			log.Printf("sessionmeta: stat %s: %v; skipping", metaPath, err)
			continue
		}

		sess, err := s.Read(id)
		if err != nil {
			log.Printf("sessionmeta: read %s: %v; skipping", metaPath, err)
			continue
		}
		// Sweep loads dead sessions only; if a live runner is still
		// listening, discovery.Register will upsert it with Alive=true
		// shortly after.
		sess.Alive = false
		loaded = append(loaded, sess)
	}
	return loaded, nil
}
