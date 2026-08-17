package peering

import "reflect"

// SessionProjection is the authority-neutral, copyable projection received
// from a peer's snapshot.sessions stream. It intentionally contains only
// wire/runtime facts; no durable store type crosses the peering boundary.
type SessionProjection struct {
	ID                    string            `json:"id"`
	Peer                  string            `json:"peer,omitempty"`
	CreatedAt             string            `json:"created_at,omitempty"`
	Command               []string          `json:"command,omitempty"`
	Cwd                   string            `json:"cwd,omitempty"`
	Adapter               string            `json:"adapter"`
	DriveMode             string            `json:"drive_mode,omitempty"`
	WorkspaceRoot         string            `json:"workspace_root,omitempty"`
	Remotes               map[string]string `json:"remotes,omitempty"`
	ParentSessionID       string            `json:"parent_session_id,omitempty"`
	LaunchedFromSessionID string            `json:"launched_from_session_id,omitempty"`
	SemanticAgent         bool              `json:"semantic_agent,omitempty"`
	Alive                 bool              `json:"alive"`
	Pid                   int               `json:"pid,omitempty"`
	ExitCode              *int              `json:"exit_code,omitempty"`
	StartedAt             string            `json:"started_at,omitempty"`
	ExitedAt              string            `json:"exited_at,omitempty"`
	Title                 string            `json:"title,omitempty"`
	Subtitle              string            `json:"subtitle,omitempty"`
	Status                *SessionStatus    `json:"status"`
	Unread                bool              `json:"unread"`
	UnreadToken           string            `json:"unread_token"`
	Resumable             bool              `json:"resumable,omitempty"`
	SocketPath            string            `json:"socket_path,omitempty"`
	TerminalCols          uint16            `json:"terminal_cols,omitempty"`
	TerminalRows          uint16            `json:"terminal_rows,omitempty"`
	Slug                  string            `json:"slug,omitempty"`
	ConversationRef       string            `json:"conversation_file,omitempty"`
	RunnerVersion         string            `json:"runner_version,omitempty"`
	BinaryHash            string            `json:"binary_hash,omitempty"`
	ProjectSlug           string            `json:"project_slug,omitempty"`
	ProjectIndex          int               `json:"project_index,omitempty"`
	LastOutputAt          string            `json:"last_output_at,omitempty"`
}

type SessionStatus struct {
	Active      bool `json:"active"`
	Error       bool `json:"error,omitempty"`
	Interrupted bool `json:"interrupted,omitempty"`
}

func cloneProjection(s SessionProjection) SessionProjection {
	s.Command = append([]string(nil), s.Command...)
	if s.Remotes != nil {
		s.Remotes = cloneStrings(s.Remotes)
	}
	if s.ExitCode != nil {
		v := *s.ExitCode
		s.ExitCode = &v
	}
	if s.Status != nil {
		v := *s.Status
		s.Status = &v
	}
	return s
}
func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ProjectionSink preserves legacy production effects without making Manager
// depend on their authority. Calls happen after the manager cache is updated.
type ProjectionSink interface {
	ReplacePeerSessions(peer string, sessions []SessionProjection)
	RemovePeerSessions(peer string)
	SessionActivity(id string)
	PeerWorldChanged(name string)
	AliveSessionCount(peer string) int
}

type EventHooks struct {
	PeerWorldDirty    func()
	PeerSessionsDirty func()
	// SessionActivity forwards the lossy protocol activity pulse without
	// requiring the legacy ProjectionSink authority.
	SessionActivity       func(string)
	LocalPeerConnected    func(string, []SessionProjection)
	LocalPeerDisconnected func(string)
}

// projectionsEqual reports whether two peer session projections describe
// the same runtime facts. It is the no-op gate for snapshot replacement,
// so it is deliberately:
//
//   - order-insensitive (keyed by ID): a peer reordering an unchanged set
//     must not read as a change, or the reciprocal link never quiesces;
//   - symmetric about duplicate IDs: a repeated ID on *either* side means the
//     slices no longer describe the same ID set even at equal length, so the
//     comparison degrades to "changed" rather than silently matching two new
//     rows against one cached row;
//   - nil/empty-insensitive for Command and Remotes: encoders differ on
//     `null` vs `[]`/`{}` and the distinction carries no meaning;
//   - value-comparing for the *int / *SessionStatus pointers, since the
//     rows are freshly cloned on every delivery.
//
// Everything else is compared exactly. When in doubt it must return false:
// a false negative costs one redundant broadcast, a false positive hides a
// real update forever (nothing else re-drives peer projections).
func projectionsEqual(a, b []SessionProjection) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]*SessionProjection, len(a))
	for i := range a {
		if _, dup := index[a[i].ID]; dup {
			return false // duplicate IDs: fall back to "changed"
		}
		index[a[i].ID] = &a[i]
	}
	seen := make(map[string]struct{}, len(b))
	for i := range b {
		if _, dup := seen[b[i].ID]; dup {
			return false // duplicate IDs: fall back to "changed"
		}
		seen[b[i].ID] = struct{}{}
		prev, ok := index[b[i].ID]
		if !ok || !projectionEqual(*prev, b[i]) {
			return false
		}
	}
	return true
}

// worldRelevantSessionChange reports whether a peer projection replacement can
// move a field the world snapshot derives from peer sessions: the per-peer
// alive session count (world.go) and the local-peer presence set keyed by
// session ID. It is deliberately narrower than projectionsEqual — pure
// metadata churn (title, activity timestamps) belongs to the sessions kind
// only, and promoting it to a world recompose would put avoidable world
// frames (and therefore peer /v1/projects fetches) back on the wire.
//
// Row counts are not a proxy for ID-set size: a snapshot may repeat an ID, in
// which case the same number of rows carries a *smaller* set and
// PeerWorld.LocalPeerSessions (keyed by ID) really does move. Both sides are
// therefore compared by distinct-ID count as well as by membership.
func worldRelevantSessionChange(prev, next []SessionProjection) bool {
	if len(prev) != len(next) {
		return true
	}
	aliveBefore, aliveAfter := 0, 0
	ids := make(map[string]struct{}, len(prev))
	for i := range prev {
		if prev[i].Alive {
			aliveBefore++
		}
		ids[prev[i].ID] = struct{}{}
	}
	nextIDs := make(map[string]struct{}, len(next))
	for i := range next {
		if next[i].Alive {
			aliveAfter++
		}
		nextIDs[next[i].ID] = struct{}{}
		if _, ok := ids[next[i].ID]; !ok {
			return true
		}
	}
	if len(ids) != len(nextIDs) {
		return true // duplicate IDs collapsed (or un-collapsed) the ID set
	}
	return aliveBefore != aliveAfter
}

func projectionEqual(x, y SessionProjection) bool {
	if !equalStringSlice(x.Command, y.Command) || !equalStringMap(x.Remotes, y.Remotes) {
		return false
	}
	if !equalIntPtr(x.ExitCode, y.ExitCode) || !equalStatus(x.Status, y.Status) {
		return false
	}
	// Compare the remaining scalar fields by zeroing the ones handled above
	// so a newly added field is included by default (fail-open to "changed"
	// only if it differs, never silently ignored).
	x.Command, y.Command = nil, nil
	x.Remotes, y.Remotes = nil, nil
	x.ExitCode, y.ExitCode = nil, nil
	x.Status, y.Status = nil, nil
	return reflect.DeepEqual(x, y)
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalStatus(a, b *SessionStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
