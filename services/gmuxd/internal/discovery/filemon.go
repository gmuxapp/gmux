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
//   - .jsonl file Write/Create events trigger line processing for
//     already-attributed files. Unattributed files are queued and
//     matched on the next throttled attribution scan.
//
// Attribution:
//   - Candidates are all live sessions of the same adapter kind
//   - Content-similarity matching between file tail and session scrollback
//     (fetched via GET /scrollback/text on the runner) picks the right one
//   - Scrollback fetches happen off the event loop on a throttled timer
//   - Sticky: once attributed, re-match only when a DIFFERENT file writes
package discovery

import (
	"context"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// FileMonitor watches adapter session directories for live sessions.
type FileMonitor struct {
	store   *store.Store
	watcher *fsnotify.Watcher
	poke    chan struct{} // non-blocking signal to retry attribution
	index   *conversations.Index // optional; nil in unit tests

	// rootToAdapter maps each adapter's SessionRootDir() to its adapter.
	// Built once at construction from adapters.AllAdapters() so the
	// always-on path → adapter resolver works without a live session of
	// that kind. Read-only after NewFileMonitor returns.
	rootToAdapter map[string]adapter.Adapter

	mu             sync.Mutex
	watchedDirs    map[string]bool              // all dirs currently watched (roots + session dirs)
	rootDirs       map[string]bool              // session root dirs being watched
	sessions       map[string]*monitoredSession // sessionID -> info
	attributions   map[string]string            // filePath -> sessionID (sticky)
	activeFiles    map[string]string            // sessionID -> filePath (tracks current file for Slug)
	fileOffsets    map[string]int64             // filePath -> read offset
	candidateFiles map[string]bool              // files seen but not yet attributed
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
		store:          s,
		watcher:        w,
		poke:           make(chan struct{}, 1),
		rootToAdapter:  rootToAdapter,
		watchedDirs:    make(map[string]bool),
		rootDirs:       make(map[string]bool),
		sessions:       make(map[string]*monitoredSession),
		attributions:   attrs,
		activeFiles:    make(map[string]string),
		fileOffsets:    make(map[string]int64),
		candidateFiles: make(map[string]bool),
	}
}

// SetConvIndex wires the conversations index to receive ScanFile and
// RemoveByPath calls on .jsonl events. Must be called before Run
// starts; not safe to swap concurrently. Tests that don't exercise
// the index can leave it unset (calls become no-ops).
func (fm *FileMonitor) SetConvIndex(ix *conversations.Index) {
	fm.index = ix
}

// WatchRoots installs always-on fsnotify watches for every adapter
// SessionRootDir() and every existing subdirectory under it. Walks
// the tree depth-first so codex's date-nested layout (YYYY/MM/DD) is
// fully covered. Idempotent and safe to call once at gmuxd startup
// before Run begins.
//
// We mkdir any missing root because fsnotify can only watch existing
// directories, and we want to detect when a user starts using an
// adapter mid-session without forcing a daemon restart. Side effect:
// gmuxd creates an empty `~/.pi/agent/sessions/` (etc.) for every
// configured adapter, even ones the user has never used. Acceptable
// in exchange for not requiring a restart on first use.
//
// New subdirectories created later are picked up by handleFSEvent's
// Create handler, which adds a watch on any subdir created under an
// already-watched dir.
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

// watchTreeLocked walks the directory tree under root and adds a
// watch on every subdirectory. Errors on individual entries are
// logged but don't abort the walk.
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

// adapterForPath returns the adapter responsible for path by matching
// its directory against known SessionRootDir prefixes. Returns nil if
// no adapter claims the path.
func (fm *FileMonitor) adapterForPath(path string) adapter.Adapter {
	dir := filepath.Dir(path)
	for root, a := range fm.rootToAdapter {
		if isUnderRoot(dir, root) {
			return a
		}
	}
	return nil
}

// notifyConvIndex dispatches a .jsonl filesystem event to the
// conversations index. No-op if the index isn't wired (test mode).
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

// attributionThrottle is the minimum interval between proactive
// attribution scans. Keeps scrollback fetches bounded during bursts
// of session registrations or file writes.
const attributionThrottle = 3 * time.Second

