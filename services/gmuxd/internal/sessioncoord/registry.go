// Package sessioncoord provides the nonproduction lifecycle boundary between
// runner streams and the authoritative central store. It is intentionally not
// wired into gmuxd.
package sessioncoord

import (
	"context"
	"sort"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// Runtime is the registry's runtime-only value. Durable common session facts
// must never be added here.
type Runtime struct {
	SessionID     centralstore.SessionID
	Generation    uint64
	Endpoint      string
	PID           int
	RunnerVersion string
	BinaryHash    string
	Subscribed    bool
	RowVersion    centralstore.RowVersion
}

type registryEntry struct {
	Runtime
	cancel context.CancelFunc
	stream EventStream
	// superseded marks the entry as fenced during a replacement's
	// commit-to-install window: the coordinator has committed (or is about to
	// commit) a replacement registration, so no observation for this
	// generation may reach the store anymore. current and advance treat
	// superseded entries as absent; remove still works so cleanup paths can
	// close the entry.
	superseded bool
}

// Registry stores liveness and delivery coordination only.
type Registry struct {
	mu      sync.RWMutex
	entries map[centralstore.SessionID]registryEntry
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[centralstore.SessionID]registryEntry)}
}

func (r *Registry) current(id centralstore.SessionID) (registryEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.superseded {
		return registryEntry{}, false
	}
	return e, true
}

// fenced reports whether the installed entry for id is currently superseded
// (a replacement is inside its commit-to-install window). Callers can use it
// to distinguish a fence from genuine absence.
func (r *Registry) fenced(id centralstore.SessionID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	return ok && e.superseded
}

// supersede fences the given generation so current/advance no longer see it.
// It returns false when the entry is absent or belongs to another generation.
func (r *Registry) supersede(id centralstore.SessionID, generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.Generation != generation {
		return false
	}
	e.superseded = true
	r.entries[id] = e
	return true
}

// restore lifts a supersede fence after a failed replacement so the old
// generation keeps operating. It is a no-op for absent or replaced entries.
func (r *Registry) restore(id centralstore.SessionID, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.Generation != generation {
		return
	}
	e.superseded = false
	r.entries[id] = e
}

func (r *Registry) install(e registryEntry) (old registryEntry, replaced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, replaced = r.entries[e.SessionID]
	r.entries[e.SessionID] = e
	return
}

func (r *Registry) advance(id centralstore.SessionID, generation uint64, version centralstore.RowVersion) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.Generation != generation || e.superseded {
		return false
	}
	e.RowVersion = version
	r.entries[id] = e
	return true
}

func (r *Registry) remove(id centralstore.SessionID, generation uint64) (registryEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.Generation != generation {
		return registryEntry{}, false
	}
	delete(r.entries, id)
	return e, true
}

// Snapshot returns a deep, copy-safe runtime-only view.
func (r *Registry) Snapshot() []Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Runtime, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Runtime)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

func closeEntry(e registryEntry) {
	if e.cancel != nil {
		e.cancel()
	}
	if e.stream != nil {
		_ = e.stream.Close()
	}
}
