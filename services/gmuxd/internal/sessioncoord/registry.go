// Package sessioncoord provides the nonproduction lifecycle boundary between
// runner streams and the authoritative central store. It is intentionally not
// wired into gmuxd.
package sessioncoord

import (
	"context"
	"sort"
	"sync"

	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// Runtime is the registry's runtime-only value. Durable common session facts
// must never be added here.
type Runtime struct {
	SessionID  centralstore.SessionID
	Generation uint64
	Endpoint   string
	// Socket is the physical identity (device + inode) the endpoint pathname
	// named while this generation registered. Endpoint alone cannot identify
	// a runner: a pathname is reusable, and a replacement runner binding the
	// same pathname is a different socket, a different process, and a
	// different generation, while the old one may still be draining.
	//
	// Consumers must treat an unknown (zero) identity as "cannot prove
	// anything": never as a match, and never as licence to suppress a probe
	// or to terminate a process. It is unknown when the endpoint is not a
	// filesystem socket (synthetic test endpoints) or when the pathname was
	// rebound while this registration was in flight, in which case the
	// registration's stream and the pathname's current inode may disagree.
	Socket socklease.Ident
	// Incarnation is the runner process that served this registration, as
	// reported by both its subscription and its metadata. Empty means the
	// runner could not be identified (it predates the protocol, or the
	// transport does not carry the field), which is never treated as a match.
	//
	// It is runtime-only and deliberately never persisted: it distinguishes a
	// process from its own successor at the same endpoint, a question that
	// only has meaning while both are in living memory.
	Incarnation string
	// Frame is the newest turn frame this generation relayed, or nil for a
	// generation that never sent one (a shell session, a hook-driven agent, a
	// version-skewed runner). It is runtime-only and shared by pointer: the
	// runner publishes immutable frame values, so retaining one costs one
	// pointer per generation however many waiters read it.
	//
	// nil is load-bearing: it means "this generation asserts no turn results",
	// which is what scopes the delivery invariant — such a close resolves
	// normally and result-free instead of hanging on a guarantee the runner
	// cannot satisfy.
	Frame         *TurnFrame
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
	// dead is closed exactly when this entry leaves the registry map (remove,
	// or replacement by install). Lifecycle operations wait on it to observe
	// this generation's death through the ordinary observation path.
	dead chan struct{}
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
	if replaced && old.dead != nil {
		close(old.dead)
	}
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
	if e.dead != nil {
		close(e.dead)
	}
	return e, true
}

func (r *Registry) removeAll() []registryEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]registryEntry, 0, len(r.entries))
	for id, e := range r.entries {
		delete(r.entries, id)
		if e.dead != nil {
			close(e.dead)
		}
		out = append(out, e)
	}
	return out
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

// InstalledSockets returns the set of physical socket identities that
// currently have an installed, subscribed, non-fenced generation. It is the
// authority for "this socket is already installed": the registry itself, not
// a cache maintained alongside it, so an entry that leaves the registry stops
// suppressing work in the same instant.
//
// Unknown identities are excluded. They prove nothing, and an unknown identity
// that suppressed a probe would hide every runner the daemon could not
// identify -- the opposite of the intent.
func (r *Registry) InstalledSockets() map[socklease.Ident]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[socklease.Ident]struct{}, len(r.entries))
	for _, e := range r.entries {
		if e.superseded || !e.Subscribed || !e.Socket.Known() {
			continue
		}
		out[e.Socket] = struct{}{}
	}
	return out
}

// liveSessionIDs returns the IDs of entries with an installed, non-fenced
// generation, sorted. Unlike Snapshot it excludes superseded (fenced)
// entries — a fenced entry is mid-replacement and must not be treated as
// live by callers that act on liveness (e.g. ReplaceCatalog's auto-assign
// pass).
func (r *Registry) liveSessionIDs() []centralstore.SessionID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]centralstore.SessionID, 0, len(r.entries))
	for id, e := range r.entries {
		if e.superseded {
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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
