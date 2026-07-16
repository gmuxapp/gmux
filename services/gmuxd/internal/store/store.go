package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
)

// Session is the in-memory model for a gmux session. Fields are grouped
// into API-visible (forwarded to the frontend) and internal (used by
// backend logic but excluded from MarshalJSON).
type Session struct {
	// ── API-visible fields ──
	ID string `json:"id"`

	// Peer identifies which gmuxd instance owns this session.
	// Empty = local. Non-empty = the peer name (manual peer in peers.json, or a discovered peer).
	Peer      string   `json:"peer,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	Command   []string `json:"command,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	// Adapter holds the adapter name ("pi", "claude", "shell", ...).
	Adapter       string            `json:"adapter"`
	WorkspaceRoot string            `json:"workspace_root,omitempty"`
	Remotes       map[string]string `json:"remotes,omitempty"`
	// ParentSessionID is the session this one was spawned from (e.g.
	// `gmux edit` invoked as $EDITOR inside an existing session). The
	// UI uses it to place the child next to its parent in the sidebar.
	ParentSessionID string  `json:"parent_session_id,omitempty"`
	Alive           bool    `json:"alive"`
	Pid             int     `json:"pid,omitempty"`
	ExitCode        *int    `json:"exit_code,omitempty"`
	StartedAt       string  `json:"started_at,omitempty"`
	ExitedAt        string  `json:"exited_at,omitempty"`
	Title           string  `json:"title,omitempty"`
	Subtitle        string  `json:"subtitle,omitempty"`
	Status          *Status `json:"status"`
	Unread          bool    `json:"unread"`

	// LastOutputAt timestamps the last time this session produced
	// *unseen* output — the read→unread transition. Used by the UI as
	// the activity-feed sort key so a session floats up the moment the
	// agent (or shell/editor) produces something the user hasn't looked
	// at. RFC3339, set by the owning daemon.
	//
	// Bumped on (and only on):
	//   - unread: false → true  (new output the user hasn't seen)
	//
	// Deliberately NOT bumped by the user's own input: status.working
	// false→true happens when you send / start a task, and reshuffling
	// the session you just acted on is exactly the jitter we avoid.
	// Exit and error aren't bumped either — in practice they arrive with
	// output that already trips unread, and the status dot covers the
	// rare silent case, so keeping the key output-only keeps it honest.
	//
	// Not bumped on: new session creation (the first unread transition
	// does it), title/cwd/slug/stamp changes, no-op status updates,
	// sessionmeta rehydrate at startup.
	//
	// A future last_input_at could track the user side (last send /
	// interaction) for an IM-style "recently used" ordering; not today.
	//
	// Peer sessions arrive over the wire with this already set by
	// the owning daemon; UpsertRemote preserves it as-received.
	LastOutputAt string `json:"last_output_at,omitempty"`
	Resumable    bool   `json:"resumable,omitempty"`
	SocketPath   string `json:"socket_path,omitempty"`
	TerminalCols uint16 `json:"terminal_cols,omitempty"`
	TerminalRows uint16 `json:"terminal_rows,omitempty"`
	// Slug is a human-readable stable identifier derived from the
	// adapter's conversation-derived slug. Used for URL routing (the frontend
	// falls back to the session id when empty). It is display-only and unique
	// within (adapter, peer).
	Slug string `json:"slug,omitempty"`

	// ConversationRef is the opaque, adapter-scoped ref of the conversation
	// this runner authoritatively writes, as reported by the agent hook
	// (session → conversation; ADR 0011). The daemon never interprets it —
	// only the owning adapter does (today's file-backed adapters use the
	// transcript's absolute path; a DB-backed adapter may use a row key).
	// Two live sessions may carry the same ConversationRef when the same
	// conversation is resumed in more than one tab; the frontend groups
	// by this to warn about a conversation open in multiple runners.
	// The wire/persisted key stays "conversation_file" for compatibility.
	ConversationRef string `json:"conversation_file,omitempty"`

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

	// conversationID and conversationAncestors are described once when a
	// local runner binds a conversation ref (prepareConversationLineage) and
	// drive lineage takeover. The daemon treats the ref itself as opaque
	// (ADR 0022); identity comes from the adapter's DescribeConversation.
	// Both are unexported: in-memory matching state, never marshaled.
	conversationID        string
	conversationAncestors []string
}

