// Package session holds the in-memory session state for a single gmux-run
// instance. This is the source of truth — served via /meta and /events.
package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// State is the in-memory session state served by GET /meta.
type State struct {
	mu sync.RWMutex

	// Core identity (immutable after creation)
	ID             string   `json:"id"`
	CreatedAt      string   `json:"created_at"`
	Command        []string `json:"command"`
	Cwd            string   `json:"cwd"`
	Kind           string   `json:"kind"`
	WorkspaceRoot  string            `json:"workspace_root,omitempty"`
	Remotes        map[string]string `json:"remotes,omitempty"`

	// Process state (owned by runner)
	Alive     bool   `json:"alive"`
	Pid       int    `json:"pid"`
	ExitCode  *int   `json:"exit_code"`
	StartedAt string `json:"started_at"`
	ExitedAt  string `json:"exited_at,omitempty"`

	// Title sources. Display title is resolved: adapter > shell > command basename.
	ShellTitle   string `json:"shell_title,omitempty"`
	AdapterTitle string `json:"adapter_title,omitempty"`

	// Other display fields
	Subtitle string          `json:"subtitle,omitempty"`
	Status   *adapter.Status `json:"status"`
	Unread   bool            `json:"unread"`

	// Slug is an adapter-provided stable identifier for URL routing.
	Slug string `json:"slug,omitempty"`

	// Terminal size (updated by the runner whenever PTY is resized).
	TerminalCols uint16 `json:"terminal_cols,omitempty"`
	TerminalRows uint16 `json:"terminal_rows,omitempty"`

	// Transport
	SocketPath string `json:"socket_path"`

	// Build identity
	BinaryHash    string `json:"binary_hash,omitempty"`
	RunnerVersion string `json:"runner_version,omitempty"`

	// SSE subscribers (not serialized)
	subs []chan Event

	// Throttle for activity events — at most one per interval.
	lastActivity time.Time
}

// Event is sent over SSE to /events subscribers.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Config for creating a new session state.
type Config struct {
	ID            string
	Command       []string
	Cwd           string
	Kind          string
	SocketPath    string
	BinaryHash    string
	RunnerVersion string
	WorkspaceRoot string
	Remotes       map[string]string
}

// New creates a new session state.
func New(cfg Config) *State {
	return &State{
		ID:            cfg.ID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Command:       cfg.Command,
		Cwd:           cfg.Cwd,
		Kind:          cfg.Kind,
		WorkspaceRoot: cfg.WorkspaceRoot,
		Remotes:       cfg.Remotes,
		SocketPath:    cfg.SocketPath,
		BinaryHash:    cfg.BinaryHash,
		RunnerVersion: cfg.RunnerVersion,
		Alive:         false,
	}
}

// Title returns the resolved display title: adapter > shell > command basename.
func (s *State) Title() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.titleLocked()
}

func (s *State) titleLocked() string {
	if s.AdapterTitle != "" {
		return s.AdapterTitle
	}
	if s.ShellTitle != "" {
		return s.ShellTitle
	}
	return commandBasename(s.Command)
}

func commandBasename(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	display := make([]string, len(cmd))
	copy(display, cmd)
	if strings.Contains(display[0], "/") {
		display[0] = filepath.Base(display[0])
	}
	return strings.Join(display, " ")
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
		return
	}
	s.Unread = unread
	s.emit(Event{Type: "meta", Data: map[string]any{"unread": unread}})
}

// activityThrottle is the minimum interval between activity events.
const activityThrottle = 2 * time.Second

// EmitActivity sends a lightweight "activity" event to signal that
// the terminal produced output. Throttled to at most once per 2s.
// This is not stored state — it's a transient signal for the frontend.
func (s *State) EmitActivity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.lastActivity) < activityThrottle {
		return
	}
	s.lastActivity = now
	s.emit(Event{Type: "activity", Data: nil})
}

// SetStatus updates the application status (from adapter or child).
func (s *State) SetStatus(status *adapter.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
	s.emit(Event{Type: "status", Data: status})
}

// SetAdapterTitle sets the high-priority title from the adapter/file monitor.
func (s *State) SetAdapterTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AdapterTitle == title {
		return
	}
	s.AdapterTitle = title
	s.emitMetaLocked()
}

// SetShellTitle sets the terminal/OSC title, used when no adapter title is set.
func (s *State) SetShellTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ShellTitle == title {
		return
	}
	s.ShellTitle = title
	s.emitMetaLocked()
}

// SetSlug sets the URL-safe session identifier.
func (s *State) SetSlug(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Slug == slug {
		return
	}
	s.Slug = slug
	s.emit(Event{Type: "meta", Data: map[string]string{"slug": slug}})
}

// SetSubtitle updates the display subtitle.
func (s *State) SetSubtitle(subtitle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Subtitle = subtitle
	s.emitMetaLocked()
}

// SetTerminalSize records the current PTY dimensions and emits a terminal_resize
// event so gmuxd discovery can update the store without relying on the proxy.
func (s *State) SetTerminalSize(cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TerminalCols = cols
	s.TerminalRows = rows
	s.emit(Event{Type: "terminal_resize", Data: map[string]uint16{
		"cols": cols,
		"rows": rows,
	}})
}

func (s *State) emitMetaLocked() {
	data := map[string]string{
		"title":       s.titleLocked(),
		"shell_title": s.ShellTitle,
	}
	if s.AdapterTitle != "" {
		data["adapter_title"] = s.AdapterTitle
	}
	if s.Subtitle != "" {
		data["subtitle"] = s.Subtitle
	}
	s.emit(Event{Type: "meta", Data: data})
}

// MarshalJSON produces JSON with a computed "title" field.
func (s *State) MarshalJSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type Alias State
	return json.Marshal(&struct {
		Title string `json:"title"`
		*Alias
	}{
		Title: s.titleLocked(),
		Alias: (*Alias)(s),
	})
}

// Subscribe returns a channel that receives events.
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

func (s *State) emit(e Event) {
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
