package store

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Session matches the schema v2 model served by gmux-run's GET /meta.
type Session struct {
	ID           string   `json:"id"`
	CreatedAt    string   `json:"created_at,omitempty"`
	Command      []string `json:"command,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	Kind         string   `json:"kind"`
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
	CloseAction  string   `json:"close_action,omitempty"`
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
}

func New() *Store {
	return &Store{
		sessions:    make(map[string]Session),
		subscribers: make(map[*subscriber]struct{}),
	}
}

// SetResumableKinds configures which adapter kinds support resume.
// Derived from the compiled adapter set at startup.
func (s *Store) SetResumableKinds(kinds map[string]bool) {
	s.resumableKinds = kinds
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

// resolveTitle picks the best title: adapter > shell > command basename.
func resolveTitle(sess Session) string {
	if sess.AdapterTitle != "" {
		return sess.AdapterTitle
	}
	if sess.ShellTitle != "" {
		return sess.ShellTitle
	}
	// Fall back to command basename (same logic the runner uses).
	if len(sess.Command) > 0 {
		base := filepath.Base(sess.Command[0])
		if len(sess.Command) > 1 {
			parts := make([]string, len(sess.Command))
			parts[0] = base
			copy(parts[1:], sess.Command[1:])
			return strings.Join(parts, " ")
		}
		return base
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
	sess.Title = resolveTitle(sess)
	resumeKind := s.isResumableKind(sess.Kind)
	// A session is resumable only if it has an attributed file (ResumeKey).
	// Without a file, there's nothing to resume — the original command
	// would just start a fresh session.
	hasFile := sess.ResumeKey != ""
	sess.Resumable = !sess.Alive && resumeKind && hasFile && len(sess.Command) > 0
	// close_action:
	//   alive + resume-capable kind + has file → minimize (−)
	//   everything else → dismiss (×)
	// Before file attribution, even resumable-kind sessions get × because
	// killing them produces nothing to resume.
	if sess.Alive && resumeKind && hasFile {
		sess.CloseAction = "minimize"
	} else {
		sess.CloseAction = "dismiss"
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	s.broadcast(Event{
		Type:    "session-upsert",
		ID:      sess.ID,
		Session: &sess,
	})
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