// MarshalJSON serializes a Session for the frontend API, excluding internal
// fields (ShellTitle, AdapterTitle) that are resolved into Title before sending.
func (s Session) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID              string            `json:"id"`
		Peer            string            `json:"peer,omitempty"`
		CreatedAt       string            `json:"created_at,omitempty"`
		Command         []string          `json:"command,omitempty"`
		Cwd             string            `json:"cwd,omitempty"`
		Adapter         string            `json:"adapter"`
		WorkspaceRoot   string            `json:"workspace_root,omitempty"`
		Remotes         map[string]string `json:"remotes,omitempty"`
		ParentSessionID string            `json:"parent_session_id,omitempty"`
		Alive           bool              `json:"alive"`
		Pid             int               `json:"pid,omitempty"`
		ExitCode        *int              `json:"exit_code,omitempty"`
		StartedAt       string            `json:"started_at,omitempty"`
		ExitedAt        string            `json:"exited_at,omitempty"`
		Title           string            `json:"title,omitempty"`
		Subtitle        string            `json:"subtitle,omitempty"`
		Status          *Status           `json:"status"`
		Unread          bool              `json:"unread"`
		Resumable       bool              `json:"resumable,omitempty"`
		SocketPath      string            `json:"socket_path,omitempty"`
		TerminalCols    uint16            `json:"terminal_cols,omitempty"`
		TerminalRows    uint16            `json:"terminal_rows,omitempty"`
		Slug            string            `json:"slug,omitempty"`
		ConversationRef string            `json:"conversation_file,omitempty"`
		RunnerVersion   string            `json:"runner_version,omitempty"`
		BinaryHash      string            `json:"binary_hash,omitempty"`
		ProjectSlug     string            `json:"project_slug,omitempty"`
		ProjectIndex    int               `json:"project_index,omitempty"`
		LastOutputAt    string            `json:"last_output_at,omitempty"`
	}
	return json.Marshal(wire{
		ID: s.ID, Peer: s.Peer, CreatedAt: s.CreatedAt, Command: s.Command,
		Cwd: s.Cwd, Adapter: s.Adapter, WorkspaceRoot: s.WorkspaceRoot,
		Remotes: s.Remotes, ParentSessionID: s.ParentSessionID,
		Alive: s.Alive, Pid: s.Pid,
		ExitCode: s.ExitCode, StartedAt: s.StartedAt, ExitedAt: s.ExitedAt,
		Title: s.Title, Subtitle: s.Subtitle, Status: s.Status,
		Unread: s.Unread, Resumable: s.Resumable,
		SocketPath: s.SocketPath, TerminalCols: s.TerminalCols,
		TerminalRows: s.TerminalRows, Slug: s.Slug,
		ConversationRef: s.ConversationRef,
		RunnerVersion:   s.RunnerVersion, BinaryHash: s.BinaryHash,
		ProjectSlug: s.ProjectSlug, ProjectIndex: s.ProjectIndex,
		LastOutputAt: s.LastOutputAt,
	})
}

// Status is the application-reported status. Carries only granular
// booleans; display text is derived in the frontend.
type Status struct {
	Working bool `json:"working"`
	Error   bool `json:"error,omitempty"`
}

// Event types on the store's subscriber bus. Only EventSessionUpsert
// carries a Session payload; consumers must dispatch on Type, not on
// Session == nil (an activity pulse is not a removal).
const (
	EventSessionUpsert   = "session-upsert"   // state change; Session present
	EventSessionRemove   = "session-remove"   // session dropped from the store
	EventSessionActivity = "session-activity" // transient output pulse; no Session
)

type Event struct {
	Type string `json:"type"` // EventSessionUpsert | EventSessionRemove | EventSessionActivity
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
	// activitySeed, when set, supplies a durable last-activity floor
	// (RFC3339) for a session that arrives with no LastOutputAt —
	// the rehydrate/re-register case after a daemon restart, where the
	// in-memory stamp was never persisted. Returns "" when no seed is
	// available. See SetActivitySeed.
	activitySeed func(Session) string

	lineageMu    sync.Mutex
	lineageByRef map[string]lineageEntry // key: adapter\x00ref (refs are unique only within an adapter, ADR 0022)
}

func New() *Store {
	return &Store{
		sessions:     make(map[string]Session),
		subscribers:  make(map[*subscriber]struct{}),
		lineageByRef: make(map[string]lineageEntry),
	}
}

