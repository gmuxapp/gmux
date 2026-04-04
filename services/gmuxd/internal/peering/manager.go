package peering

import (
	"context"
	"log"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// Manager orchestrates connections to all configured spoke peers.
type Manager struct {
	peers  map[string]*Peer
	store  *store.Store
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewManager creates a manager but does not start connections.
// Call Start() to begin subscribing to peers.
func NewManager(configs []config.PeerConfig, st *store.Store) *Manager {
	peers := make(map[string]*Peer, len(configs))
	for _, cfg := range configs {
		peers[cfg.Name] = newPeer(cfg, st)
	}
	return &Manager{
		peers: peers,
		store: st,
	}
}

// Start begins background goroutines that connect to each peer.
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	for name, p := range m.peers {
		m.wg.Add(1)
		go func(name string, p *Peer) {
			defer m.wg.Done()
			log.Printf("peering: starting connection to %s (%s)", name, p.Config.URL)
			p.run(ctx)
		}(name, p)
	}
}

// Stop cancels all peer connections and waits for goroutines to finish.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()

	// Clean up any remaining peer sessions from the store.
	for name := range m.peers {
		m.store.RemoveByPeer(name)
	}
}

// FindPeer looks up the peer that owns a namespaced session ID.
// Returns nil for local sessions (no "@" in the ID).
func (m *Manager) FindPeer(namespacedID string) (*Peer, string) {
	originalID, peerName := ParseID(namespacedID)
	if peerName == "" {
		return nil, namespacedID
	}
	p, ok := m.peers[peerName]
	if !ok {
		return nil, originalID
	}
	return p, originalID
}

// PeerStatus returns the current status of all peers.
func (m *Manager) PeerStatus() []PeerInfo {
	infos := make([]PeerInfo, 0, len(m.peers))
	for _, p := range m.peers {
		infos = append(infos, PeerInfo{
			Name:         p.Config.Name,
			URL:          p.Config.URL,
			Status:       p.Status().String(),
			SessionCount: len(m.store.ListByPeer(p.Config.Name)),
		})
	}
	return infos
}

// HasPeers returns true if any peers are configured.
func (m *Manager) HasPeers() bool {
	return len(m.peers) > 0
}
