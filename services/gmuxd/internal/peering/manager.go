package peering

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// Manager orchestrates connections to all configured spoke peers.
type Manager struct {
	mu       sync.RWMutex
	peers    map[string]*managedPeer
	store    *store.Store
	selfName string // this machine's hostname, for self-echo detection
	baseCtx  context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type managedPeer struct {
	peer   *Peer
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager creates a manager but does not start connections.
// Call Start() to begin subscribing to peers.
//
// selfName is the local machine's hostname, used to detect (and drop)
// sessions that are our own data echoed back through a mutual peer
// subscription.
func NewManager(configs []config.PeerConfig, st *store.Store, selfName string) *Manager {
	m := &Manager{
		peers:    make(map[string]*managedPeer, len(configs)),
		store:    st,
		selfName: selfName,
	}

	for _, cfg := range configs {
		p := newPeer(cfg, st, m.onStatus)
		p.isKnownOrigin = m.isKnownOrigin
		m.peers[cfg.Name] = &managedPeer{peer: p}
	}
	return m
}

// onStatus broadcasts a peer status change as an SSE event.
func (m *Manager) onStatus(name string, status Status) {
	m.store.Broadcast(store.Event{
		Type: "peer-status",
		ID:   name,
	})
}

// isKnownOrigin reports whether name refers to this node or a peer we
// are directly subscribed to. Called from Peer.handleEvent to drop
// forwarded sessions reachable via a shorter path.
func (m *Manager) isKnownOrigin(name string) bool {
	if name == m.selfName {
		return true
	}
	m.mu.RLock()
	_, ok := m.peers[name]
	m.mu.RUnlock()
	return ok
}

// Start begins background goroutines that connect to each peer.
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.baseCtx = ctx
	m.cancel = cancel

	for name, mp := range m.peers {
		m.startPeer(name, mp)
	}
}

func (m *Manager) startPeer(name string, mp *managedPeer) {
	peerCtx, peerCancel := context.WithCancel(m.baseCtx)
	mp.cancel = peerCancel
	mp.done = make(chan struct{})

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(mp.done)
		log.Printf("peering: starting connection to %s (%s)", name, mp.peer.Config.URL)
		mp.peer.run(peerCtx)
	}()
}

// Stop cancels all peer connections and waits for goroutines to finish.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()

	// Clean up any remaining peer sessions from the store.
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name := range m.peers {
		m.store.RemoveByPeer(name)
	}
}

// AddPeer registers a new peer and starts its connection. If a peer
// with the same name already exists, AddPeer is a no-op.
func (m *Manager) AddPeer(cfg config.PeerConfig, opts ...PeerOption) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.baseCtx == nil {
		return // manager not started
	}
	if _, exists := m.peers[cfg.Name]; exists {
		return
	}

	p := newPeer(cfg, m.store, m.onStatus, opts...)
	p.isKnownOrigin = m.isKnownOrigin
	mp := &managedPeer{peer: p}
	m.peers[cfg.Name] = mp
	m.startPeer(cfg.Name, mp)
}

// RemovePeer stops a peer connection, waits for its goroutine to finish,
// and removes all its sessions from the store.
func (m *Manager) RemovePeer(name string) {
	m.mu.Lock()
	mp, ok := m.peers[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.peers, name)
	m.mu.Unlock()

	if mp.cancel != nil {
		mp.cancel()
	}
	if mp.done != nil {
		<-mp.done
	}
	m.store.RemoveByPeer(name)
}

// FindPeer looks up the peer that owns a namespaced session ID.
// Returns nil for local sessions (no "@" in the ID).
func (m *Manager) FindPeer(namespacedID string) (*Peer, string) {
	originalID, peerName := ParseID(namespacedID)
	if peerName == "" {
		return nil, namespacedID
	}
	m.mu.RLock()
	mp, ok := m.peers[peerName]
	m.mu.RUnlock()
	if !ok {
		return nil, originalID
	}
	return mp.peer, originalID
}

// PeerStatus returns the current status of all peers.
func (m *Manager) PeerStatus() []PeerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	infos := make([]PeerInfo, 0, len(m.peers))
	for _, mp := range m.peers {
		name := mp.peer.Config.Name
		alive := 0
		for _, id := range m.store.ListByPeer(name) {
			if s, ok := m.store.Get(id); ok && s.Alive {
				alive++
			}
		}
		infos = append(infos, PeerInfo{
			Name:         name,
			URL:          mp.peer.Config.URL,
			Status:       mp.peer.Status().String(),
			SessionCount: alive,
			LastError:    mp.peer.LastError(),
		})
	}
	return infos
}

// GetPeer returns a peer by name, or nil if not found.
func (m *Manager) GetPeer(name string) *Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mp := m.peers[name]
	if mp == nil {
		return nil
	}
	return mp.peer
}

// HasPeers returns true if any peers are configured.
func (m *Manager) HasPeers() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.peers) > 0
}

// PeerConfigs fetches /v1/config from all connected peers in parallel.
// Returns a map from peer name to the raw JSON config data.
// Peers that are not connected or fail to respond are omitted.
func (m *Manager) PeerConfigs(ctx context.Context) map[string]json.RawMessage {
	m.mu.RLock()
	type entry struct {
		name string
		peer *Peer
	}
	var connected []entry
	for name, mp := range m.peers {
		if mp.peer.Status() == StatusConnected {
			connected = append(connected, entry{name, mp.peer})
		}
	}
	m.mu.RUnlock()

	if len(connected) == 0 {
		return nil
	}

	results := make(map[string]json.RawMessage, len(connected))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, e := range connected {
		wg.Add(1)
		go func(name string, peer *Peer) {
			defer wg.Done()
			data := peer.FetchConfig(ctx)
			if data != nil {
				mu.Lock()
				results[name] = data
				mu.Unlock()
			}
		}(e.name, e.peer)
	}

	wg.Wait()
	return results
}