// Run processes inotify events and proactive attribution scans until
// stop is closed. File events for already-attributed files are processed
// immediately (cheap, no network). Unattributed files are queued and
// matched on a throttled timer via tryAttributeUnmatched, which does the
// expensive scrollback fetches off the event loop.
func (fm *FileMonitor) Run(stop <-chan struct{}) {
	if fm.watcher == nil {
		<-stop
		return
	}
	defer fm.watcher.Close()

	// Throttle timer for proactive attribution. Nil when idle (no
	// unattributed files). Set to attributionThrottle after a poke.
	var throttle <-chan time.Time

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
			// Errors here are typically inotify queue overflow or
			// transient EINTR. We log and continue; the index gets
			// reconciled on the next gmuxd restart via the bootstrap
			// scan. Add an explicit reconcile hook here if reports
			// of staleness after overflow start coming in.
			log.Printf("filemon: watcher error: %v", err)

		case <-fm.poke:
			// New session or unattributed file. Start the throttle
			// timer if not already running.
			if throttle == nil {
				throttle = time.After(attributionThrottle)
			}

		case <-throttle:
			throttle = nil
			if fm.tryAttributeUnmatched() {
				// Still have unattributed files; keep retrying.
				throttle = time.After(attributionThrottle)
			}
		}
	}
}

// handleFSEvent dispatches a single fsnotify event.
func (fm *FileMonitor) handleFSEvent(event fsnotify.Event) {
	name := event.Name

	if event.Has(fsnotify.Create) {
		// A new entry was created. Could be:
		// 1. A new subdirectory under any watched dir -> add watch +
		//    catch up on .jsonl files that may already exist there.
		//    We check watchedDirs (not just rootDirs) so codex's
		//    YYYY/MM/DD nesting is supported: a new month dir under a
		//    watched year dir gets its own watch.
		// 2. A new .jsonl file in a watched dir -> handle as file change.
		fm.mu.Lock()
		var catchUp []indexWork
		dir := filepath.Dir(name)
		if fm.watchedDirs[dir] {
			catchUp = fm.handleNewSubdirLocked(name)
		}
		fm.mu.Unlock()

		// Run catch-up ScanFile calls outside fm.mu. The walk itself
		// stays locked (it modifies watchedDirs), but the expensive
		// part (per-file JSONL parse) shouldn't hold up other fm.mu
		// users like NotifyNewSession during a large catch-up.
		for _, w := range catchUp {
			fm.index.ScanFile(w.adapter, w.path)
		}

		if strings.HasSuffix(name, ".jsonl") {
			fm.handleFileChange(name)
		}
	}

	if event.Has(fsnotify.Write) {
		if strings.HasSuffix(name, ".jsonl") {
			fm.handleFileChange(name)
		}
	}

	// Conversations index stays in sync with disk for every .jsonl
	// event, regardless of whether any session is alive. This is the
	// path that replaces the old 30s periodic Scan.
	fm.notifyConvIndex(event)
}

// indexWork is a deferred ScanFile call that handleNewSubdirLocked
// returns to its caller. Decoupling collection (under fm.mu) from
// parsing (after release) keeps a large catch-up from blocking other
// fm.mu users like NotifyNewSession.
type indexWork struct {
	adapter adapter.Adapter
	path    string
}

// handleNewSubdirLocked is called when a Create event fires inside a
// watched dir. Any new subdirectory is watched, and any pre-existing
// .jsonl files in it are returned as deferred ScanFile work for the
// caller to run after releasing fm.mu.
//
// Catch-up exists to close the `mkdir x && touch x/y.jsonl` race
// where a file lands in a fresh subdir between the dir's creation
// and our watch taking effect. We recurse so a deep subtree created
// by `mkdir -p YYYY/MM/DD` is fully covered.
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

// NotifyNewSession registers a session for file monitoring.
// Sets up watches on the session root and all its subdirectories, seeds
// candidate files from recent .jsonl files, and signals the Run loop to
// attempt attribution on the next throttle tick.
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

	// Watch the session directory for the terminal's cwd. Create it if
	// needed (e.g. Codex date-nested layouts where today's dir doesn't
	// exist yet).
	dir := filer.SessionDir(sess.Cwd)
	if dir != "" {
		if _, err := os.Stat(dir); err != nil {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Printf("filemon: mkdir %s: %v", dir, err)
			}
		}
		fm.addWatchLocked(dir)
	}

	// Watch all other subdirectories under the session root. Tools may
	// write session files in directories other than SessionDir(cwd).
	// New directories created later are caught by handleNewSubdirLocked.
	if root != "" {
		fm.watchAllSubdirsLocked(root)
	}

	// Seed candidate files: collect recent .jsonl files so
	// tryAttributeUnmatched can match them. This handles gmuxd restart
	// (files already exist) and sessions that write before the inotify
	// watch is established.
	var startedAt time.Time
	if s, ok := fm.store.Get(sessionID); ok {
		startedAt, _ = time.Parse(time.RFC3339, s.StartedAt)
	}
	var nDirs int
	for d := range fm.watchedDirs {
		if fm.rootDirs[d] || root == "" || !isUnderRoot(d, root) {
			continue
		}
		nDirs++
		fm.collectCandidateFilesLocked(d, startedAt)
	}
	log.Printf("filemon: watching %d session dirs for %s (kind=%s)", nDirs, sessionID, sess.Kind)

	fm.pokeLocked()
}

