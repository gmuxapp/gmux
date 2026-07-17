package peering

import (
	"context"
	"log"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// Manager orchestrates connections to all configured spoke peers.
type Manager struct {
	mu          sync.RWMutex
	peers       map[string]*managedPeer
	sink        ProjectionSink
	sessions    map[string][]SessionProjection
	hooks       EventHooks
	selfName    string // this machine's hostname, for self-echo detection
	defaultOpts []PeerOption
	baseCtx     context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	// OnPeerRemoved fires after RemovePeer has stopped the peer's
	// goroutine and pruned its sessions from the store. wasLocal
	// indicates whether the peer was a Local peer (PeerConfig.Local
	// = true), so callers can prune namespaced projects.json keys
	// matching this peer name. nil = no-op.
	OnPeerRemoved func(name string, wasLocal bool)
}

type managedPeer struct {
	peer   *Peer
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager creates a manager but does not start connections.
// Call Start() to begin subscribing to peers.
//
// selfName is the local machine's hostname, used by isKnownOrigin as a
// defensive backstop against our own data echoed back through a mutual
// peer. NOTE: this is a backstop only, not the load-bearing guard. The
// actual protection against re-forwarding a network peer's sessions is
// the ADR 0002 owned-only filter on `?as=peer` streams (see isOwned in
// cmd/gmuxd/main.go): a spoke only ever ships sessions it owns
// (Peer=="" or a Local/devcontainer peer), so a well-behaved peer never
// echoes our own sessions back here. Consequently selfName is *not*
// required to match this node's tailscale/URL identity (ADR 0007); the
// two can legitimately diverge (e.g. os.Hostname()==container-id) with
// no observable effect. Do not "fix" this into a load-bearing check
// without also revisiting the owned-only filter.

// onStatus broadcasts a peer status change as an SSE event.
func (m *Manager) onStatus(name string, status Status) {
	if m.sink != nil {
		m.sink.PeerWorldChanged(name)
	}
	if m.hooks.PeerWorldDirty != nil {
		m.hooks.PeerWorldDirty()
	}
	if m.IsLocalPeer(name) && status == StatusConnected && m.hooks.LocalPeerConnected != nil {
		m.hooks.LocalPeerConnected(name, m.peerSessions(name))
	}
	// A transport disconnect invalidates the runtime snapshot before durable
	// Local-peer placements are pruned. removePeerSessions performs that
	// ordered transition and emits each dirty/prune callback once.
	if status == StatusDisconnected && m.sink == nil {
		m.removePeerSessions(name)
	}
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

// IsLocalPeer reports whether name is a local peer (e.g. a devcontainer)
// whose sessions this node owns and should include in its SSE stream.
func (m *Manager) IsLocalPeer(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mp, ok := m.peers[name]
	return ok && mp.peer.Config.Local
}

// Start begins background goroutines that connect to each peer.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.baseCtx != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.baseCtx = ctx
	m.cancel = cancel
	peers := make(map[string]*managedPeer, len(m.peers))
	for name, mp := range m.peers {
		peers[name] = mp
	}
	m.mu.Unlock()

	for name, mp := range peers {
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

// OnSleep restarts all peer connections to force a full resync.
// Call this when the system wakes from sleep.
func (m *Manager) OnSleep() {
	m.mu.RLock()
	peers := make(map[string]*managedPeer, len(m.peers))
	for name, mp := range m.peers {
		peers[name] = mp
	}
	m.mu.RUnlock()

	for name, mp := range peers {
		if mp.cancel != nil {
			mp.cancel()
		}
		if mp.done != nil {
			<-mp.done
		}
		m.startPeer(name, mp)
	}
}

// ReconnectAll signals every peer to retry immediately, cutting short
// any backoff wait. Used when an external event suggests peers may now
// be reachable — e.g. a browser client connecting (the user opened or
// returned to the UI and wants to see peer sessions promptly). Peering
// is dial-out only, so without a nudge like this a just-online peer is
// only discovered on its next scheduled retry. A healthy connected
// peer is unaffected: the signal only shortcuts the reconnect wait.
func (m *Manager) ReconnectAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mp := range m.peers {
		mp.peer.Reconnect()
	}
}

// Stop cancels all peer connections and waits for goroutines to finish.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()

	// Clean up remaining projections without recursively taking the lock.
	m.mu.RLock()
	names := make([]string, 0, len(m.peers))
	for name := range m.peers {
		names = append(names, name)
	}
	m.mu.RUnlock()
	for _, name := range names {
		m.removePeerSessions(name)
	}
}

// AddPeer registers a new peer and starts its connection. If a peer
// with the same name already exists, AddPeer is a no-op.
func (m *Manager) AddPeer(cfg config.PeerConfig, opts ...PeerOption) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.peers[cfg.Name]; exists {
		return
	}

	allOpts := append(append([]PeerOption(nil), m.defaultOpts...), opts...)
	p := newPeer(cfg, m.managerSink(), m.onStatus, allOpts...)
	p.isKnownOrigin = m.isKnownOrigin
	mp := &managedPeer{peer: p}
	m.peers[cfg.Name] = mp
	// Pre-Start reconciliation installs configuration only. Start owns all
	// connection goroutine creation.
	if m.baseCtx != nil {
		m.startPeer(cfg.Name, mp)
	}
}

// RemovePeer stops a peer connection, waits for its goroutine to finish,
// and removes all its sessions from the store. If OnPeerRemoved is set,
// it fires after store cleanup so the caller can run additional cleanup
// (e.g. PruneNamespacedKeys for a destroyed devcontainer).
func (m *Manager) RemovePeer(name string) {
	m.mu.Lock()
	mp, ok := m.peers[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.peers, name)
	wasLocal := mp.peer.Config.Local
	m.mu.Unlock()

	if mp.cancel != nil {
		mp.cancel()
	}
	if mp.done != nil {
		<-mp.done
	}
	m.removePeerSessions(name)
	if wasLocal && m.hooks.LocalPeerDisconnected != nil {
		m.hooks.LocalPeerDisconnected(name)
	}
	if m.OnPeerRemoved != nil {
		m.OnPeerRemoved(name, wasLocal)
	}
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
		if m.sink != nil {
			alive = m.sink.AliveSessionCount(name)
		} else {
			for _, s := range m.sessions[name] {
				if s.Alive {
					alive++
				}
			}
		}
		info := PeerInfo{
			Name:         name,
			URL:          mp.peer.Config.URL,
			Status:       mp.peer.Status().String(),
			SessionCount: alive,
			LastError:    mp.peer.LastError(),
			Local:        mp.peer.Config.Local,
			Source:       mp.peer.Config.Source,
		}
		if h, ok := mp.peer.CachedHealth(); ok {
			info.Version = h.Version
			info.DefaultLauncher = h.DefaultLauncher
			info.Launchers = h.Launchers
			info.NodeID = h.NodeID
		}
		infos = append(infos, info)
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

// NewProjectionManager constructs the authority-neutral runtime projection.
// Unlike NewManager it never selects the legacy store adapter.
func NewProjectionManager(configs []config.PeerConfig, selfName string, sink ProjectionSink, hooks EventHooks, opts ...PeerOption) *Manager {
	m := &Manager{peers: make(map[string]*managedPeer, len(configs)), sink: sink, sessions: make(map[string][]SessionProjection), selfName: selfName, defaultOpts: opts, hooks: hooks}
	for _, cfg := range configs {
		p := newPeer(cfg, m.managerSink(), m.onStatus, opts...)
		p.isKnownOrigin = m.isKnownOrigin
		m.peers[cfg.Name] = &managedPeer{peer: p}
	}
	return m
}

type managerProjectionSink struct{ m *Manager }

func (m *Manager) managerSink() ProjectionSink { return managerProjectionSink{m} }
func (s managerProjectionSink) ReplacePeerSessions(peer string, rows []SessionProjection) {
	copyRows := make([]SessionProjection, len(rows))
	for i := range rows {
		copyRows[i] = cloneProjection(rows[i])
	}
	s.m.mu.Lock()
	s.m.sessions[peer] = copyRows
	local := false
	if mp := s.m.peers[peer]; mp != nil {
		local = mp.peer.Config.Local
	}
	s.m.mu.Unlock()
	if s.m.sink != nil {
		s.m.sink.ReplacePeerSessions(peer, copyRows)
	}
	if s.m.hooks.PeerSessionsDirty != nil {
		s.m.hooks.PeerSessionsDirty()
	}
	if local && s.m.hooks.LocalPeerConnected != nil {
		s.m.hooks.LocalPeerConnected(peer, copyRows)
	}
}
func (s managerProjectionSink) RemovePeerSessions(peer string) { s.m.removePeerSessions(peer) }
func (s managerProjectionSink) PeerWorldChanged(name string) {
	if s.m.sink != nil {
		s.m.sink.PeerWorldChanged(name)
	}
}
func (s managerProjectionSink) AliveSessionCount(peer string) int {
	s.m.mu.RLock()
	defer s.m.mu.RUnlock()
	n := 0
	for _, r := range s.m.sessions[peer] {
		if r.Alive {
			n++
		}
	}
	return n
}
func (s managerProjectionSink) SessionActivity(id string) {
	if s.m.sink != nil {
		s.m.sink.SessionActivity(id)
	}
	if s.m.hooks.SessionActivity != nil {
		s.m.hooks.SessionActivity(id)
	}
}
func (m *Manager) removePeerSessions(name string) {
	m.mu.Lock()
	_, hadProjection := m.sessions[name]
	delete(m.sessions, name)
	local := false
	if mp := m.peers[name]; mp != nil {
		local = mp.peer.Config.Local
	}
	m.mu.Unlock()
	if m.sink != nil {
		m.sink.RemovePeerSessions(name)
	}
	if hadProjection && m.hooks.PeerSessionsDirty != nil {
		m.hooks.PeerSessionsDirty()
	}
	if local && m.hooks.LocalPeerDisconnected != nil {
		m.hooks.LocalPeerDisconnected(name)
	}
}
func (m *Manager) peerSessions(name string) []SessionProjection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rows := m.sessions[name]
	out := make([]SessionProjection, len(rows))
	for i := range rows {
		out[i] = cloneProjection(rows[i])
	}
	return out
}

// SessionProjections returns a deep point-in-time copy under the manager lock.
func (m *Manager) SessionProjections() []SessionProjection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []SessionProjection
	for _, rows := range m.sessions {
		for _, r := range rows {
			out = append(out, cloneProjection(r))
		}
	}
	return out
}

// SetEventHooks installs central projection callbacks before Start.
func (m *Manager) SetEventHooks(h EventHooks) { m.mu.Lock(); m.hooks = h; m.mu.Unlock() }

// ReplacePeerSessions applies a decoded peer snapshot through the runtime
// projection. It is primarily the protocol/event boundary used by Peer.
func (m *Manager) ReplacePeerSessions(name string, rows []SessionProjection) {
	m.managerSink().ReplacePeerSessions(name, rows)
}
