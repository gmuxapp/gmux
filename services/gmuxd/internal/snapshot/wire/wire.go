// Package wire is the sole consumer-facing conversion from composer batches
// (internal/snapshot/central) — plus the runtime/verdict overlays already
// riding them and the peer-session overlay supplied here — to the existing
// ADR 0001 wire shapes: the snapshot.sessions / snapshot.world SSE payloads
// and the GET /v1/sessions JSON. It is the only place that knows both the
// central-store shapes and the wire shapes (design-cutover §3); the SSE
// handler and the one-shot routes both consume it, so pushed snapshots and
// one-shot reads cannot diverge.
//
// Conversion policy pinned here (all derivation, never persisted):
//
//   - Title via the production resolveTitle precedence
//     (internal/store/store.go resolveTitle): adapter title > shell title >
//     CommandTitler(command) > adapter name.
//   - Timestamps: durable Unix-ms → RFC3339 at second precision (FD-4).
//   - Resume-command rewriting for dead rows as a pure function of
//     (adapter, conversation ref); the durable row keeps the launch
//     command (design §3.1 — one function, two call sites, zero
//     persistence).
//   - Resumable narrowing: dead ∧ command present ∧ verdict ≠ Gone (the
//     composer's overlay), recomputed against the rewritten command.
//   - project_index: FD-1 flatten of the durable scoped ordering into the
//     legacy per-project flat integer.
package wire

import (
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// Status is the application-reported status on the wire. Mirrors
// store.Status. Rows whose runner never reported a status carry a nil
// Status (wire "status": null), exactly like production — the durable
// status_reported fact preserves the distinction gmux wait's died/idle
// verdict depends on (ADR 0023; review fable M-1).
type Status struct {
	Working bool `json:"working"`
	Error   bool `json:"error,omitempty"`
}

// Session is the exact ADR 0001 session wire shape — field-for-field the
// wire struct inside store.Session.MarshalJSON (internal/store/store.go).
// It is also the shape of a peer-session projection as received from a
// peer's own snapshot.sessions feed, which is why the peer overlay
// (PeerSessionSource) traffics in this type verbatim.
type Session struct {
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
	LastActivityAt  string            `json:"last_activity_at,omitempty"`
}

// SessionsPayload is the body of a snapshot.sessions SSE event and the
// data of GET /v1/sessions. Sessions are sorted by ID (deterministic wire
// bytes, ADR 0001).
type SessionsPayload struct {
	Sessions []Session `json:"sessions"`
}

// FilterOwned returns the payload narrowed to the ?as=peer subscriber
// view: own sessions plus Local-peer (devcontainer) sessions; network-peer
// mirrors are excluded (ADR 0002 — a peer reconciles those from their
// origin directly).
func (p SessionsPayload) FilterOwned(isLocalPeer func(string) bool) SessionsPayload {
	out := make([]Session, 0, len(p.Sessions))
	for _, s := range p.Sessions {
		if s.Peer == "" || (isLocalPeer != nil && isLocalPeer(s.Peer)) {
			out = append(out, s)
		}
	}
	return SessionsPayload{Sessions: out}
}

// MatchRule mirrors projects.MatchRule on the wire.
type MatchRule struct {
	Path   string `json:"path,omitempty"`
	Remote string `json:"remote,omitempty"`
	Exact  bool   `json:"exact,omitempty"`
}

// ProjectItem mirrors the projects.Item JSON shape served inside
// snapshot.world and GET /v1/projects. Sessions[] is rebuilt from durable
// placements in FD-1 flatten order: local sessions keyed by ID, Local-peer
// sessions by their namespaced "id@peer" key. NodeID rides references
// verbatim from the durable catalog (ADR 0017 liveness anchor; review
// fable H-1 — persisted, not dropped).
type ProjectItem struct {
	Slug     string      `json:"slug"`
	Peer     string      `json:"peer,omitempty"`
	Match    []MatchRule `json:"match,omitempty"`
	Sessions []string    `json:"sessions,omitempty"`
	NodeID   string      `json:"node_id,omitempty"`
}

// WorldPayload is the body of a snapshot.world SSE event — the typed
// successor of snapshot.WorldPayload with identical JSON keys (checklist 4
// type tightening: every field is concrete).
type WorldPayload struct {
	Projects        []ProjectItem                        `json:"projects"`
	Peers           []peering.PeerInfo                   `json:"peers"`
	Health          *central.HealthInfo                  `json:"health"`
	Launchers       []peering.LauncherDef                `json:"launchers"`
	DefaultLauncher string                               `json:"default_launcher"`
	PeerProjects    map[string][]peering.SpokeProject    `json:"peer_projects,omitempty"`
	PeerDiscovered  map[string][]peering.SpokeDiscovered `json:"peer_discovered,omitempty"`
}
