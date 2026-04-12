// Package discovery — file monitor component.
//
// FileMonitor watches adapter session directories using inotify (via fsnotify)
// and feeds new JSONL lines to the adapter's ParseNewLines to extract title
// and status updates. This is the "file-driven status" path that replaces
// PTY spinner detection for adapters like pi.
//
// Watching strategy:
//   - Session root dirs (e.g. ~/.pi/agent/sessions/) are always watched
//     so we detect new subdirectories being created.
//   - All subdirectories under the root are watched, not just the one
//     matching the terminal's cwd. Tools may write session files in other
//     directories (grove worktrees, /resume from a different cwd).
//   - .jsonl file Write/Create events trigger attribution + parsing.
//
// Attribution follows ADR-0009:
//   - Candidates are all live sessions of the same adapter kind
//   - Content-similarity matching between file tail and session scrollback
//     (fetched via GET /scrollback/text on the runner) picks the right one
//   - Sticky: once attributed, re-match only when a DIFFERENT file writes
package discovery

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// FileMonitor watches adapter session directories for live sessions.
type FileMonitor struct {
	store   *store.Store
	watcher *fsnotify.Watcher

	mu           sync.Mutex
	watchedDirs  map[string]bool              // all dirs currently watched (roots + session dirs)
	rootDirs     map[string]bool              // session root dirs being watched
	sessions     map[string]*monitoredSession // sessionID → info
	attributions map[string]string            // filePath → sessionID (sticky)
	activeFiles  map[string]string            // sessionID → filePath (tracks current file for Slug)
	fileOffsets  map[string]int64             // filePath → read offset

}

// monitoredSession tracks a live session for file monitoring.
type monitoredSession struct {
	id         string
	cwd        string
	kind       string
	socketPath string
	adapter    adapter.Adapter
	fileMon    adapter.FileMonitor
	filer      adapter.SessionFiler
	readAll    bool // true if we should read from beginning on first attribution
}

func NewFileMonitor(s *store.Store) *FileMonitor {
	return NewFileMonitorWithAttributions(s, loadAttributions())
}

// NewFileMonitorWithAttributions creates a FileMonitor pre-seeded with
// the given attributions. Used by NewFileMonitor (with persisted state
// from disk) and by tests.
func NewFileMonitorWithAttributions(s *store.Store, attrs map[string]string) *FileMonitor {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("filemon: failed to create watcher: %v", err)
	}
	if attrs == nil {
		attrs = make(map[string]string)
	}
	return &FileMonitor{
		store:        s,
		watcher:      w,
		watchedDirs:  make(map[string]bool),
		rootDirs:     make(map[string]bool),
		sessions:     make(map[string]*monitoredSession),
		attributions: attrs,
		activeFiles:  make(map[string]string),
		fileOffsets:  make(map[string]int64),
	}
}

// Run processes inotify events until stop is closed. Fully event-driven —
// no polling. Root dirs are watched to detect new session subdirectories.
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
			log.Printf("filemon: watcher error: %v", err)
		}
	}
}

// handleFSEvent dispatches a single fsnotify event.
func (fm *FileMonitor) handleFSEvent(event fsnotify.Event) {
	name := event.Name

	if event.Has(fsnotify.Create) {
		// A new entry was created. Could be:
		// 1. A new session subdirectory in a root dir → add watch
		// 2. A new .jsonl file in a session dir → handle as file change
		fm.mu.Lock()
		dir := filepath.Dir(name)
		if fm.rootDirs[dir] {
			// Created inside a root dir — check if it's a directory we're waiting for.
			fm.handleNewSubdirLocked(name)
		}
		fm.mu.Unlock()

		if strings.HasSuffix(name, ".jsonl") {
			fm.handleFileChange(name)
		}
	}

	if event.Has(fsnotify.Write) {
		if strings.HasSuffix(name, ".jsonl") {
			fm.handleFileChange(name)
		}
	}
}

