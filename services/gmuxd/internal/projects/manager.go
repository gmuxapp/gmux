package projects

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// ValidationError wraps a State.Validate failure produced while applying
// a mutation. Callers (notably the HTTP layer) can detect it with
// errors.As to map the failure to a 4xx response rather than a 500: the
// request was well-formed but conflicts with existing state (e.g. a
// duplicate path), so nothing was persisted.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Manager provides concurrent access to project state and handles
// auto-assignment of sessions to projects. All mutations go through
// Manager to ensure atomic load+modify+save.
type Manager struct {
	mu             sync.Mutex
	stateDir       string
	legacySlugToID map[string]string

	// Broadcast is called after every state mutation that should be
	// synced to connected clients (via SSE). The caller receives the
	// just-saved State so it can derive related state (e.g.,
	// per-session project stamps via Reconcile) without re-Loading
	// (which would deadlock against the lock Update is holding).
	// Set by the caller; nil disables broadcast.
	Broadcast func(state *State)
}

// NewManager creates a manager. legacySlugToID is the slug-to-session-ID
// table from sessionmeta's startup sweep, used only while loading v3 files.
func NewManager(stateDir string, legacySlugToID ...map[string]string) *Manager {
	var resolver map[string]string
	if len(legacySlugToID) > 0 {
		resolver = legacySlugToID[0]
	}
	return &Manager{stateDir: stateDir, legacySlugToID: resolver}
}

// Load returns the current project state. Thread-safe.
func (m *Manager) Load() (*State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Load(m.stateDir, m.legacySlugToID)
}

// SeedIfEmpty creates a default "home" project when no projects exist.
// Called once at startup so new users see their sessions immediately
// instead of an empty sidebar. The user can remove or reorder it.
func (m *Manager) SeedIfEmpty() {
	err := m.Update(func(s *State) bool {
		if len(s.Items) > 0 {
			return false
		}
		s.Items = []Item{{
			Slug:  "home",
			Match: []MatchRule{{Path: "~", Exact: true}},
		}}
		log.Printf("projects: seeded default home project")
		return true
	})
	if err != nil {
		log.Printf("projects: seed error: %v", err)
	}
}

// AddProject appends a new owned project built from the given match
// rules, assigning a unique slug derived from baseSlug. The mutation is
// validated before it is persisted: if the resulting state is invalid
// (e.g. baseSlug's path duplicates an existing project), nothing is
// saved and a *ValidationError is returned. On success the created
// item (with its final, possibly deduplicated slug) is returned.
//
// This exists so the caller can tell "created" from "rejected": a bare
// Update returning false (abort) and returning true with invalid state
// would both surface as a nil error if Update did not validate.
// Reporting success on an aborted add let clients pin references to
// projects that were never created (see the dangling peer-reference
// bug). Update now validates centrally, so AddProject just contextualises
// the resulting *ValidationError with the requested slug.
func (m *Manager) AddProject(baseSlug string, rules []MatchRule) (Item, error) {
	var created Item
	err := m.Update(func(s *State) bool {
		created = Item{Slug: UniqueSlug(baseSlug, s.Items), Match: rules}
		s.Items = append(s.Items, created)
		return true
	})
	if err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			return Item{}, &ValidationError{Err: fmt.Errorf("project %q: %w", baseSlug, verr.Err)}
		}
		return Item{}, err
	}
	return created, nil
}

// Update atomically loads state, calls fn to modify it, validates, and
// saves. If fn returns false, the update is aborted (no save, no
// broadcast, nil error).
//
// Validation is the single chokepoint here, not in each caller: when fn
// returns true but leaves the state invalid, Update saves nothing and
// returns a *ValidationError. This makes a silently-discarded mutation
// impossible — every caller now sees a non-nil error instead of a
// success that persisted nothing. User-initiated callers (HTTP handlers)
// map it to a 4xx; background/reconcile callers log and drop it (their
// mutations only add/remove session keys, which Validate never rejects,
// so in practice they never hit this path).
func (m *Manager) Update(fn func(s *State) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := Load(m.stateDir, m.legacySlugToID)
	if err != nil {
		return err
	}

	if !fn(state) {
		return nil // aborted by fn
	}

	if err := state.Validate(); err != nil {
		return &ValidationError{Err: err}
	}

	if err := state.Save(m.stateDir); err != nil {
		return err
	}

	if m.Broadcast != nil {
		m.Broadcast(state)
	}
	return nil
}

