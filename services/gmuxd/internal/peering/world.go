package peering

// WorldProjection is a deep point-in-time copy of peer status caches.
type WorldProjection struct {
	Peers          []PeerInfo
	PeerProjects   map[string][]SpokeProject
	PeerDiscovered map[string][]SpokeDiscovered
}

func (m *Manager) WorldProjection() WorldProjection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w := WorldProjection{Peers: make([]PeerInfo, 0, len(m.peers)), PeerProjects: map[string][]SpokeProject{}, PeerDiscovered: map[string][]SpokeDiscovered{}}
	for name, mp := range m.peers {
		alive := 0
		for _, s := range m.sessions[name] {
			if s.Alive {
				alive++
			}
		}
		i := PeerInfo{Name: name, URL: mp.peer.Config.URL, Status: mp.peer.Status().String(), SessionCount: alive, LastError: mp.peer.LastError(), Local: mp.peer.Config.Local, Source: mp.peer.Config.Source}
		if h, ok := mp.peer.CachedHealth(); ok {
			i.Version = h.Version
			i.DefaultLauncher = h.DefaultLauncher
			i.Launchers = append([]LauncherDef(nil), h.Launchers...)
			i.NodeID = h.NodeID
		}
		w.Peers = append(w.Peers, i)
		if p, ok := mp.peer.CachedProjects(); ok {
			w.PeerProjects[name] = append([]SpokeProject(nil), p...)
		}
		if d, ok := mp.peer.CachedDiscovered(); ok {
			w.PeerDiscovered[name] = append([]SpokeDiscovered(nil), d...)
		}
	}
	return w
}
