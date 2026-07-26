package statetool

import (
	"context"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ExportDoc is the deterministic redacted JSON document served by
// GET /v1/state/export: sorted session IDs, catalog order, sorted peer
// names, stable key order via struct marshaling. Tokens are redacted to
// presence bits (ManualPeer.Redacted) and every URL-bearing field is
// scrubbed of userinfo. There is deliberately no with-secrets variant —
// the raw backup is the secret-bearing artifact (design §5).
type ExportDoc struct {
	SchemaVersion int64             `json:"schema_version"`
	Sessions      []ExportSession   `json:"sessions"`
	Projects      []ExportProject   `json:"projects"`
	Placements    []ExportPlacement `json:"placements"`
	Peers         []ExportPeer      `json:"peers"`
}

// ExportSession is the durable projection of one local session row,
// dismissed rows included (hidden-not-forgotten). Timestamps stay Unix-ms
// integers: export is a diagnostic inventory, not the ADR 0001 wire shape.
type ExportSession struct {
	ID              string            `json:"id"`
	Adapter         string            `json:"adapter"`
	ConversationRef string            `json:"conversation_ref,omitempty"`
	Command         []string          `json:"command"`
	CWD             string            `json:"cwd"`
	WorkspaceRoot   string            `json:"workspace_root,omitempty"`
	Remotes         map[string]string `json:"remotes"`
	Slug            string            `json:"slug,omitempty"`
	ShellTitle      string            `json:"shell_title,omitempty"`
	AdapterTitle    string            `json:"adapter_title,omitempty"`
	Subtitle        string            `json:"subtitle,omitempty"`
	Working         bool              `json:"working"`
	Unread          bool              `json:"unread"`
	Error           bool              `json:"error"`
	StatusReported  bool              `json:"status_reported"`
	CreatedAtMs     int64             `json:"created_at_ms"`
	StartedAtMs     *int64            `json:"started_at_ms,omitempty"`
	ExitedAtMs      *int64            `json:"exited_at_ms,omitempty"`
	LastActivityMs  *int64            `json:"last_activity_at_ms,omitempty"`
	DismissedAtMs   *int64            `json:"dismissed_at_ms,omitempty"`
	ExitCode        *int              `json:"exit_code,omitempty"`
	TerminalCols    *uint16           `json:"terminal_cols,omitempty"`
	TerminalRows    *uint16           `json:"terminal_rows,omitempty"`
	LaunchParentID  string            `json:"launch_parent_id,omitempty"`
	PromotedToRoot  bool              `json:"promoted_to_root"`
}

// ExportProject is one catalog entry in sidebar order.
type ExportProject struct {
	Kind    string       `json:"kind"`
	Slug    string       `json:"slug"`
	PeerKey string       `json:"peer_key,omitempty"`
	NodeID  string       `json:"node_id,omitempty"`
	Rules   []ExportRule `json:"rules,omitempty"`
}

// ExportRule is one project match rule.
type ExportRule struct {
	Path   string `json:"path,omitempty"`
	Remote string `json:"remote,omitempty"`
	Exact  bool   `json:"exact,omitempty"`
}

// ExportPlacement is one placement row joined to its project slug.
type ExportPlacement struct {
	ProjectSlug         string `json:"project_slug"`
	LocalSessionID      string `json:"local_session_id,omitempty"`
	LocalPeerKey        string `json:"local_peer_key,omitempty"`
	PeerSessionID       string `json:"peer_session_id,omitempty"`
	PeerParentSessionID string `json:"peer_parent_session_id,omitempty"`
	SiblingScope        string `json:"sibling_scope"`
	Position            int64  `json:"position"`
}

// ExportPeer is one manual peer, token redacted to a presence bit, URL
// scrubbed of userinfo.
type ExportPeer struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	NodeID       string `json:"node_id,omitempty"`
	TokenPresent bool   `json:"token_present"`
	CreatedAtMs  int64  `json:"created_at_ms"`
	UpdatedAtMs  int64  `json:"updated_at_ms"`
}

// Export builds the deterministic redacted export document.
func Export(ctx context.Context, store *centralstore.Store) (ExportDoc, error) {
	raw, err := store.ExportState(ctx)
	if err != nil {
		return ExportDoc{}, err
	}
	doc := ExportDoc{
		SchemaVersion: raw.SchemaVersion,
		Sessions:      make([]ExportSession, 0, len(raw.Sessions)),
		Projects:      make([]ExportProject, 0, len(raw.Catalog)),
		Placements:    make([]ExportPlacement, 0, len(raw.Placements)),
		Peers:         make([]ExportPeer, 0, len(raw.Peers)),
	}
	for _, s := range raw.Sessions {
		doc.Sessions = append(doc.Sessions, exportSession(s))
	}
	for _, e := range raw.Catalog {
		p := ExportProject{Kind: string(e.Kind), Slug: e.Slug, PeerKey: string(e.PeerKey), NodeID: e.NodeID}
		for _, r := range e.Rules {
			p.Rules = append(p.Rules, ExportRule{Path: r.Path, Remote: RedactURLUserinfo(r.Remote), Exact: r.Exact})
		}
		doc.Projects = append(doc.Projects, p)
	}
	for _, p := range raw.Placements {
		doc.Placements = append(doc.Placements, ExportPlacement{
			ProjectSlug:    p.ProjectSlug,
			LocalSessionID: p.LocalSessionID,
			LocalPeerKey:   p.LocalPeerKey, PeerSessionID: p.PeerSessionID,
			PeerParentSessionID: p.PeerParentSessionID,
			SiblingScope:        p.SiblingScope, Position: p.Position,
		})
	}
	for _, p := range raw.Peers {
		doc.Peers = append(doc.Peers, ExportPeer{
			Name: p.Name, URL: RedactURLUserinfo(p.URL), NodeID: p.NodeID,
			TokenPresent: p.TokenPresent,
			CreatedAtMs:  int64(p.CreatedAt), UpdatedAtMs: int64(p.UpdatedAt),
		})
	}
	return doc, nil
}

func exportSession(s centralstore.Session) ExportSession {
	out := ExportSession{
		ID: string(s.ID), Adapter: s.Adapter, ConversationRef: s.ConversationRef,
		Command: append([]string{}, s.Command...),
		CWD:     s.CWD, WorkspaceRoot: s.WorkspaceRoot,
		Remotes: map[string]string{},
		Slug:    s.Slug, ShellTitle: s.ShellTitle, AdapterTitle: s.AdapterTitle, Subtitle: s.Subtitle,
		Working: s.Working, Unread: s.Unread, Error: s.Error, StatusReported: s.StatusReported,
		CreatedAtMs:    int64(s.CreatedAt),
		StartedAtMs:    millis(s.StartedAt),
		ExitedAtMs:     millis(s.ExitedAt),
		LastActivityMs: millis(s.LastActivityAt),
		DismissedAtMs:  millis(s.DismissedAt),
		ExitCode:       s.ExitCode,
		TerminalCols:   s.TerminalCols, TerminalRows: s.TerminalRows,
		PromotedToRoot: s.PromotedToRoot,
	}
	// Remotes are URLs (git remotes may embed credentials): scrub each.
	for name, u := range s.Remotes {
		out.Remotes[name] = RedactURLUserinfo(u)
	}
	if s.LaunchParentID != nil {
		out.LaunchParentID = string(*s.LaunchParentID)
	}
	return out
}

func millis(v *centralstore.UnixMillis) *int64 {
	if v == nil {
		return nil
	}
	x := int64(*v)
	return &x
}