// handleNewSubdirLocked is called when a Create event fires inside a root dir.
// Any new subdirectory is watched, because the tool may write session files
// in directories other than SessionDir(cwd) (e.g., grove worktrees, /resume
// from a different folder).
func (fm *FileMonitor) handleNewSubdirLocked(path string) {
	// Verify it's actually a directory.
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}

	fm.addWatchLocked(path)

}

// NotifyNewSession registers a session for file monitoring.
// Watches all subdirectories under the session root (not just the one
// matching the terminal's cwd) so that files in other directories are
// detected. The next file change triggers a full read.
func (fm *FileMonitor) NotifyNewSession(sessionID string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	sess, ok := fm.store.Get(sessionID)
	if !ok || sess.Cwd == "" {
		return
	}

	a := findAdapter(sess.Kind)
	if a == nil {
		return
	}
	fileMon, ok := a.(adapter.FileMonitor)
	if !ok {
		return
	}
	filer, ok := a.(adapter.SessionFiler)
	if !ok {
		return
	}

	fm.sessions[sessionID] = &monitoredSession{
		id:         sessionID,
		cwd:        sess.Cwd,
		kind:       sess.Kind,
		socketPath: sess.SocketPath,
		adapter:    a,
		fileMon:    fileMon,
		filer:      filer,
		readAll:    true,
	}

	// Ensure the root dir is watched (to catch new session subdirs).
	root := filer.SessionRootDir()
	if root != "" {
		fm.ensureRootWatchLocked(root)
	}

	// Watch the session directory for the terminal's cwd. This is the
	// most likely location for new session files. Create it if needed
	// (e.g. Codex date-nested layouts where today's dir doesn't exist).
	dir := filer.SessionDir(sess.Cwd)
	if dir != "" {
		if _, err := os.Stat(dir); err != nil {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Printf("filemon: mkdir %s: %v", dir, err)
			}
		}
		fm.addWatchLocked(dir)
	}

	// Also watch all other subdirectories under the session root.
	// Tools may write session files in directories other than
	// SessionDir(cwd). For example, pi with grove worktrees uses the
	// worktree path as its cwd, and /resume can open a session from
	// any previous cwd.
	// New directories created later are caught by handleNewSubdirLocked.
	if root != "" {
		fm.watchAllSubdirsLocked(root)
	}

	// Eagerly scan for recently-modified session files. Only files
	// modified after the session started are scanned, to avoid pulling
	// in stale titles from old sessions. We scan all watched dirs under
	// this adapter's root, not just SessionDir(cwd), so files in
	// other directories (grove worktrees, /resume targets) are found.
	var startedAt time.Time
	if s, ok := fm.store.Get(sessionID); ok {
		startedAt, _ = time.Parse(time.RFC3339, s.StartedAt)
	}
	var dirsToScan []string
	for d := range fm.watchedDirs {
		if !fm.rootDirs[d] && root != "" && isUnderRoot(d, root) {
			dirsToScan = append(dirsToScan, d)
		}
	}
	log.Printf("filemon: watching %d session dirs for %s (kind=%s)", len(dirsToScan), sessionID, sess.Kind)

	fm.mu.Unlock()
	for _, d := range dirsToScan {
		fm.scanDirForRecentSessions(d, startedAt)
	}
	fm.mu.Lock()
}

// watchAllSubdirsLocked watches every immediate subdirectory under root.
// This covers session directories for any cwd, not just the one matching
// the terminal's cwd.
func (fm *FileMonitor) watchAllSubdirsLocked(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fm.addWatchLocked(filepath.Join(root, e.Name()))
	}
}

// scanDirForRecentSessions processes .jsonl files in a directory that
// were modified after the given threshold. Files modified before are
// skipped to avoid attributing stale sessions.
func (fm *FileMonitor) scanDirForRecentSessions(dir string, modifiedAfter time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !modifiedAfter.IsZero() && info.ModTime().Before(modifiedAfter) {
			continue
		}
		fm.handleFileChange(filepath.Join(dir, e.Name()))
	}
}

