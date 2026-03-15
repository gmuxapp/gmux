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
	Alive         bool     `json:"alive"`
	Pid           int      `json:"pid,omitempty"`
	ExitCode      *int     `json:"exit_code,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	ExitedAt      string   `json:"exited_at,omitempty"`
	Title         string   `json:"title,omitempty"`
	Subtitle      string   `json:"subtitle,omitempty"`
	Status        *Status  `json:"status"`
	Unread        bool     `json:"unread"`
	Resumable     bool     `json:"resumable,omitempty"`
	ResumeKey     string   `json:"resume_key,omitempty"`
	CloseAction   string   `json:"close_action,omitempty"`
	SocketPath    string   `json:"socket_path,omitempty"`
	ResizeOwnerID string   `json:"resize_owner_id,omitempty"`
	TerminalCols  uint16   `json:"terminal_cols,omitempty"`
	TerminalRows  uint16   `json:"terminal_rows,omitempty"`
}

// Status is the application-reported status.
type Status struct {
	Label   string `json:"label"`
	Working bool   `json:"working"`
}

type Event struct {
	Type string `json:"type"` // session-upsert, session-remove
	ID   string `json:"id"`

	// Present for session-upsert
	Session *Session `json:"session,omitempty"`
}

type subscriber struct {
	ch chan Event
}

type Store struct {
	mu          sync.RWMutex
	sessions    map[string]Session
	subscribers map[*subscriber]struct{}
}

func New() *Store {
	return &Store{
		sessions:    make(map[string]Session),
		subscribers: make(map[*subscriber]struct{}),
	}
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

func (s *Store) Upsert(sess Session) {
	// Derive close_action:
	//   alive + resume-capable kind → minimize (−) — killing will yield a resumable session
	//   everything else → dismiss (×) — remove from store
	if sess.Alive && sess.Kind != "" && sess.Kind != "shell" && sess.Kind != "generic" {
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

func (s *Store) SetResizeState(id, deviceID string, cols, rows uint16) bool {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	sess.ResizeOwnerID = deviceID
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
