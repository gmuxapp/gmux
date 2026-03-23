// Package discovery — file monitor component.
//
// FileMonitor watches adapter session directories using inotify (via fsnotify)
// and feeds new JSONL lines to the adapter's ParseNewLines to extract title
// and status updates. This is the "file-driven status" path that replaces
// PTY spinner detection for adapters like pi.
//
// Watching strategy:
//   - Session root dirs (e.g. ~/.pi/agent/sessions/) are always watched
//     so we detect new per-cwd subdirectories being created.
//   - Per-cwd session dirs are watched when they exist and have live sessions.
//   - .jsonl file Write/Create events trigger attribution + parsing.
//
// Attribution follows ADR-0009:
//   - Single live session per (cwd, kind) → trivially attributed
//   - Multiple sessions → content-similarity matching between file tail
//     and session scrollback (fetched via GET /scrollback/text on the runner)
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
	activeFiles  map[string]string            // sessionID → filePath (tracks current file for ResumeKey)
	fileOffsets  map[string]int64             // filePath → read offset
	// pendingDirs maps a session directory path to session IDs that need it.
	// Used when a session dir doesn't exist yet — we watch the root and
	// add the session dir watch when inotify reports its creation.
	pendingDirs map[string][]string // dirPath → []sessionID
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
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("filemon: failed to create watcher: %v", err)
	}
	return &FileMonitor{
		store:        s,
		watcher:      w,
		watchedDirs:  make(map[string]bool),
		rootDirs:     make(map[string]bool),
		sessions:     make(map[string]*monitoredSession),
		attributions: make(map[string]string),
		activeFiles:  make(map[string]string),
		fileOffsets:  make(map[string]int64),
		pendingDirs:  make(map[string][]string),
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
// If the created path matches a pending session directory, we add a watch and
// try attribution for the waiting sessions.
func (fm *FileMonitor) handleNewSubdirLocked(path string) {
	_, pending := fm.pendingDirs[path]
	if !pending {
		return
	}

	// Verify it's actually a directory.
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}

	// Add watch on the new session directory.
	fm.addWatchLocked(path)
	delete(fm.pendingDirs, path)

	// Do not eagerly attribute an existing JSONL file here.
	// A new session may start in a cwd that already has old session files,
	// and attributing the "most recent" file would leak a stale title into
	// the new live session. We wait for an actual file write/create event,
	// then attribute based on the writing file.
}

// NotifyNewSession registers a session for file monitoring.
// Watches the session directory (or the root dir if the session dir
// doesn't exist yet). The next file change triggers a full read.
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

	// Try to watch the session directory. If it doesn't exist yet,
	// register it as pending — we'll pick it up from the root watch.
	dir := filer.SessionDir(sess.Cwd)
	if dir == "" {
		return
	}

	// Ensure the directory exists. For adapters with date-nested layouts
	// (e.g. Codex: YYYY/MM/DD), the leaf dir may not exist yet. Creating
	// it is harmless and avoids complex multi-level pending watch logic.
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("filemon: mkdir %s: %v", dir, err)
			fm.pendingDirs[dir] = append(fm.pendingDirs[dir], sessionID)
			return
		}
	}
	fm.addWatchLocked(dir)
	log.Printf("filemon: watching %s for session %s (kind=%s)", dir, sessionID, sess.Kind)

	// Eagerly scan for the most recently modified file. This catches files
	// written before the watch was set up (e.g. gmuxd restart, or file
	// written between launch and watch registration). Attribution logic
	// in the adapter handles matching by cwd + timestamp proximity.
	fm.mu.Unlock()
	fm.scanDirMostRecent(dir)
	fm.mu.Lock()
}

// scanDirMostRecent processes the most recently modified .jsonl file in a
// directory. This catches files written before the watch was set up (e.g.
// gmuxd restart). Only the newest file is tried — the adapter's
// AttributeFile decides if it matches a live session (by cwd + timestamp).
func (fm *FileMonitor) scanDirMostRecent(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newest = filepath.Join(dir, e.Name())
		}
	}
	if newest != "" {
		fm.handleFileChange(newest)
	}
}

