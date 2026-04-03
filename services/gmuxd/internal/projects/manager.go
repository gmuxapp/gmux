package projects

import (
	"log"
	"sync"
)

// Manager provides concurrent access to project state and handles
// auto-assignment of sessions to projects. All mutations go through
// Manager to ensure atomic load+modify+save.
type Manager struct {
	mu       sync.Mutex
	stateDir string

	// Broadcast is called after every state mutation that should be
	// synced to connected clients (via SSE). Set by the caller.
	Broadcast func()
}

func NewManager(stateDir string) *Manager {
	return &Manager{stateDir: stateDir}
}

// Load returns the current project state. Thread-safe.
func (m *Manager) Load() (*State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Load(m.stateDir)
}

// Update atomically loads state, calls fn to modify it, validates, and saves.
// If fn returns false, the update is aborted (no save, no broadcast).
func (m *Manager) Update(fn func(s *State) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := Load(m.stateDir)
	if err != nil {
		return err
	}

	if !fn(state) {
		return nil // aborted by fn
	}

	if err := state.Save(m.stateDir); err != nil {
		return err
	}

	if m.Broadcast != nil {
		m.Broadcast()
	}
	return nil
}

// AutoAssignSession checks if a session matches a project and adds it
// to that project's sessions list. Returns the project slug if assigned.
// This is called when:
//   - A new session is discovered (Register)
//   - A session gets a ResumeKey (file attribution)
func (m *Manager) AutoAssignSession(info SessionInfo) string {
	var assigned string
	err := m.Update(func(state *State) bool {
		key := SessionKey(info.ID, info.ResumeKey)

		// Already in a project?
		if state.FindSessionProject(key) != "" {
			return false
		}

		// If the session has a ResumeKey different from its ID, check if
		// the old key (session ID) is already assigned. This handles the
		// transition when a session gets attributed: replace the ID-based
		// entry with the ResumeKey-based entry to preserve ordering.
		if info.ResumeKey != "" && info.ResumeKey != info.ID {
			if slug := state.FindSessionProject(info.ID); slug != "" {
				// Replace ID with ResumeKey in the same position.
				for i := range state.Items {
					if state.Items[i].Slug != slug {
						continue
					}
					for j, existing := range state.Items[i].Sessions {
						if existing == info.ID {
							state.Items[i].Sessions[j] = info.ResumeKey
							assigned = slug
							return true
						}
					}
				}
			}
		}

		// Match against project rules.
		match := state.Match(info.Cwd, info.WorkspaceRoot, info.Remotes)
		if match == nil {
			return false
		}

		state.AddSession(match.Slug, key)
		assigned = match.Slug
		return true
	})
	if err != nil {
		log.Printf("projects: auto-assign error: %v", err)
	}
	return assigned
}

// DismissSession removes a session from its project's sessions list.
// Returns the project slug if the session was found.
func (m *Manager) DismissSession(id, resumeKey string) string {
	var removed string
	err := m.Update(func(state *State) bool {
		key := SessionKey(id, resumeKey)
		slug := state.RemoveSessionFromAll(key)
		if slug == "" {
			// Also try the ID if we used resumeKey.
			if resumeKey != "" && resumeKey != id {
				slug = state.RemoveSessionFromAll(id)
			}
		}
		if slug != "" {
			removed = slug
			return true
		}
		return false
	})
	if err != nil {
		log.Printf("projects: dismiss error: %v", err)
	}
	return removed
}
