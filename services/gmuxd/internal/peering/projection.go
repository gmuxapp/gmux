package peering

// SessionProjection is the authority-neutral, copyable projection received
// from a peer's snapshot.sessions stream. It intentionally contains only
// wire/runtime facts; no durable store type crosses the peering boundary.
type SessionProjection struct {
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
	Status          *SessionStatus    `json:"status"`
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

type SessionStatus struct {
	Working bool `json:"working"`
	Error   bool `json:"error,omitempty"`
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
	PeerWorldDirty        func()
	PeerSessionsDirty     func()
	LocalPeerConnected    func(string, []SessionProjection)
	LocalPeerDisconnected func(string)
}