// watchAllSubdirsLocked watches every immediate subdirectory under root.
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

// collectCandidateFilesLocked adds unattributed .jsonl files in dir
// modified after the threshold to the candidate set.
func (fm *FileMonitor) collectCandidateFilesLocked(dir string, modifiedAfter time.Time) {
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
		path := filepath.Join(dir, e.Name())
		if _, attributed := fm.attributions[path]; !attributed {
			fm.candidateFiles[path] = true
		}
	}
}

// pokeLocked sends a non-blocking signal to the Run loop to attempt
// attribution on the next throttle tick.
func (fm *FileMonitor) pokeLocked() {
	select {
	case fm.poke <- struct{}{}:
	default:
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

// --- File event handling ---

// handleFileChange processes a .jsonl file write/create event.
// Already-attributed files are processed immediately (cheap, no network).
// Unattributed files are added to the candidate set for the next
// throttled attribution scan.
func (fm *FileMonitor) handleFileChange(path string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	sessionID, attributed := fm.attributions[path]

	// Clear stale attributions: if the session ID no longer corresponds
	// to a monitored session (e.g. gmuxd restarted, old session gone),
	// treat the file as unattributed.
	if attributed {
		if _, ok := fm.sessions[sessionID]; !ok {
			delete(fm.attributions, path)
			attributed = false
		}
	}

	if !attributed {
		fm.candidateFiles[path] = true
		fm.pokeLocked()
		return
	}

	// Attributed file: process new lines immediately.
	fm.processAttributedFileLocked(sessionID, path)
}

// processAttributedFileLocked reads new lines from an attributed file
// and applies title/status/unread updates to the session. Must be
// called with fm.mu held.
func (fm *FileMonitor) processAttributedFileLocked(sessionID, path string) {
	ms, ok := fm.sessions[sessionID]
	if !ok {
		return
	}

	readAll := ms.readAll
	lines := fm.readNewLines(path, readAll)
	if readAll {
		ms.readAll = false
	}

	// Sync title + slug from the file. On a file change this always
	// re-derives; on subsequent writes it only re-derives when the
	// title is still a placeholder (first user message just arrived).
	fm.syncFileMetadataLocked(sessionID, path)

	if len(lines) == 0 {
		return
	}

	events := ms.fileMon.ParseNewLines(lines, path)
	if len(events) == 0 {
		return
	}

	fm.store.Update(sessionID, func(sess *store.Session) {
		for _, evt := range events {
			if evt.Title != "" {
				sess.AdapterTitle = evt.Title
			}
			if evt.Status != nil {
				if evt.Status.Label == "" && !evt.Status.Working && !evt.Status.Error {
					sess.Status = nil
				} else {
					sess.Status = &store.Status{
						Label:   evt.Status.Label,
						Working: evt.Status.Working,
						Error:   evt.Status.Error,
					}
				}
			}
			if evt.Unread != nil && !readAll {
				sess.Unread = *evt.Unread
			}
		}
	})
}

// --- Active file tracking ---

// syncFileMetadataLocked derives slug and title from the session file.
// Called when the active file changes (always re-derives) and on each
// write (re-derives only when the title is still a placeholder, since
// the first user message may arrive after attribution).
func (fm *FileMonitor) syncFileMetadataLocked(sessionID, filePath string) {
	fileChanged := fm.activeFiles[sessionID] != filePath
	fm.activeFiles[sessionID] = filePath

	ms, ok := fm.sessions[sessionID]
	if !ok {
		return
	}

	// Skip the file parse when nothing interesting could have changed.
	if !fileChanged {
		sess, ok := fm.store.Get(sessionID)
		if !ok {
			return
		}
		if sess.AdapterTitle != "" && sess.AdapterTitle != "(new)" {
			return // title already set, same file
		}
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
		if fileChanged || sess.AdapterTitle == "" || sess.AdapterTitle == "(new)" {
			sess.AdapterTitle = info.Title
		}
	})
}

// persistAttributionsLocked writes the current attributions to disk.
func (fm *FileMonitor) persistAttributionsLocked() {
	saveAttributions(fm.attributions, fm.sessions)
}

// --- Attribution ---

// tryAttributeUnmatched attempts to match candidate files to sessions
// using scrollback similarity. Called from the Run loop on a throttled
// timer. Returns true if unattributed files remain (caller should keep
// retrying).
//
// The expensive work (scrollback fetches, file I/O) happens with the
// lock released. The lock is only held briefly to snapshot state and
// record results.
func (fm *FileMonitor) tryAttributeUnmatched() bool {
	fm.mu.Lock()

	// Prune candidates that were attributed since they were queued,
	// or whose directory no longer maps to any live session's root
	// (the session died or was never relevant).
	var files []string
	for path := range fm.candidateFiles {
		if _, ok := fm.attributions[path]; ok {
			delete(fm.candidateFiles, path)
			continue
		}
		dir := filepath.Dir(path)
		hasKind := false
		for _, ms := range fm.sessions {
			if root := ms.filer.SessionRootDir(); root != "" && isUnderRoot(dir, root) {
				hasKind = true
				break
			}
		}
		if !hasKind {
			delete(fm.candidateFiles, path)
			continue
		}
		files = append(files, path)
	}
	if len(files) == 0 {
		fm.mu.Unlock()
		return false
	}

	// Snapshot session state needed for attribution.
	type sessionSnap struct {
		id         string
		cwd        string
		kind       string
		socketPath string
		startedAt  time.Time
	}
	snaps := make(map[string]*sessionSnap)
	for _, ms := range fm.sessions {
		snap := &sessionSnap{
			id: ms.id, cwd: ms.cwd, kind: ms.kind,
			socketPath: ms.socketPath,
		}
		if sess, ok := fm.store.Get(ms.id); ok {
			snap.startedAt, _ = time.Parse(time.RFC3339, sess.StartedAt)
		}
		snaps[ms.id] = snap
	}

	// Determine which adapter kind each file belongs to.
	fileKinds := make(map[string]string)
	for _, path := range files {
		dir := filepath.Dir(path)
		for _, ms := range fm.sessions {
			if root := ms.filer.SessionRootDir(); root != "" && isUnderRoot(dir, root) {
				fileKinds[path] = ms.kind
				break
			}
		}
	}

	// Snapshot adapter references for each kind.
	adapterByKind := make(map[string]adapter.Adapter)
	for _, ms := range fm.sessions {
		if _, ok := adapterByKind[ms.kind]; !ok {
			adapterByKind[ms.kind] = ms.adapter
		}
	}

	fm.mu.Unlock()

	// --- Expensive work outside the lock ---

	// Fetch scrollback for each session (one HTTP call each).
	scrollbacks := make(map[string]string)
	for id, snap := range snaps {
		scrollbacks[id] = fetchScrollbackText(snap.socketPath)
	}

	// Try to attribute each file.
	newAttrs := make(map[string]string)
	for _, path := range files {
		kind := fileKinds[path]
		if kind == "" {
			continue
		}

		var candidates []adapter.FileCandidate
		for _, snap := range snaps {
			if snap.kind != kind {
				continue
			}
			candidates = append(candidates, adapter.FileCandidate{
				SessionID:  snap.id,
				Cwd:        snap.cwd,
				StartedAt:  snap.startedAt,
				Scrollback: scrollbacks[snap.id],
			})
		}
		if len(candidates) == 0 {
			continue
		}

		a := adapterByKind[kind]
		attr, hasAttr := a.(adapter.FileAttributor)
		if hasAttr {
			if id := attr.AttributeFile(path, candidates); id != "" {
				newAttrs[path] = id
				continue
			}
			// Adapter couldn't match. Single candidate with a
			// freshly-written file: fall back to mtime heuristic.
			if len(candidates) == 1 {
				if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < 30*time.Second {
					newAttrs[path] = candidates[0].SessionID
				}
			}
		} else {
			// No FileAttributor; trivial attribution.
			newAttrs[path] = candidates[0].SessionID
		}
	}

	if len(newAttrs) == 0 {
		return true // candidates remain, keep retrying
	}

	// --- Apply results under the lock ---

	fm.mu.Lock()
	for path, sessionID := range newAttrs {
		if _, already := fm.attributions[path]; already {
			delete(fm.candidateFiles, path)
			continue
		}
		if _, ok := fm.sessions[sessionID]; !ok {
			continue // session died while we were fetching
		}
		fm.attributions[path] = sessionID
		delete(fm.candidateFiles, path)
		log.Printf("filemon: attributed %s -> %s", filepath.Base(path), sessionID)

		// Process the file: sets active file, reads all lines, derives
		// title, and applies status/title/unread updates.
		fm.processAttributedFileLocked(sessionID, path)
	}
	fm.persistAttributionsLocked()

	hasUnattributed := len(fm.candidateFiles) > 0
	fm.mu.Unlock()

	return hasUnattributed
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

func (fm *FileMonitor) removeWatchLocked(dir string) {
	if fm.watcher == nil || !fm.watchedDirs[dir] {
		return
	}
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

	resp, err := runnerRequest(context.Background(), socketPath, http.MethodGet, "/scrollback/text", nil)
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

// findAdapter looks up an adapter by kind among the non-fallback adapters
// (i.e. all adapters except shell). This is intentional: shell sessions do
// not use the file-monitoring pipeline (shell has no FileMonitor). Use
// adapters.FindByKind when the shell fallback must be included.
func findAdapter(kind string) adapter.Adapter {
	for _, a := range adapters.All {
		if a.Name() == kind {
			return a
		}
	}
	return nil
}
