// Package sessionfiles scans adapter session directories and populates the
// store with resumable sessions. For each adapter implementing SessionFiler,
// it enumerates all session files and upserts resumable entries with real
// titles, resume commands, and deduplication against live sessions.
package sessionfiles

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/workspace"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// Scanner discovers resumable sessions from adapter session files.
type Scanner struct {
	store *store.Store

	// OnFirstScan is called once, right after the initial Scan + Purge
	// complete. At that point the store has the full set of known sessions,
	// making it safe to clean up stale references elsewhere (e.g. project
	// session arrays).
	OnFirstScan func()
}

func New(s *store.Store) *Scanner {
	return &Scanner{store: s}
}

// Run performs a scan immediately, then rescans periodically until stop is closed.
func (sc *Scanner) Run(interval time.Duration, stop <-chan struct{}) {
	sc.Scan()
	sc.PurgeStaleSessions(10 * time.Minute)
	if sc.OnFirstScan != nil {
		sc.OnFirstScan()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sc.Scan()
			sc.PurgeStaleSessions(10 * time.Minute)
		}
	}
}

// Scan enumerates all session directories for all SessionFiler adapters,
// parses each file, and upserts resumable entries into the store.
// Already-known sessions (by resume_key) are skipped.
func (sc *Scanner) Scan() {
	existing := sc.existingResumeKeys()

	for _, a := range adapters.AllAdapters() {
		sf, ok := a.(adapter.SessionFiler)
		if !ok {
			continue
		}
		resumer, hasResume := a.(adapter.Resumer)

		root := sf.SessionRootDir()
		if root == "" {
			continue
		}

		// If the adapter provides its own file listing (e.g. for
		// date-nested directories like Codex), use that. Otherwise
		// enumerate per-cwd subdirectories under the session root.
		var allFiles []string
		if lister, ok := a.(adapter.SessionFileLister); ok {
			allFiles = lister.ListSessionFiles()
		} else {
			subdirs, err := os.ReadDir(root)
			if err != nil {
				if !os.IsNotExist(err) {
					log.Printf("sessionfiles: read root %s: %v", root, err)
				}
				continue
			}
			for _, d := range subdirs {
				if !d.IsDir() {
					continue
				}
				dir := filepath.Join(root, d.Name())
				allFiles = append(allFiles, adapters.ListSessionFiles(dir)...)
			}
		}

		for _, path := range allFiles {
			info, err := sf.ParseSessionFile(path)
			if err != nil {
				continue
			}

			if existing[info.ID] {
				continue
			}

			if hasResume && !resumer.CanResume(path) {
				continue
			}

			var cmd []string
			if hasResume {
				cmd = resumer.ResumeCommand(info)
			}

			// Use the cwd from the session file header, not from
			// decoding the directory name (which is lossy for paths
			// containing dashes).
			cwd := info.Cwd
			if cwd == "" {
				continue
			}

			wsRoot := workspace.DetectRoot(cwd)
			sess := store.Session{
				ID:            "file-" + info.ID[:8],
				CreatedAt:     info.Created.UTC().Format(time.RFC3339),
				Command:       cmd,
				Cwd:           cwd,
				Kind:          a.Name(),
				WorkspaceRoot: wsRoot,
				Remotes:       workspace.DetectRemotes(wsRoot),
				Alive:         false,
				AdapterTitle:  info.Title,
				ResumeKey:     info.ID,
				// Resumable is derived by Upsert from !Alive + Command.
			}

			sc.store.Upsert(sess)
			existing[info.ID] = true
		}
	}
}

// existingResumeKeys returns resume_keys that should be skipped.
// Alive sessions and already-resumable sessions are skipped.
// Dead sessions without a command (Resumable=false) are NOT skipped
// so the scanner can re-derive their resume command.
func (sc *Scanner) existingResumeKeys() map[string]bool {
	keys := make(map[string]bool)
	for _, s := range sc.store.List() {
		if s.ResumeKey != "" && (s.Alive || s.Resumable) {
			keys[s.ResumeKey] = true
		}
	}
	return keys
}

// PurgeStaleSessions removes dead sessions that have no resume_key and
// are older than maxAge. These are short-lived gmux sessions that exited
// without ever being attributed to a session file.
func (sc *Scanner) PurgeStaleSessions(maxAge time.Duration) {
	now := time.Now().UTC()
	for _, s := range sc.store.List() {
		if s.Alive || s.Resumable || s.ResumeKey != "" {
			continue
		}
		exited, err := time.Parse(time.RFC3339, s.ExitedAt)
		if err != nil {
			continue
		}
		if now.Sub(exited) > maxAge {
			log.Printf("sessionfiles: purging stale session %s (exited %s ago)", s.ID, now.Sub(exited).Round(time.Second))
			sc.store.Remove(s.ID)
		}
	}
}