// ResolveResumeCommand derives the resume command for a session that just
// exited, by re-parsing its attributed session file. Returns nil if the
// session has no attribution or isn't resumable.
func (fm *FileMonitor) ResolveResumeCommand(sess *store.Session) []string {
	a := findAdapter(sess.Kind)
	if a == nil {
		return nil
	}
	filer, hasFiler := a.(adapter.SessionFiler)
	if !hasFiler {
		return nil
	}
	resumer, hasResume := a.(adapter.Resumer)
	if !hasResume {
		return nil
	}

	// Find the attributed file path for this session.
	fm.mu.Lock()
	var filePath string
	for path, sid := range fm.attributions {
		if sid == sess.ID {
			filePath = path
			break
		}
	}
	fm.mu.Unlock()

	if filePath == "" {
		return nil
	}

	// Re-parse the file to get the tool's real ID and metadata.
	info, err := filer.ParseSessionFile(filePath)
	if err != nil {
		return nil
	}

	return resumer.ResumeCommand(info)
}

// NotifySessionDied removes a session from monitoring.
func (fm *FileMonitor) NotifySessionDied(sessionID string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	ms, exists := fm.sessions[sessionID]
	delete(fm.sessions, sessionID)
	delete(fm.activeFiles, sessionID)

	// Remove attributions pointing to this session.
	changed := false
	for path, sid := range fm.attributions {
		if sid == sessionID {
			delete(fm.attributions, path)
			delete(fm.fileOffsets, path)
			changed = true
		}
	}
	if changed {
		fm.persistAttributionsLocked()
	}

	// If no more sessions need this session dir, remove the watch.
	if exists && ms != nil {
		dir := ms.filer.SessionDir(ms.cwd)
		if dir != "" && !fm.dirNeededLocked(dir) {
			fm.removeWatchLocked(dir)
		}
	}
}

// dirNeededLocked returns true if any live session needs a watch on dir.
func (fm *FileMonitor) dirNeededLocked(dir string) bool {
	for _, ms := range fm.sessions {
		if root := ms.filer.SessionRootDir(); root != "" && isUnderRoot(dir, root) {
			return true
		}
	}
	return false
}