// ResolveResumeCommand derives the resume command for a session that just
// exited, using its ResumeKey (set during file attribution) and the
// adapter's Resumer interface. Returns nil if the session isn't resumable.
func (fm *FileMonitor) ResolveResumeCommand(sess *store.Session) []string {
	if sess.ResumeKey == "" {
		return nil
	}
	a := findAdapter(sess.Kind)
	if a == nil {
		return nil
	}
	resumer, ok := a.(adapter.Resumer)
	if !ok {
		return nil
	}

	// Build SessionFileInfo from what we know. Most adapters only need
	// the ID; Pi also needs FilePath.
	info := &adapter.SessionFileInfo{ID: sess.ResumeKey}

	fm.mu.Lock()
	for path, sid := range fm.attributions {
		if sid == sess.ID {
			info.FilePath = path
			break
		}
	}
	fm.mu.Unlock()

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
	for path, sid := range fm.attributions {
		if sid == sessionID {
			delete(fm.attributions, path)
			delete(fm.fileOffsets, path)
		}
	}

	// Remove from pending dirs.
	for dir, sids := range fm.pendingDirs {
		filtered := sids[:0]
		for _, sid := range sids {
			if sid != sessionID {
				filtered = append(filtered, sid)
			}
		}
		if len(filtered) == 0 {
			delete(fm.pendingDirs, dir)
		} else {
			fm.pendingDirs[dir] = filtered
		}
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
		if ms.filer.SessionDir(ms.cwd) == dir {
			return true
		}
	}
	return len(fm.pendingDirs[dir]) > 0
}

// handleFileChange processes a .jsonl file write/create event.
func (fm *FileMonitor) handleFileChange(path string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	dir := filepath.Dir(path)

	// Find who this file is attributed to.
	sessionID, attributed := fm.attributions[path]

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
	// (e.g. /new or /resume in the tool's TUI), update ResumeKey.
	fm.updateActiveFileLocked(sessionID, path)

	ms, ok := fm.sessions[sessionID]
	if !ok {
		return
	}

	lines := fm.readNewLines(path, ms.readAll)
	if ms.readAll {
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
				if evt.Status.Label == "" && !evt.Status.Working {
					sess.Status = nil // clear — no meaningful info to show
				} else {
					sess.Status = &store.Status{
						Label:   evt.Status.Label,
						Working: evt.Status.Working,
					}
				}
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
// ResumeKey when the file changes. This handles /new and /resume commands
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

	fm.store.Update(sessionID, func(sess *store.Session) {
		sess.ResumeKey = info.ID
	})
}

// --- Attribution ---

// attributeFileLocked determines which session a file belongs to.
func (fm *FileMonitor) attributeFileLocked(dir, filePath string) string {
	var candidates []*monitoredSession
	for _, ms := range fm.sessions {
		if ms.filer.SessionDir(ms.cwd) == dir {
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
			// Always fetch scrollback — needed for single-candidate
			// attribution too (pi uses scrollback similarity to avoid
			// matching old files from previous sessions in the same dir).
			fc.Scrollback = fetchScrollbackText(ms.socketPath)
			fileCandidates[i] = fc
		}
		if id := attr.AttributeFile(filePath, fileCandidates); id != "" {
			fm.attributions[filePath] = id
			log.Printf("filemon: attributed %s → %s", filepath.Base(filePath), id)
			return id
		}
		return "" // adapter rejected the file
	}

	// No FileAttributor — trivial attribution to first candidate.
	fm.attributions[filePath] = candidates[0].id
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

// --- Adapter/file helpers ---

func findAdapter(kind string) adapter.Adapter {
	for _, a := range adapters.All {
		if a.Name() == kind {
			return a
		}
	}
	return nil
}
