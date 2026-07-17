package peering

import (
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
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

func TestLegacyConstructorPreservesStoreBehavior(t *testing.T) {
	st := store.New()
	m := NewManager(nil, st, "self")
	sink := m.managerSink()
	sink.ReplacePeerSessions("box", []SessionProjection{{ID: "s@box", Peer: "box", Alive: true, Adapter: "shell"}})
	if got, ok := st.Get("s@box"); !ok || !got.Alive || got.Peer != "box" {
		t.Fatalf("legacy projection missing: %#v %v", got, ok)
	}
	sink.RemovePeerSessions("box")
	if _, ok := st.Get("s@box"); ok {
		t.Fatal("legacy removal did not prune")
	}
}
