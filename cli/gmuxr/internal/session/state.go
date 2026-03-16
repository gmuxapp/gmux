// Package session holds the in-memory session state for a single gmux-run
// instance. This is the source of truth — served via /meta and /events.
// Replaces the file-based metadata package.
package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// State is the in-memory session state served by GET /meta.
// Fields follow session-schema-v2.
type State struct {
	mu sync.RWMutex

	// Core identity (immutable after creation)
	ID        string   `json:"id"`
	CreatedAt string   `json:"created_at"`
	Command   []string `json:"command"`
	Cwd       string   `json:"cwd"`
	Kind      string   `json:"kind"` // adapter name

	// Process state (owned by runner)
	Alive     bool   `json:"alive"`
	Pid       int    `json:"pid"`
	ExitCode  *int   `json:"exit_code"`
	StartedAt string `json:"started_at"`
	ExitedAt  string `json:"exited_at,omitempty"`

	// Display (set by adapter or child)
	Title    string          `json:"title"`
	Subtitle string          `json:"subtitle,omitempty"`
	Status   *adapter.Status `json:"status"`
	Unread   bool            `json:"unread"`

	// Transport
	SocketPath string `json:"socket_path"`

	// Build identity — sha256 of the gmuxr binary, computed once at startup.
	// Used by gmuxd to detect stale runners from a different build.
	BinaryHash string `json:"binary_hash,omitempty"`

	// Subscribers for /events SSE
	subs []chan Event

	// Title precedence state (not serialized):
	// adapter title > shell/OSC title > base title.
	baseTitle    string `json:"-"`
	shellTitle   string `json:"-"`
	adapterTitle string `json:"-"`
}

// Event is sent over SSE to /events subscribers.
type Event struct {
	Type string      `json:"type"` // "status", "meta", "exit"
	Data interface{} `json:"data"`
}

// Config for creating a new session state.
type Config struct {
	ID          string
	Command     []string
	Cwd         string
	Kind        string
	SocketPath  string
	Title       string
	TitlePinned bool // true for explicit/user-provided titles that should beat shell fallback
	BinaryHash  string
}

// New creates a new session state.
func New(cfg Config) *State {
	now := time.Now().UTC().Format(time.RFC3339)
	adapterTitle := ""
	if cfg.TitlePinned {
		adapterTitle = cfg.Title
	}
	return &State{
		ID:           cfg.ID,
		CreatedAt:    now,
		Command:      cfg.Command,
		Cwd:          cfg.Cwd,
		Kind:         cfg.Kind,
		SocketPath:   cfg.SocketPath,
		Title:        cfg.Title,
		BinaryHash:   cfg.BinaryHash,
		Alive:        false,
		baseTitle:    cfg.Title,
		adapterTitle: adapterTitle,
	}
}

// SetRunning marks the session as alive with the given PID.
func (s *State) SetRunning(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Alive = true
	s.Pid = pid
	s.StartedAt = time.Now().UTC().Format(time.RFC3339)
}

// SetExited marks the session as dead with the given exit code.
func (s *State) SetExited(exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Alive = false
	s.ExitCode = &exitCode
	s.ExitedAt = time.Now().UTC().Format(time.RFC3339)
	s.emit(Event{Type: "exit", Data: map[string]int{"exit_code": exitCode}})
}

// SetUnread marks the session as having unseen output (or clears it).
func (s *State) SetUnread(unread bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Unread == unread {
		return // no change, avoid noisy events
	}
	s.Unread = unread
	s.emit(Event{Type: "meta", Data: map[string]any{"unread": unread}})
}

// SetStatus updates the application status (from adapter or child).
func (s *State) SetStatus(status *adapter.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
	s.emit(Event{Type: "status", Data: status})
}

func (s *State) resolvedTitleLocked() string {
	if s.adapterTitle != "" {
		return s.adapterTitle
	}
	if s.shellTitle != "" {
		return s.shellTitle
	}
	return s.baseTitle
}

func (s *State) emitMetaLocked() {
	s.emit(Event{Type: "meta", Data: map[string]string{"title": s.Title, "subtitle": s.Subtitle}})
}

// SetAdapterTitle updates the high-priority adapter title. Empty clears it,
// allowing shell title (if any) to become visible again.
func (s *State) SetAdapterTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapterTitle = title
	resolved := s.resolvedTitleLocked()
	if resolved == s.Title {
		return
	}
	s.Title = resolved
	s.emitMetaLocked()
}

// SetShellTitle updates the fallback shell/OSC title. It is only visible when
// no adapter title is currently set.
func (s *State) SetShellTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shellTitle = title
	resolved := s.resolvedTitleLocked()
	if resolved == s.Title {
		return
	}
	s.Title = resolved
	s.emitMetaLocked()
}

// SetTitle updates the display title.
// Deprecated: use SetAdapterTitle or SetShellTitle so precedence is preserved.
func (s *State) SetTitle(title string) { s.SetAdapterTitle(title) }

// SetSubtitle updates the display subtitle.
func (s *State) SetSubtitle(subtitle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Subtitle = subtitle
	s.emitMetaLocked()
}

// PatchMeta updates title and/or subtitle from a partial update.
// Title patches are treated as adapter titles (higher priority than shell titles).
// An empty title clears the adapter title, revealing the shell or base title.
func (s *State) PatchMeta(title, subtitle *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if title != nil {
		s.adapterTitle = *title
		s.Title = s.resolvedTitleLocked()
	}
	if subtitle != nil {
		s.Subtitle = *subtitle
	}
	s.emitMetaLocked()
}

// JSON returns the full state as JSON bytes.
func (s *State) JSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.Marshal(s)
}

// Subscribe returns a channel that receives events. The caller must
// call Unsubscribe when done.
func (s *State) Subscribe() chan Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, 16)
	s.subs = append(s.subs, ch)
	return ch
}

// Unsubscribe removes a subscription channel.
func (s *State) Unsubscribe(ch chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sub := range s.subs {
		if sub == ch {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// emit sends an event to all subscribers (must be called under write lock).
func (s *State) emit(e Event) {
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
			// Drop if subscriber is slow — SSE will recover
		}
	}
}
