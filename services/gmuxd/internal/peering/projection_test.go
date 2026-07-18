package peering

import (
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"testing"
)

type recordingSink struct {
	replaces int
	removes  int
	rows     []SessionProjection
}

func (s *recordingSink) ReplacePeerSessions(_ string, r []SessionProjection) {
	s.replaces++
	s.rows = r
}
func (s *recordingSink) RemovePeerSessions(string) { s.removes++ }
func (*recordingSink) SessionActivity(string)      {}
func (*recordingSink) PeerWorldChanged(string)     {}
func (s *recordingSink) AliveSessionCount(string) int {
	n := 0
	for _, r := range s.rows {
		if r.Alive {
			n++
		}
	}
	return n
}

func TestProjectionManagerIsAuthorityNeutralAndCopyOnly(t *testing.T) {
	sink := &recordingSink{}
	sessionsDirty := 0
	m := NewProjectionManager([]config.PeerConfig{{Name: "box", Local: true}}, "self", sink, EventHooks{PeerSessionsDirty: func() { sessionsDirty++ }})
	m.managerSink().ReplacePeerSessions("box", []SessionProjection{{ID: "s@box", Peer: "box", Alive: true, Command: []string{"x"}, Remotes: map[string]string{"origin": "u"}}})
	if sink.replaces != 1 || sessionsDirty != 1 {
		t.Fatalf("replace=%d dirty=%d", sink.replaces, sessionsDirty)
	}
	a := m.SessionProjections()
	a[0].Command[0] = "mutated"
	a[0].Remotes["origin"] = "mutated"
	b := m.SessionProjections()
	if b[0].Command[0] != "x" || b[0].Remotes["origin"] != "u" {
		t.Fatal("projection escaped manager lock without deep copy")
	}
}

func TestLocalPeerConnectDisconnectHooks(t *testing.T) {
	connected, disconnected := 0, 0
	m := NewProjectionManager([]config.PeerConfig{{Name: "box", Local: true}}, "self", nil, EventHooks{
		LocalPeerConnected:    func(string, []SessionProjection) { connected++ },
		LocalPeerDisconnected: func(string) { disconnected++ },
	})
	m.onStatus("box", StatusConnected)
	m.onStatus("box", StatusDisconnected)
	if connected != 1 || disconnected != 1 {
		t.Fatalf("connect=%d disconnect=%d", connected, disconnected)
	}
}

func TestAuthorityNeutralActivityAndDisconnectProjection(t *testing.T) {
	var activity string
	dirty, pruned := 0, 0
	m := NewProjectionManager([]config.PeerConfig{{Name: "box", Local: true}}, "self", nil, EventHooks{
		SessionActivity:       func(id string) { activity = id },
		PeerSessionsDirty:     func() { dirty++ },
		LocalPeerDisconnected: func(string) { pruned++ },
	})
	m.managerSink().ReplacePeerSessions("box", []SessionProjection{{ID: "s@box", Peer: "box"}})
	dirty = 0
	m.managerSink().SessionActivity("s@box")
	m.onStatus("box", StatusDisconnected)
	if activity != "s@box" || len(m.SessionProjections()) != 0 || dirty != 1 || pruned != 1 {
		t.Fatalf("activity=%q rows=%d dirty=%d pruned=%d", activity, len(m.SessionProjections()), dirty, pruned)
	}
}

func TestAddPeerBeforeStartInstallsAndReplacesConfig(t *testing.T) {
	m := NewProjectionManager(nil, "self", nil, EventHooks{})
	m.AddPeer(config.PeerConfig{Name: "box", URL: "old", Token: "one"})
	if p := m.GetPeer("box"); p == nil || p.Config.Token != "one" || m.baseCtx != nil {
		t.Fatalf("pre-start peer=%v baseCtx=%v", p, m.baseCtx)
	}
	m.RemovePeer("box")
	m.AddPeer(config.PeerConfig{Name: "box", URL: "new", Token: "two"})
	if p := m.GetPeer("box"); p == nil || p.Config.URL != "new" || p.Config.Token != "two" {
		t.Fatalf("replacement=%v", p)
	}
}