// AutoAssignSession checks if a live session matches a project and adds it
// to that project's sessions list. Returns the project slug if assigned.
// This is called when:
//   - A new session is discovered (Register)
//
// Dead/resumable sessions are not auto-assigned. projects.json is the
// source of truth for sidebar membership; if dismiss removed a session key,
// a late exit/resume-command upsert must not add it back.
//
// Network-peer sessions (info.Host set, info.LocalHost false) are never
// written to the local projects.json: project membership for them is
// owned by their origin host (ADR 0002). Local-peer sessions
// (devcontainers, info.LocalHost = true) are an exception: the parent
// owns their assignment per the ADR 0002 amendment, so they flow
// through here like local sessions.
func (m *Manager) AutoAssignSession(info SessionInfo) string {
	if !info.Alive || (info.Host != "" && !info.LocalHost) {
		return ""
	}
	var assigned string
	err := m.Update(func(state *State) bool {
		key := info.ID

		// Already in a project?
		if state.FindSessionProject(key) != "" {
			return false
		}

		// Match against project rules.
		match := state.Match(matchParamsFromInfo(info))
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

// AutoAssignAll iterates all live sessions and adds them to their matching
// projects in a single atomic update. Called after adding a project so that
// existing live sessions populate the array immediately rather than waiting
// for the next session-upsert event.
//
// Dead/resumable sessions are skipped: sessionmeta owns their runtime state,
// but projects.json owns sidebar membership. If a key is already present in
// projects.json, reconciliation can still stamp the dead session; auto-assign
// just must not create new membership for dead sessions.
//
// Network-peer sessions are skipped; Local-peer sessions flow through
// because the parent owns their assignment. See AutoAssignSession.
func (m *Manager) AutoAssignAll(sessions []SessionInfo) {
	err := m.Update(func(state *State) bool {
		changed := false
		for _, info := range sessions {
			if !info.Alive {
				continue
			}
			if info.Host != "" && !info.LocalHost {
				continue
			}
			key := info.ID
			if state.FindSessionProject(key) != "" {
				continue
			}
			match := state.Match(matchParamsFromInfo(info))
			if match == nil {
				continue
			}
			state.AddSession(match.Slug, key)
			changed = true
		}
		return changed
	})
	if err != nil {
		log.Printf("projects: auto-assign-all error: %v", err)
	}
}

// PruneNamespacedKeys removes any session key ending in "@<peerName>"
// from every project's Sessions[]. Called when a Local peer
// (devcontainer) is destroyed: its namespaced ids will never resolve
// again and would accumulate as dead weight in the parent's
// projects.json. Reference items (peer-owned) are skipped: their
// content is the peer's business.
func (m *Manager) PruneNamespacedKeys(peerName string) {
	if peerName == "" {
		return
	}
	suffix := "@" + peerName
	err := m.Update(func(state *State) bool {
		changed := false
		for i := range state.Items {
			if state.Items[i].IsReference() {
				continue
			}
			filtered := state.Items[i].Sessions[:0]
			for _, key := range state.Items[i].Sessions {
				if len(key) > len(suffix) && key[len(key)-len(suffix):] == suffix {
					changed = true
					continue
				}
				filtered = append(filtered, key)
			}
			state.Items[i].Sessions = filtered
		}
		return changed
	})
	if err != nil {
		log.Printf("projects: prune namespaced keys (%s): %v", peerName, err)
	}
}

// CleanupSessions removes orphaned entries from all project session arrays.
// An entry is orphaned if its key doesn't appear in the known set. Call this
// after the initial session scan so the store has the full picture.
func (m *Manager) CleanupSessions(known map[string]bool) {
	err := m.Update(func(state *State) bool {
		changed := false
		for i := range state.Items {
			filtered := state.Items[i].Sessions[:0]
			for _, key := range state.Items[i].Sessions {
				if known[key] {
					filtered = append(filtered, key)
				} else {
					changed = true
				}
			}
			state.Items[i].Sessions = filtered
		}
		return changed
	})
	if err != nil {
		log.Printf("projects: cleanup error: %v", err)
	}
}

// DismissSession removes a session ID from its project's sessions list.
// Returns the project slug if the session was found.
func (m *Manager) DismissSession(id string) string {
	var removed string
	err := m.Update(func(state *State) bool {
		if projectSlug := state.RemoveSessionFromAll(id); projectSlug != "" {
			removed = projectSlug
			return true
		}
		return false
	})
	if err != nil {
		log.Printf("projects: dismiss error: %v", err)
	}
	return removed
}

// WatchRemovals removes project membership for every removed store session.
// Errors are logged so a failed cleanup does not stop the event loop.
func (m *Manager) WatchRemovals(events <-chan store.Event) {
	for ev := range events {
		if ev.Type != "session-remove" {
			continue
		}
		if err := m.RemoveSessionFromAll(ev.ID); err != nil {
			log.Printf("projects: cleanup remove %s: %v", ev.ID, err)
		}
	}
}

// RemoveSessionFromAll removes an ID from project membership.
func (m *Manager) RemoveSessionFromAll(id string) error {
	return m.Update(func(state *State) bool {
		return state.RemoveSessionFromAll(id) != ""
	})
}
