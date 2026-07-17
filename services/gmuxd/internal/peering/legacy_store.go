package peering

import (
	"encoding/json"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// legacyStoreSink is selected only by the legacy constructors used by serve.
// It preserves the old store mutations while the central constructor remains
// write-free with respect to store.Store.
type legacyStoreSink struct{ store *store.Store }

func projectionSink(v any) ProjectionSink {
	switch x := v.(type) {
	case nil:
		return nil
	case ProjectionSink:
		return x
	case *store.Store:
		return legacyStoreSink{x}
	default:
		panic("peering: unsupported projection sink")
	}
}
func (s legacyStoreSink) ReplacePeerSessions(peer string, rows []SessionProjection) {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		b, _ := json.Marshal(row)
		var dst store.Session
		if json.Unmarshal(b, &dst) != nil {
			continue
		}
		seen[dst.ID] = true
		s.store.UpsertRemote(dst)
	}
	for _, row := range s.store.List() {
		if row.Peer == peer && !seen[row.ID] {
			s.store.Remove(row.ID)
		}
	}
}
func (s legacyStoreSink) RemovePeerSessions(peer string) { s.store.RemoveByPeer(peer) }
func (s legacyStoreSink) SessionActivity(id string) {
	s.store.Broadcast(store.Event{Type: store.EventSessionActivity, ID: id})
}

func (s legacyStoreSink) PeerWorldChanged(name string) {
	s.store.Broadcast(store.Event{Type: "peer-status", ID: name})
}
func (s legacyStoreSink) AliveSessionCount(peer string) int {
	n := 0
	for _, id := range s.store.ListByPeer(peer) {
		if row, ok := s.store.Get(id); ok && row.Alive {
			n++
		}
	}
	return n
}

// NewManager is the legacy serve adapter. Core Manager construction is
// NewProjectionManager and carries no store dependency.
func NewManager(configs []config.PeerConfig, st *store.Store, selfName string, opts ...PeerOption) *Manager {
	return NewProjectionManager(configs, selfName, legacyStoreSink{st}, EventHooks{}, opts...)
}
