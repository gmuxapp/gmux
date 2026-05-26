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

	// LastActivityAt timestamps the most recent noteworthy state
	// transition for this session, used by the UI to surface
	// recently-relevant sessions (the home dashboard's "Recent"
	// section, sort keys, etc.). RFC3339, set by the owning daemon.
	//
	// Bumped on (and only on):
	//   - alive: true → false  (the session exited)
	//   - unread: false → true  (new output the user hasn't seen)
	//   - status.working: false → true  (adapter started a task)
	//   - status.error: false → true  (status went into error)
	//
	// Not bumped on: new session creation (the first follow-up
	// transition does it), title/cwd/slug/stamp changes, status
	// label-only updates, sessionmeta rehydrate at startup.
	//
	// Peer sessions arrive over the wire with this already set by
	// the owning daemon; UpsertRemote preserves it as-received.
	LastActivityAt string `json:"last_activity_at,omitempty"`
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

	// Project assignment stamps populated by the project Reconcile
	// step on the origin host. ProjectSlug is the slug of the project
	// whose Sessions[] array currently contains this session's key;
	// ProjectIndex is the 0-based position. Empty slug means the
	// origin disclaims the session, leaving viewers free to fall
	// through to their own match rules. See ADR 0002.
	//
	// These ride the wire so peers (and the browser) can render
	// (peer, slug) folders without re-running match rules locally.
	// Both use omitempty: ProjectSlug is meaningful only when set;
	// ProjectIndex defaults to 0 on decode, which is also a valid
	// first-position stamp, so the omit is safe round-trip.
	//
	// They are also persisted by sessionmeta (which uses default
	// reflection marshaling). That's a benign redundancy: the
	// startup flow always runs Reconcile after loading projects.json
	// and before any SSE subscriber attaches, so persisted stamps
	// can never be observed stale.
	ProjectSlug  string `json:"project_slug,omitempty"`
	ProjectIndex int    `json:"project_index,omitempty"`
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
		ProjectSlug   string            `json:"project_slug,omitempty"`
		ProjectIndex  int               `json:"project_index,omitempty"`
		LastActivityAt string           `json:"last_activity_at,omitempty"`
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
		ProjectSlug: s.ProjectSlug, ProjectIndex: s.ProjectIndex,
		LastActivityAt: s.LastActivityAt,
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

// HasLocalSlug reports whether the store already contains a local
// (non-peer) session with the given (kind, slug). Slug is identity
// for sidebar purposes (see UBIQUITOUS_LANGUAGE.md), so this is the
// right check when deciding whether a project-tracked session is
// already represented, regardless of which instance ID it carries.
func (s *Store) HasLocalSlug(kind, slug string) bool {
	if slug == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.Peer == "" && sess.Kind == kind && sess.Slug == slug {
			return true
		}
	}
	return false
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
	s.upsertCommon(sess, true)
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
	s.upsertCommon(sess, false)
}

// upsertCommon is the shared body of Upsert and UpsertRemote.
// bumpLocally controls whether LastActivityAt is recomputed from
// the prev→next transition: true for locally-owned sessions, false
// for peer payloads (where the owning daemon already stamped it).
func (s *Store) upsertCommon(sess Session, bumpLocally bool) {
	sess.Cwd = paths.CanonicalizePath(sess.Cwd)
	sess.WorkspaceRoot = paths.CanonicalizePath(sess.WorkspaceRoot)
	s.mu.Lock()
	if bumpLocally {
		prev, hadPrev := s.sessions[sess.ID]
		if shouldBumpActivity(snapshotActivity(prev), hadPrev, snapshotActivity(sess)) {
			sess.LastActivityAt = nowRFC3339()
		} else if hadPrev && sess.LastActivityAt == "" {
			// Preserve the previously stamped timestamp across
			// no-bump Upserts. Adapters call Upsert with fresh
			// Session structs built from runner state, which never
			// includes LastActivityAt; without this carry-forward,
			// a routine title/cwd refresh would silently zero out
			// the field and drop the session from Recent.
			sess.LastActivityAt = prev.LastActivityAt
		}
	}
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
	prev, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	prevSnap := snapshotActivity(prev)
	sess := prev
	fn(&sess)
	sess.Cwd = paths.CanonicalizePath(sess.Cwd)
	sess.WorkspaceRoot = paths.CanonicalizePath(sess.WorkspaceRoot)
	sess.Title = s.resolveTitle(sess)
	sess.Resumable = !sess.Alive && len(sess.Command) > 0
	if shouldBumpActivity(prevSnap, true, snapshotActivity(sess)) {
		sess.LastActivityAt = nowRFC3339()
	}
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

// Reconcile re-derives ProjectSlug and ProjectIndex for every session
// by calling assignFn. assignFn is expected to return ("", 0) for
// sessions that no project currently claims.
//
// The caller decides which sessions to claim. Local sessions and
// Local-peer (devcontainer) sessions both flow through assignFn; the
// caller is responsible for honouring origin-side stamps for genuine
// network peers (returning the session's existing stamp unchanged so
// the assignment doesn't get clobbered by parent rules).
//
// In-memory mutation only: no events are broadcast. Subscribers
// observe the new stamps the next time the session is re-emitted
// (e.g. via Upsert).
func (s *Store) Reconcile(assignFn func(Session) (slug string, index int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		slug, index := assignFn(sess)
		if sess.ProjectSlug == slug && sess.ProjectIndex == index {
			continue
		}
		sess.ProjectSlug = slug
		sess.ProjectIndex = index
		s.sessions[id] = sess
	}
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


// activitySnapshot captures the four scalar bits that determine
// whether a session transition bumps LastActivityAt. Snapshotting
// upfront (rather than dereferencing Session.Status during the
// comparison) is load-bearing: Update callers do `sess := prev`
// before running their mutator, which shallow-copies the Session
// but leaves prev.Status and sess.Status pointing at the same
// Status struct. A mutator that writes through that pointer
// (e.g. `sess.Status.Working = true`) would silently mutate prev
// too, hiding the transition from the bump check. Capturing the
// booleans before fn runs sidesteps the aliasing entirely.
type activitySnapshot struct {
	alive, unread, working, errored bool
}

func snapshotActivity(s Session) activitySnapshot {
	return activitySnapshot{
		alive:   s.Alive,
		unread:  s.Unread,
		working: s.Status != nil && s.Status.Working,
		errored: s.Status != nil && s.Status.Error,
	}
}

// shouldBumpActivity reports whether the prev→next session transition
// is noteworthy enough to refresh LastActivityAt. See the field's
// docstring for the exact bump set. Brand-new sessions (hadPrev=false)
// never bump: their first follow-up transition does, which avoids
// timestamping every sessionmeta-rehydrated dead session at daemon
// startup.
func shouldBumpActivity(prev activitySnapshot, hadPrev bool, next activitySnapshot) bool {
	if !hadPrev {
		return false
	}
	if prev.alive && !next.alive {
		return true
	}
	if !prev.unread && next.unread {
		return true
	}
	if !prev.working && next.working {
		return true
	}
	if !prev.errored && next.errored {
		return true
	}
	return false
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
