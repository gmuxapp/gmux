package store

import (
	"sync"
	"time"
)

// Session matches the schema v2 model served by gmux-run's GET /meta.
type Session struct {
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at,omitempty"`
	Command       []string `json:"command,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	Kind          string   `json:"kind"`
	WorkspaceRoot string   `json:"workspace_root,omitempty"`
	Alive        bool     `json:"alive"`
	Pid          int      `json:"pid,omitempty"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	StartedAt    string   `json:"started_at,omitempty"`
	ExitedAt     string   `json:"exited_at,omitempty"`
	Title        string   `json:"title,omitempty"`
	ShellTitle   string   `json:"shell_title,omitempty"`
	AdapterTitle string   `json:"adapter_title,omitempty"`
	Subtitle     string   `json:"subtitle,omitempty"`
	Status       *Status  `json:"status"`
	Unread       bool     `json:"unread"`
	Resumable    bool     `json:"resumable,omitempty"`
	ResumeKey    string   `json:"resume_key,omitempty"`
	SocketPath   string   `json:"socket_path,omitempty"`
	TerminalCols uint16   `json:"terminal_cols,omitempty"`
	TerminalRows uint16   `json:"terminal_rows,omitempty"`

	// Build identity — sha256 of the gmux binary that owns this session.
	// Populated from the runner's /meta `binary_hash` field.
	BinaryHash string `json:"binary_hash,omitempty"`
	// Stale is true when BinaryHash differs from gmuxd's expected runner hash.
	// Indicates the session was started by a different build of gmux.
	Stale bool `json:"stale,omitempty"`
}

// Status is the application-reported status.
type Status struct {
	Label   string `json:"label"`
	Working bool   `json:"working"`
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
	mu              sync.RWMutex
	sessions        map[string]Session
	subscribers     map[*subscriber]struct{}
	resumableKinds  map[string]bool
	commandTitlers  map[string]func([]string) string
	dismissed       map[string]bool // dismissed ResumeKeys — prevents scanner re-adding
}

func New() *Store {
	return &Store{
		sessions:    make(map[string]Session),
		subscribers: make(map[*subscriber]struct{}),
		dismissed:   make(map[string]bool),
	}
}

// SetResumableKinds configures which adapter kinds support resume.
// Derived from the compiled adapter set at startup.
func (s *Store) SetResumableKinds(kinds map[string]bool) {
	s.resumableKinds = kinds
}

// SetCommandTitlers configures per-kind functions that derive a display
// title from a command array. Used as the fallback when no adapter_title
// or shell_title is set (e.g. "codex" instead of "codex resume <id>").
func (s *Store) SetCommandTitlers(titlers map[string]func([]string) string) {
	s.commandTitlers = titlers
}

// IsDismissed returns true if a resume key was previously dismissed by the user.
func (s *Store) IsDismissed(resumeKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dismissed[resumeKey]
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

func (s *Store) isResumableKind(kind string) bool {
	if s.resumableKinds == nil {
		return false
	}
	return s.resumableKinds[kind]
}

func (s *Store) Upsert(sess Session) {
	sess.Title = s.resolveTitle(sess)
	resumeKind := s.isResumableKind(sess.Kind)
	// A session is resumable only if it has an attributed file (ResumeKey).
	// Without a file, there's nothing to resume — the original command
	// would just start a fresh session.
	hasFile := sess.ResumeKey != ""
	sess.Resumable = !sess.Alive && resumeKind && hasFile && len(sess.Command) > 0
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

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
	sess.Title = s.resolveTitle(sess)
	resumeKind := s.isResumableKind(sess.Kind)
	hasFile := sess.ResumeKey != ""
	sess.Resumable = !sess.Alive && resumeKind && hasFile && len(sess.Command) > 0
	s.sessions[id] = sess
	s.mu.Unlock()

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

func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		if sess.ResumeKey != "" {
			s.dismissed[sess.ResumeKey] = true
		}
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