// SetCommandTitlers configures per-adapter functions that derive a display
// title from a command array. Used as the fallback when no adapter or
// shell title is set (e.g. "codex" instead of "codex resume <id>").
func (s *Store) SetCommandTitlers(titlers map[string]func([]string) string) {
	s.commandTitlers = titlers
}

// SetActivitySeed installs a hook that reseeds LastOutputAt from a
// durable on-disk activity proxy (conversation-file / scrollback mtime)
// when a locally-owned session is upserted with an empty stamp. This
// recovers "last activity" across a daemon restart: an alive session's
// LastOutputAt is never persisted, so on re-register it would
// otherwise reset to (effectively) creation time. The hook is applied
// only as a seed/floor — a session that already carries a stamp (a live
// value, or a persisted one restored from sessionmeta) is never
// overwritten. seed may return "" to decline.
func (s *Store) SetActivitySeed(seed func(Session) string) {
	s.activitySeed = seed
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
// (non-peer) session with the given (adapter, slug). Slug is identity
// for sidebar purposes (see UBIQUITOUS_LANGUAGE.md), so this is the
// right check when deciding whether a project-tracked session is
// already represented, regardless of which instance ID it carries.
func (s *Store) HasLocalSlug(adapter, slug string) bool {
	if slug == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.Peer == "" && sess.Adapter == adapter && sess.Slug == slug {
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
	if fn := s.commandTitlers[sess.Adapter]; fn != nil && len(sess.Command) > 0 {
		return fn(sess.Command)
	}
	// Default: use the adapter name as the title (e.g. "codex", "claude").
	if sess.Adapter != "" {
		return sess.Adapter
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
// otherwise overwrite a correct Title with the adapter-name fallback.
//
// Canonicalization, duplicate-slug handling, unique-slug
// numbering, and event broadcast all still run; only the title and
// resumable derivation are skipped.
func (s *Store) UpsertRemote(sess Session) {
	s.upsertCommon(sess, false)
}

// upsertCommon is the shared body of Upsert and UpsertRemote.
// bumpLocally controls whether LastOutputAt is recomputed from
// the prev→next transition: true for locally-owned sessions, false
// for peer payloads (where the owning daemon already stamped it).
func (s *Store) upsertCommon(sess Session, bumpLocally bool) {
	sess.Cwd = paths.CanonicalizePath(sess.Cwd)
	sess.WorkspaceRoot = paths.CanonicalizePath(sess.WorkspaceRoot)
	// Compute the activity seed BEFORE taking the lock. The hook stats
	// an adapter-supplied path that may live on a remote/FUSE filesystem;
	// a hung stat under s.mu would stall the whole store (all reads, the
	// SSE snapshot fan-out), not just this upsert. It's only relevant when
	// the incoming stamp is empty (the rehydrate/fresh case), and it's
	// applied under the lock only if the stamp is still empty after
	// carry-forward — so it stays a pure floor.
	var seed string
	if bumpLocally && sess.LastOutputAt == "" && s.activitySeed != nil {
		seed = s.activitySeed(sess)
	}

	// Describing adapter conversations can be expensive. Do it before taking
	// the store mutex, and only once per local conversation ref; peer
	// snapshots arrive frequently and their refs belong to another host.
	s.prepareConversationLineage(&sess)

	s.mu.Lock()
	prev, hadPrev := s.sessions[sess.ID]
	if bumpLocally {
		if bumpsOutput(prev, sess, hadPrev) {
			sess.LastOutputAt = nowRFC3339()
		} else if hadPrev && sess.LastOutputAt == "" {
			// Preserve the previously stamped timestamp across
			// no-bump Upserts. Adapters call Upsert with fresh
			// Session structs built from runner state, which never
			// includes LastOutputAt; without this carry-forward,
			// a routine title/cwd refresh would silently zero out
			// the field and drop the session from Recent.
			sess.LastOutputAt = prev.LastOutputAt
		}
		// Reseed from a durable on-disk proxy when we still have no
		// stamp — the alive-across-restart case, where neither a live
		// value nor a persisted one exists. Gated on empty so it acts
		// purely as a floor and never clobbers a newer value; and only
		// on locally-owned sessions (peers carry the owner's stamp).
		if sess.LastOutputAt == "" && seed != "" {
			sess.LastOutputAt = seed
		}
	}
	removed, skip, unchanged := s.commitLocked(prev, hadPrev, &sess)
	s.mu.Unlock()
	s.broadcastCommit(sess, removed, skip, unchanged)
}

// commitLocked finalizes sess into the store and reports what the write
// did. It resolves conversation takeovers (which may evict shadowed
// sessions), assigns a unique display/URL slug, and stores the result. The caller must hold
// s.mu.
//
// Returns:
//   - removed: ids evicted by conversation-lineage takeover (a live session
//     shadows a dead conversation it resumes).
//   - skip: the write was dropped entirely (a dead conversation shadowed by a
//     live session that owns it); nothing was stored.
//   - unchanged: the stored session is byte-identical to prev, so this
//     was a no-op that must NOT broadcast session-upsert.
//
// The unchanged check is load-bearing. Every write path that broadcasts
// session-upsert (Upsert, UpsertRemote, Update) routes through here, so
// the dedup applies uniformly: a no-op write — a peer re-shipping its
// full snapshot at 20 Hz, the file monitor re-reading identical
// metadata, a status handler re-stamping the same status — never wakes
// every SSE subscriber to re-ship a byte-identical snapshot.sessions.
// Comparing the post-normalization value (after path canonicalization
// and unique-slug renumbering), not the caller's raw input, is what
// makes the comparison correct. See ADR 0001.
func (s *Store) commitLocked(prev Session, hadPrev bool, sess *Session) (removed []string, skip, unchanged bool) {
	removed, skip = s.resolveConversationTakeoverLocked(*sess)
	if skip {
		return removed, true, false
	}
	s.ensureUniqueSlug(sess)
	unchanged = hadPrev && reflect.DeepEqual(prev, *sess)
	s.sessions[sess.ID] = *sess
	return removed, false, unchanged
}

// broadcastCommit emits the events implied by a commitLocked result:
// one session-remove per evicted shadow, then a single session-upsert
// unless the write was skipped or a no-op. Caller must NOT hold s.mu.
func (s *Store) broadcastCommit(sess Session, removed []string, skip, unchanged bool) {
	for _, id := range removed {
		s.broadcast(Event{Type: EventSessionRemove, ID: id})
	}
	if skip || unchanged {
		return
	}
	s.broadcast(Event{
		Type:    EventSessionUpsert,
		ID:      sess.ID,
		Session: &sess,
	})
}

// Update atomically reads a session, applies a modifier function, and writes
// it back. This prevents read-modify-write races between concurrent updaters
// (e.g. the file monitor and the SSE subscriber goroutines).
// Returns false if the session doesn't exist.
func (s *Store) Update(id string, fn func(*Session)) bool {
	// A new conversation bind needs adapter I/O (DescribeConversation) that
	// must not run under s.mu. Rather than dropping the lock mid-update —
	// which could write a stale copy back over a concurrent dismiss or
	// update — warm the lineage cache OUTSIDE the critical section and
	// retry the whole update from fresh state. The loop is bounded: the
	// cache records failures too, so the second pass always hits it.
	// describedRef guards the unlock-describe-retry so it runs at most once
	// per distinct ref this call: termination is independent of whether the
	// describe succeeded or was cached (a transient describe failure is not
	// cached, so a cache-presence gate would livelock here).
	describedRef := ""
	for {
		s.mu.Lock()
		prev, ok := s.sessions[id]
		if !ok {
			s.mu.Unlock()
			return false
		}
		// prev is a by-value copy from the map, so prev.Unread is
		// unaffected by fn mutating sess — no pre-snapshot needed for a
		// plain bool (unlike the old Status-pointer path).
		sess := prev
		fn(&sess)
		sess.Cwd = paths.CanonicalizePath(sess.Cwd)
		sess.WorkspaceRoot = paths.CanonicalizePath(sess.WorkspaceRoot)
		sess.Title = s.resolveTitle(sess)
		sess.Resumable = !sess.Alive && len(sess.Command) > 0
		if bumpsOutput(prev, sess, true) {
			sess.LastOutputAt = nowRFC3339()
		}
		if sess.Peer == "" && sess.ConversationRef != "" && sess.ConversationRef != prev.ConversationRef {
			if describedRef != sess.ConversationRef {
				s.mu.Unlock()
				s.prepareConversationLineage(&sess)
				describedRef = sess.ConversationRef
				continue
			}
			// Second pass for this ref: re-apply the lineage onto the
			// fresh copy (cache hit when the describe succeeded; a
			// re-describe of a still-missing ref just yields empty again
			// and we proceed — no loop).
			s.prepareConversationLineage(&sess)
		}
		removed, skip, unchanged := s.commitLocked(prev, true, &sess)
		s.mu.Unlock()
		s.broadcastCommit(sess, removed, skip, unchanged)
		return true
	}
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
	if sess.TerminalCols == cols && sess.TerminalRows == rows {
		// No-op resize: the runner re-broadcasts terminal_resize even
		// when the dimensions are unchanged (e.g. a follower client
		// connecting at the same size). Don't fan out a byte-identical
		// snapshot.sessions for it. See ADR 0001.
		s.mu.Unlock()
		return true
	}
	sess.TerminalCols = cols
	sess.TerminalRows = rows
	s.sessions[id] = sess
	s.mu.Unlock()

	s.broadcast(Event{
		Type:    EventSessionUpsert,
		ID:      sess.ID,
		Session: &sess,
	})
	return true
}

// lineageKey scopes the lineage cache to a single adapter: opaque refs are
// unique only within an adapter (ADR 0022), so two adapters that happen to
// use the same ref string must not share a cache entry.
func lineageKey(adapterName, ref string) string {
	return adapterName + "\x00" + ref
}

// prepareConversationLineage describes the bound conversation at bind time,
// outside the store mutex. Its per-ref cache means recurring writes (notably
// peer snapshots) never re-describe. Peer (including Local-peer) refs are
// owned by another daemon, so they deliberately get no local takeover
// behavior. The ref is opaque to the daemon (ADR 0022): both the
// conversation's own ID and its ancestors come from the adapter.
func (s *Store) prepareConversationLineage(sess *Session) {
	if sess.Peer != "" || sess.ConversationRef == "" {
		return
	}
	key := lineageKey(sess.Adapter, sess.ConversationRef)
	s.lineageMu.Lock()
	defer s.lineageMu.Unlock()
	if cached, known := s.lineageByRef[key]; known {
		sess.conversationID = cached.id
		sess.conversationAncestors = cached.ancestors
		return
	}
	// A describe FAILURE (ref absent/unreadable — e.g. a resumed transcript
	// not yet on disk at first bind) is NOT cached: caching it would poison
	// the ref permanently and silently defeat takeover once the file lands.
	// A successful describe is stable (ancestors never disappear) and cached.
	var entry lineageEntry
	cache := false
	if a := adapters.FindByAdapter(sess.Adapter); a != nil {
		if describer, ok := a.(adapter.ConversationDescriber); ok {
			if info, err := describer.DescribeConversation(sess.ConversationRef); err == nil {
				entry = lineageEntry{id: info.ID, ancestors: info.AncestorIDs}
				cache = true
			}
		}
	} else {
		cache = true // no describer for this adapter: nothing to learn later
	}
	if cache {
		s.lineageByRef[key] = entry
	}
	sess.conversationID = entry.id
	sess.conversationAncestors = entry.ancestors
}

// lineageEntry caches what the adapter said about a conversation ref.
type lineageEntry struct {
	id        string
	ancestors []string
}

// conversationLineage is the set of conversation IDs a session's bound
// conversation covers: its own ID plus every ancestor it was resumed from.
// Empty when the adapter could not describe the ref — ref equality still
// applies then.
func conversationLineage(sess *Session) map[string]struct{} {
	lineage := make(map[string]struct{}, len(sess.conversationAncestors)+1)
	if sess.conversationID != "" {
		lineage[sess.conversationID] = struct{}{}
	}
	for _, id := range sess.conversationAncestors {
		lineage[id] = struct{}{}
	}
	return lineage
}

// coversConversation reports whether owner's bound conversation is, or
// descends from, other's: same opaque ref (adapter-scoped equality is
// daemon-legal even without a describe), or other's conversation ID is in
// owner's lineage.
func coversConversation(owner, other *Session) bool {
	if owner.ConversationRef == other.ConversationRef {
		return true
	}
	if other.conversationID == "" {
		return false
	}
	_, owns := conversationLineage(owner)[other.conversationID]
	return owns
}

// resolveConversationTakeoverLocked applies continuity only to local
// sessions bound to conversation refs. Within one adapter/peer scope, a live
// binder evicts dead rows whose conversation it covers (same ref, or an
// ancestor per the adapter's lineage); a dead write is skipped when a live
// row covers its conversation. Live rows always coexist. The caller must
// hold s.mu.
func (s *Store) resolveConversationTakeoverLocked(sess Session) (removed []string, skip bool) {
	if sess.Peer != "" || sess.ConversationRef == "" {
		return nil, false
	}
	for id, other := range s.sessions {
		if id == sess.ID || other.Adapter != sess.Adapter || other.Peer != sess.Peer || other.ConversationRef == "" {
			continue
		}
		switch {
		case sess.Alive && !other.Alive:
			if coversConversation(&sess, &other) {
				delete(s.sessions, id)
				removed = append(removed, id)
			}
		case !sess.Alive && other.Alive:
			if coversConversation(&other, &sess) {
				return removed, true
			}
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
			Type: EventSessionRemove,
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

// RemoveDeadByConversationRef retires every locally-owned, dead session
// of the given adapter whose ConversationRef matches ref, broadcasting
// session-remove for each and returning the removed IDs. Used when the
// conversations index reports a conversation has disappeared: meta.json
// mirrors conversation existence (ADR 0016), so the backing conversation
// going away retires the sidebar entry.
//
// Guards, all load-bearing:
//   - Adapter match: refs are only unique within an adapter (ADR 0022:
//     opaque, adapter-scoped), so adapter A reporting ref "x" deleted must
//     never retire adapter B's session that happens to carry the same
//     ref string.
//   - !Alive: a live runner may still be writing this conversation; its
//     record is authoritative and must not be torn down here.
//   - Peer == "": peer-owned records are re-received from the spoke; the
//     hub doesn't own their lifecycle.
//   - all matches: the conversation→session mapping is N:1 (a conversation
//     can be resumed in several runners), so every dead match is retired.
//
// Refs are compared by canonicalRef: exact equality, except that refs
// which are rooted paths (today's file-backed adapters) are compared
// after filepath.Clean so a cosmetic difference (trailing slash, "./")
// between the source-reported ref and the hook-reported ConversationRef
// doesn't defeat the match. Opaque refs are never normalized — an
// adapter may legitimately use "a/../b" and "b" as distinct locators.
// Symlinks are not resolved: the file is already gone, so EvalSymlinks
// couldn't run, and both paths derive from the same real $HOME in
// practice.
func (s *Store) RemoveDeadByConversationRef(adapterName, ref string) []string {
	if adapterName == "" || ref == "" {
		return nil
	}
	target := canonicalRef(ref)
	s.mu.Lock()
	var removed []string
	for id, sess := range s.sessions {
		if sess.Adapter != adapterName || sess.Peer != "" || sess.Alive || sess.ConversationRef == "" {
			continue
		}
		if canonicalRef(sess.ConversationRef) != target {
			continue
		}
		delete(s.sessions, id)
		removed = append(removed, id)
	}
	s.mu.Unlock()

	for _, id := range removed {
		s.broadcast(Event{Type: EventSessionRemove, ID: id})
	}
	return removed
}

// canonicalRef returns the comparison form of a conversation ref (ADR
// 0022). Refs are opaque, so the daemon must not interpret them — with
// one narrow, deliberate exception: a ref that is a rooted path is a
// file-backed adapter's transcript path, where the hook-reported and
// watcher-reported spellings can differ cosmetically ("./", trailing
// slash), so those are compared after filepath.Clean. Anything else is
// returned verbatim: normalizing an opaque ref could conflate locators
// the adapter considers distinct (e.g. "a/../b" vs "b").
func canonicalRef(ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return ref
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
		s.broadcast(Event{Type: EventSessionRemove, ID: id})
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

// ensureUniqueSlug keeps display/URL slugs distinct by appending "-2",
// "-3" suffixes when another session in the same (adapter, peer) scope already
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
			if existing.ID != sess.ID && existing.Adapter == sess.Adapter && existing.Peer == sess.Peer && existing.Slug == sess.Slug {
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

// bumpsOutput reports whether a prev→next transition is new unseen
// output (read → unread) — the sole trigger that stamps LastOutputAt.
// Intentionally output-only: the user's own input (working false→true),
// exit, and error do NOT bump (see the LastOutputAt docstring). New
// sessions (hadPrev=false) never bump; their first unread transition
// does, so a batch of dead sessions rehydrated at startup stays unstamped.
func bumpsOutput(prev, next Session, hadPrev bool) bool {
	return hadPrev && !prev.Unread && next.Unread
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
