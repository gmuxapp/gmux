package main

import (
	"context"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
	"testing"
)

func TestCentralPeerAdapterTypedCopiesAndTailscalePresence(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := peering.NewProjectionManager([]config.PeerConfig{{Name: "box", Local: true}}, "self", nil, peering.EventHooks{})
	// Feed through the protocol projection sink without any legacy store.
	m.ReplacePeerSessions("box", []peering.SessionProjection{{ID: "s@box", Peer: "box", Alive: true, Cwd: "/work", Remotes: map[string]string{"origin": "url"}}})
	a := &centralPeerAdapter{manager: m, store: st, dirty: func(bool, bool) {}, health: func() central.HealthInfo { return central.HealthInfo{} }, tailscale: &tsauth.Listener{}}
	in := a.LocalPeerMatchInputs()
	if len(in) != 1 || in[0].Subject.SessionID != "s" || in[0].CWD != "/work" {
		t.Fatalf("inputs: %#v", in)
	}
	in[0].Remotes["origin"] = "changed"
	if a.LocalPeerMatchInputs()[0].Remotes["origin"] != "url" {
		t.Fatal("match input was not copied")
	}
	rows := a.PeerSessions()
	if len(rows) != 1 || rows[0].ID != "s@box" {
		t.Fatalf("rows: %#v", rows)
	}
	world := a.PeerWorld()
	if world.Health == nil || world.Health.Tailscale == nil {
		t.Fatal("tailscale key absent while listener exists")
	}
	if _, ok := world.LocalPeerSessions[central.LocalPeerSessionKey{PeerKey: "box", SessionID: "s"}]; !ok {
		t.Fatal("typed local presence missing")
	}
}
