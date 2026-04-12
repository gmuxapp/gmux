package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
)

// Session is the in-memory model for a gmux session. Fields are grouped
// into API-visible (forwarded to the frontend) and internal (used by
// backend logic but excluded from MarshalJSON).
type Session struct {
	// ── API-visible fields ──
	ID            string            `json:"id"`

	// Peer identifies which gmuxd instance owns this session.
	// Empty = local. Non-empty = the peer name from [[peers]] config.
	Peer string `json:"peer,omitempty"`
	CreatedAt     string            `json:"created_at,omitempty"`
	Command       []string          `json:"command,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	Kind          string            `json:"kind"`
	WorkspaceRoot string            `json:"workspace_root,omitempty"`
	Remotes       map[string]string `json:"remotes,omitempty"`
	Alive         bool              `json:"alive"`
	Pid           int               `json:"pid,omitempty"`
	ExitCode      *int              `json:"exit_code,omitempty"`
	StartedAt     string            `json:"started_at,omitempty"`
	ExitedAt      string            `json:"exited_at,omitempty"`
	Title         string            `json:"title,omitempty"`
	Subtitle      string            `json:"subtitle,omitempty"`
	Status        *Status           `json:"status"`
	Unread        bool              `json:"unread"`
	Resumable     bool              `json:"resumable,omitempty"`
	SocketPath    string            `json:"socket_path,omitempty"`
	TerminalCols  uint16            `json:"terminal_cols,omitempty"`
	TerminalRows  uint16            `json:"terminal_rows,omitempty"`
	// Slug is a human-readable stable identifier derived from the
	// adapter's session file slug. Used for URL routing (the frontend
	// falls back to id[:8] when empty) and for matching dead sessions
	// to project membership arrays. Unique within (kind, peer).
	Slug string `json:"slug,omitempty"`

	// RunnerVersion is the version string of the gmux runner binary that
	// launched this session. Set by the runner at startup. The frontend
	// derives staleness by comparing this to the daemon's own version
	// (and BinaryHash when both sides report the same version string).
	RunnerVersion string `json:"runner_version,omitempty"`

	// BinaryHash is the sha256 of the gmux runner binary that launched
	// this session. The frontend compares it against the daemon's
	// runner_hash (from /v1/health) to detect dev-mode hash drift.
	BinaryHash string `json:"binary_hash,omitempty"`

	// ── Internal fields (excluded from API via MarshalJSON) ──

	// Title inputs: resolveTitle merges these by precedence into Title
	// on every Upsert/Update.
	ShellTitle   string `json:"shell_title,omitempty"`
	AdapterTitle string `json:"adapter_title,omitempty"`
}

// MarshalJSON serializes a Session for the frontend API, excluding internal
// fields (ShellTitle, AdapterTitle) that are resolved into Title before sending.
func (s Session) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID            string            `json:"id"`
		Peer          string            `json:"peer,omitempty"`
		CreatedAt     string            `json:"created_at,omitempty"`
		Command       []string          `json:"command,omitempty"`
		Cwd           string            `json:"cwd,omitempty"`
		Kind          string            `json:"kind"`
		WorkspaceRoot string            `json:"workspace_root,omitempty"`
		Remotes       map[string]string `json:"remotes,omitempty"`
		Alive         bool              `json:"alive"`
		Pid           int               `json:"pid,omitempty"`
		ExitCode      *int              `json:"exit_code,omitempty"`
		StartedAt     string            `json:"started_at,omitempty"`
		ExitedAt      string            `json:"exited_at,omitempty"`
		Title         string            `json:"title,omitempty"`
		Subtitle      string            `json:"subtitle,omitempty"`
		Status        *Status           `json:"status"`
		Unread        bool              `json:"unread"`
		Resumable     bool              `json:"resumable,omitempty"`
		SocketPath    string            `json:"socket_path,omitempty"`
		TerminalCols  uint16            `json:"terminal_cols,omitempty"`
		TerminalRows  uint16            `json:"terminal_rows,omitempty"`
		Slug          string            `json:"slug,omitempty"`
		RunnerVersion string            `json:"runner_version,omitempty"`
		BinaryHash    string            `json:"binary_hash,omitempty"`
	}
	return json.Marshal(wire{
		ID: s.ID, Peer: s.Peer, CreatedAt: s.CreatedAt, Command: s.Command,
		Cwd: s.Cwd, Kind: s.Kind, WorkspaceRoot: s.WorkspaceRoot,
		Remotes: s.Remotes, Alive: s.Alive, Pid: s.Pid,
		ExitCode: s.ExitCode, StartedAt: s.StartedAt, ExitedAt: s.ExitedAt,
		Title: s.Title, Subtitle: s.Subtitle, Status: s.Status,
		Unread: s.Unread, Resumable: s.Resumable,
		SocketPath: s.SocketPath, TerminalCols: s.TerminalCols,
		TerminalRows: s.TerminalRows, Slug: s.Slug,
		RunnerVersion: s.RunnerVersion, BinaryHash: s.BinaryHash,
	})
}

// Status is the application-reported status.
type Status struct {
	Label   string `json:"label"`
	Working bool   `json:"working"`
	Error   bool   `json:"error,omitempty"`
}

type Event struct {
	Type string `json:"type"` // "session-upsert" | "session-remove"
	ID   string `json:"id"`

	// Present for session-upsert
	Session *Session `json:"session,omitempty"`
}

type subscriber struct {
	ch chan Event
}

type Store struct {
	mu             sync.RWMutex
	sessions       map[string]Session
	subscribers    map[*subscriber]struct{}
	commandTitlers map[string]func([]string) string
}

func New() *Store {
	return &Store{
		sessions:    make(map[string]Session),
		subscribers: make(map[*subscriber]struct{}),
	}
}

// SetCommandTitlers configures per-kind functions that derive a display
// title from a command array. Used as the fallback when no adapter or
// shell title is set (e.g. "codex" instead of "codex resume <id>").
func (s *Store) SetCommandTitlers(titlers map[string]func([]string) string) {
	s.commandTitlers = titlers
}

func (s *Store) List() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]Session, 0, len(s.sessions))
	for _, item := range s.sessions {
		items = append(items, item)
	}
	return items
}

func (s *Store) Get(id string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// resolveTitle picks the best title: adapter > shell > command fallback.
func (s *Store) resolveTitle(sess Session) string {
	if sess.AdapterTitle != "" {
		return sess.AdapterTitle
	}
	if sess.ShellTitle != "" {
		return sess.ShellTitle
	}
	// Ask the adapter for a command title if it implements CommandTitler.
	if fn := s.commandTitlers[sess.Kind]; fn != nil && len(sess.Command) > 0 {
		return fn(sess.Command)
	}
	// Default: use the adapter kind as the title (e.g. "codex", "claude").
	if sess.Kind != "" {
		return sess.Kind
	}
	return sess.Title
}

func (s *Store) Upsert(sess Session) {
	sess.Title = s.resolveTitle(sess)
	sess.Resumable = !sess.Alive && len(sess.Command) > 0
	s.upsertCommon(sess)
}

// UpsertRemote writes a session that was already fully resolved by a
// peer (spoke). Unlike Upsert it does NOT recompute Title or
// Resumable: those fields are authoritatively set on the spoke and
// arriving in the SSE payload already. The spoke intentionally keeps
// its internal ShellTitle/AdapterTitle fields off the wire, so the
// hub never has enough information to re-resolve correctly and would
// otherwise overwrite a correct Title with the adapter Kind fallback.
//
// Canonicalization, duplicate-slug handling, unique-slug
// numbering, and event broadcast all still run; only the title and
// resumable derivation are skipped.
func (s *Store) UpsertRemote(sess Session) {
	s.upsertCommon(sess)
}

// upsertCommon is the shared body of Upsert and UpsertRemote.
func (s *Store) upsertCommon(sess Session) {
	sess.Cwd = paths.CanonicalizePath(sess.Cwd)
	sess.WorkspaceRoot = paths.CanonicalizePath(sess.WorkspaceRoot)
	s.mu.Lock()
	removed, skip := s.resolveDuplicateSlugsLocked(sess)
	if !skip {
		s.ensureUniqueSlug(&sess)
		s.sessions[sess.ID] = sess
	}
	s.mu.Unlock()

	for _, id := range removed {
		s.broadcast(Event{Type: "session-remove", ID: id})
	}
	if skip {
		return
	}
	s.broadcast(Event{
		Type:    "session-upsert",
		ID:      sess.ID,
		Session: &sess,
	})
}

// Update atomically reads a session, applies a modifier function, and writes
// it back. This prevents read-modify-write races between concurrent updaters
// (e.g. the file monitor and the SSE subscriber goroutines).
// Returns false if the session doesn't exist.
func (s *Store) Update(id string, fn func(*Session)) bool {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	fn(&sess)
	sess.Cwd = paths.CanonicalizePath(sess.Cwd)
	sess.WorkspaceRoot = paths.CanonicalizePath(sess.WorkspaceRoot)
	sess.Title = s.resolveTitle(sess)
	sess.Resumable = !sess.Alive && len(sess.Command) > 0
	removed, skip := s.resolveDuplicateSlugsLocked(sess)
	if !skip {
		s.ensureUniqueSlug(&sess)
		s.sessions[id] = sess
	}
	s.mu.Unlock()

	for _, rid := range removed {
		s.broadcast(Event{Type: "session-remove", ID: rid})
	}
	if skip {
		return true
	}
	s.broadcast(Event{
		Type:    "session-upsert",
		ID:      id,
		Session: &sess,
	})
	return true
}

// GetTerminalSize returns the current terminal dimensions for a session.
func (s *Store) GetTerminalSize(id string) (cols, rows uint16, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, exists := s.sessions[id]
	if !exists {
		return 0, 0, false
	}
	return sess.TerminalCols, sess.TerminalRows, true
}

// SetTerminalSize updates the terminal dimensions for a session and broadcasts
// the change. Called by the WS proxy when the resize owner sends a resize.
func (s *Store) SetTerminalSize(id string, cols, rows uint16) bool {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	sess.TerminalCols = cols
	sess.TerminalRows = rows
	s.sessions[id] = sess
	s.mu.Unlock()

	s.broadcast(Event{
		Type:    "session-upsert",
		ID:      sess.ID,
		Session: &sess,
	})
	return true
}

// resolveDuplicateSlugsLocked deduplicates sessions that share a Slug
// within the same (kind, peer) scope. When a live session arrives and
// a dead session with the same slug exists, the dead one is removed.
// When a dead session arrives and a live one exists, the dead one is
// skipped.
func (s *Store) resolveDuplicateSlugsLocked(sess Session) (removed []string, skip bool) {
	if sess.Slug == "" {
		return nil, false
	}
	for id, other := range s.sessions {
		if id == sess.ID || other.Slug != sess.Slug {
			continue
		}
		// Same (kind, peer) scope check.
		if other.Kind != sess.Kind || other.Peer != sess.Peer {
			continue
		}
		switch {
		case sess.Alive && !other.Alive:
			delete(s.sessions, id)
			removed = append(removed, id)
		case !sess.Alive && other.Alive:
			return removed, true
		}
	}
	return removed, false
}

func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	_, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()

	if ok {
		s.broadcast(Event{
			Type: "session-remove",
			ID:   id,
		})
	}
	return ok
}

// RemoveByPeer removes all sessions belonging to a peer and broadcasts
// removal events. Returns the IDs that were removed.
func (s *Store) RemoveByPeer(peer string) []string {
	s.mu.Lock()
	var removed []string
	for id, sess := range s.sessions {
		if sess.Peer == peer {
			delete(s.sessions, id)
			removed = append(removed, id)
		}
	}
	s.mu.Unlock()

	for _, id := range removed {
		s.broadcast(Event{Type: "session-remove", ID: id})
	}
	return removed
}

// ListByPeer returns all session IDs belonging to a peer.
func (s *Store) ListByPeer(peer string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for id, sess := range s.sessions {
		if sess.Peer == peer {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Store) Subscribe() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 64)}

	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		delete(s.subscribers, sub)
		s.mu.Unlock()
		close(sub.ch)
	}
	return sub.ch, cancel
}

// Broadcast sends an event to all subscribers without modifying stored state.
// Used for transient signals (e.g. session-activity) that the frontend needs
// but that shouldn't be persisted in the session model.
func (s *Store) Broadcast(ev Event) {
	s.broadcast(ev)
}

func (s *Store) broadcast(ev Event) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for sub := range s.subscribers {
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

func NowUnix() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

// ensureUniqueSlug appends "-2", "-3" suffixes to the session's
// Slug when another session in the same (kind, peer) scope already
// uses that key. Sessions without a Slug (fresh launches before
// file attribution) are left alone; the frontend falls back to the
// session ID prefix for URL routing.
//
// Must be called with s.mu held.
func (s *Store) ensureUniqueSlug(sess *Session) {
	if sess.Slug == "" {
		return
	}
	base := sess.Slug
	for i := 2; ; i++ {
		conflict := false
		for _, existing := range s.sessions {
			if existing.ID != sess.ID && existing.Kind == sess.Kind && existing.Peer == sess.Peer && existing.Slug == sess.Slug {
				conflict = true
				break
			}
		}
		if !conflict {
			return
		}
		sess.Slug = fmt.Sprintf("%s-%d", base, i)
	}
}