// handleFileChange processes a .jsonl file write/create event.
func (fm *FileMonitor) handleFileChange(path string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	dir := filepath.Dir(path)

	// Find who this file is attributed to.
	sessionID, attributed := fm.attributions[path]

	// Clear stale attributions: if the session ID no longer corresponds
	// to a monitored session (e.g. gmuxd restarted, old session gone),
	// re-attribute so the file can match the current live session.
	if attributed {
		if _, ok := fm.sessions[sessionID]; !ok {
			delete(fm.attributions, path)
			attributed = false
		}
	}

	if !attributed {
		sessionID = fm.attributeFileLocked(dir, path)
		if sessionID == "" {
			return
		}
		// New attribution — parse the full file for initial title.
		// This handles "name > first user message > (new)" correctly
		// without requiring ParseNewLines to track message order.
		if title := fm.deriveTitleFromFile(sessionID, path); title != "" {
			fm.store.Update(sessionID, func(s *store.Session) {
				s.AdapterTitle = title
			})
		}
	}

	// Track active file per session. When the active file changes
	// (e.g. /new or /resume in the tool's TUI), update Slug.
	fm.updateActiveFileLocked(sessionID, path)

	ms, ok := fm.sessions[sessionID]
	if !ok {
		return
	}

	readAll := ms.readAll
	lines := fm.readNewLines(path, readAll)
	if readAll {
		ms.readAll = false
	}
	if len(lines) == 0 {
		return
	}

	events := ms.fileMon.ParseNewLines(lines, path)

	// If adapter title is still unset (file was attributed before any user
	// messages), re-derive it from the full file. This catches the common
	// case where the tool creates the file on launch but only writes user
	// messages later. Derive the title outside the lock (file I/O), then
	// apply atomically with a condition check inside Update.
	title := fm.deriveTitleFromFile(sessionID, path)
	if title != "" {
		fm.store.Update(sessionID, func(s *store.Session) {
			if s.AdapterTitle == "" || s.AdapterTitle == "(new)" {
				s.AdapterTitle = title
				// Also refresh slug when the title transitions from
				// placeholder to real content (first user message).
				newSlug := adapter.Slugify(title)
				if newSlug != "" && (s.Slug == "" || s.Slug == "new") {
					s.Slug = newSlug
				}
			}
		})
	}

	if len(events) == 0 {
		return
	}

	// Apply all events atomically to avoid races with the SSE subscriber.
	fm.store.Update(sessionID, func(sess *store.Session) {
		for _, evt := range events {
			if evt.Title != "" {
				sess.AdapterTitle = evt.Title
			}
			if evt.Status != nil {
				if evt.Status.Label == "" && !evt.Status.Working && !evt.Status.Error {
					sess.Status = nil // clear — no meaningful info to show
				} else {
					sess.Status = &store.Status{
						Label:   evt.Status.Label,
						Working: evt.Status.Working,
						Error:   evt.Status.Error,
					}
				}
			}
			// Skip unread events from full-file re-reads (e.g. session
			// restart). Historical assistant turns are not new output.
			if evt.Unread != nil && !readAll {
				sess.Unread = *evt.Unread
			}
		}
	})
}

// deriveTitleFromFile parses the full session file and returns the best title
// (name > first user message > ""). Called on first attribution and when
// the adapter title is still unset, to derive the initial title without
// relying on ParseNewLines. Returns "" if no title can be derived.
func (fm *FileMonitor) deriveTitleFromFile(sessionID, filePath string) string {
	ms, ok := fm.sessions[sessionID]
	if !ok {
		return ""
	}
	filer, ok := ms.adapter.(adapter.SessionFiler)
	if !ok {
		return ""
	}
	info, err := filer.ParseSessionFile(filePath)
	if err != nil || info.Title == "" {
		return ""
	}
	return info.Title
}

// --- Active file tracking ---

// updateActiveFileLocked sets the active file for a session and updates
// Slug when the file changes. This handles /new and /resume commands
// in the tool's TUI, which start writing to a different session file.
func (fm *FileMonitor) updateActiveFileLocked(sessionID, filePath string) {
	prev := fm.activeFiles[sessionID]
	if prev == filePath {
		return // same file, nothing to do
	}
	fm.activeFiles[sessionID] = filePath

	ms, ok := fm.sessions[sessionID]
	if !ok {
		return
	}
	info, err := ms.filer.ParseSessionFile(filePath)
	if err != nil || info.ID == "" {
		return
	}

	slug := info.Slug
	if slug == "" {
		slug = adapter.Slugify(info.ID)
	}

	fm.store.Update(sessionID, func(sess *store.Session) {
		sess.Slug = slug
	})
}

// persistAttributionsLocked writes the current attributions to disk.
// Must be called with fm.mu held.
func (fm *FileMonitor) persistAttributionsLocked() {
	saveAttributions(fm.attributions, fm.sessions)
}

// --- Attribution ---

// attributeFileLocked determines which session a file belongs to.
// Candidates are all live sessions of the same adapter kind, regardless
// of which session directory the file is in. This handles tools that
// may write session files outside SessionDir(cwd), e.g. grove worktrees
// or /resume from a different folder.
func (fm *FileMonitor) attributeFileLocked(dir, filePath string) string {
	// Determine the adapter kind from the directory's root.
	// All session dirs live under a single root per adapter kind.
	var kind string
	for _, ms := range fm.sessions {
		if root := ms.filer.SessionRootDir(); root != "" && isUnderRoot(dir, root) {
			kind = ms.kind
			break
		}
	}
	if kind == "" {
		return ""
	}

	var candidates []*monitoredSession
	for _, ms := range fm.sessions {
		if ms.kind == kind {
			candidates = append(candidates, ms)
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	// Delegate to the adapter's FileAttributor if available.
	attr, hasAttr := candidates[0].adapter.(adapter.FileAttributor)
	if hasAttr {
		fileCandidates := make([]adapter.FileCandidate, len(candidates))
		for i, ms := range candidates {
			fc := adapter.FileCandidate{
				SessionID: ms.id,
				Cwd:       ms.cwd,
			}
			if sess, ok := fm.store.Get(ms.id); ok {
				fc.StartedAt, _ = time.Parse(time.RFC3339, sess.StartedAt)
			}
			// Fetch scrollback for content-similarity matching (pi).
			fc.Scrollback = fetchScrollbackText(ms.socketPath)
			fileCandidates[i] = fc
		}
		if id := attr.AttributeFile(filePath, fileCandidates); id != "" {
			fm.attributions[filePath] = id
			fm.persistAttributionsLocked()
			log.Printf("filemon: attributed %s → %s", filepath.Base(filePath), id)
			return id
		}

		// Adapter couldn't match by content/timestamp. For single-candidate
		// dirs, fall back if the file was just written (mtime within 30s).
		// This handles /new files during live operation without racing with
		// sequential session registration during startup scans (where old
		// files would have stale mtimes).
		if len(candidates) == 1 {
			if info, err := os.Stat(filePath); err == nil {
				if time.Since(info.ModTime()) < 30*time.Second {
					fm.attributions[filePath] = candidates[0].id
					fm.persistAttributionsLocked()
					log.Printf("filemon: attributed %s → %s (fresh single-candidate)", filepath.Base(filePath), candidates[0].id)
					return candidates[0].id
				}
			}
		}
		return ""
	}

	// No FileAttributor — trivial attribution to first candidate.
	fm.attributions[filePath] = candidates[0].id
	fm.persistAttributionsLocked()
	return candidates[0].id
}

// New sessions are not eagerly attributed to an existing file.
// Attribution only happens on an actual write/create event for a JSONL file.
// This avoids reusing the title from the most recent old session in the same cwd.

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

func (fm *FileMonitor) removeWatchLocked(dir string) {
	if fm.watcher == nil || !fm.watchedDirs[dir] {
		return
	}
	// Don't remove root dir watches.
	if fm.rootDirs[dir] {
		return
	}
	fm.watcher.Remove(dir)
	delete(fm.watchedDirs, dir)
}

// --- File reading ---

func (fm *FileMonitor) readNewLines(path string, readAll bool) []string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	offset := fm.fileOffsets[path]
	if readAll {
		offset = 0
	}
	if info.Size() <= offset {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil
		}
	}

	buf := make([]byte, info.Size()-offset)
	n, err := f.Read(buf)
	if err != nil || n == 0 {
		return nil
	}
	fm.fileOffsets[path] = offset + int64(n)

	text := string(buf[:n])
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var result []string
	for _, l := range lines {
		if l != "" {
			result = append(result, l)
		}
	}
	return result
}

// --- Network helpers ---

func fetchScrollbackText(socketPath string) string {
	if socketPath == "" {
		return ""
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 2*time.Second)
			},
		},
		Timeout: 3 * time.Second,
	}

	resp, err := client.Get("http://localhost/scrollback/text")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return ""
	}
	return string(body)
}

// isUnderRoot reports whether dir is root itself or a subdirectory of root.
func isUnderRoot(dir, root string) bool {
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// --- Adapter/file helpers ---

func findAdapter(kind string) adapter.Adapter {
	for _, a := range adapters.All {
		if a.Name() == kind {
			return a
		}
	}
	return nil
}
