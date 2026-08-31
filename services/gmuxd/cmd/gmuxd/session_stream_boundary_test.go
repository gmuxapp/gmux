package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func TestSessionStreamVersionCompatibility(t *testing.T) {
	if useSemanticSessionStream(false, "") {
		t.Fatal("unversioned browser/custom consumer must receive transitional legacy stream")
	}
	if !useSemanticSessionStream(false, "3") {
		t.Fatal("new browser did not opt into protocol 3")
	}
	if useSemanticSessionStream(true, "") {
		t.Fatal("old peer must receive legacy fallback")
	}
	if !useSemanticSessionStream(true, "3") {
		t.Fatal("new peer did not opt into protocol 3")
	}
	if useSemanticSessionStream(true, "99") {
		t.Fatal("unknown peer protocol must not be guessed")
	}
}

func realisticThousandSessionWorld() wire.WorldPayload {
	projects := make([]wire.ProjectItem, 50)
	for i := range projects {
		ids := make([]string, 20)
		for j := range ids {
			ids[j] = fmt.Sprintf("%08x", i*20+j)
		}
		projects[i] = wire.ProjectItem{
			Slug:     fmt.Sprintf("project-%02d", i),
			Match:    []wire.MatchRule{{Path: fmt.Sprintf("/home/user/dev/project-%02d", i)}, {Remote: fmt.Sprintf("github.com/example/project-%02d", i), Exact: true}},
			Sessions: ids,
			NodeID:   "node-local",
		}
	}
	peers := make([]peering.PeerInfo, 20)
	peerProjects := make(map[string][]peering.SpokeProject, len(peers))
	peerDiscovered := make(map[string][]peering.SpokeDiscovered, len(peers))
	for i := range peers {
		name := fmt.Sprintf("peer-%02d", i)
		peers[i] = peering.PeerInfo{Name: name, URL: "https://" + name + ".example", Status: "connected", SessionCount: 50, Version: "2.1.0", NodeID: "node-" + name}
		peerProjects[name] = []peering.SpokeProject{{Slug: "gmux", LaunchCwd: "/home/user/dev/gmux"}}
		peerDiscovered[name] = []peering.SpokeDiscovered{{SuggestedSlug: "work", Remote: "github.com/example/work", Paths: []string{"/home/user/work"}, SessionCount: 3, ActiveCount: 1}}
	}
	launchers := []peering.LauncherDef{{ID: "shell", Label: "Shell", Command: []string{"gmux", "new"}, Available: true}, {ID: "pi", Label: "Pi", Command: []string{"gmux", "agent"}, Available: true}}
	return wire.WorldPayload{
		Projects: projects, Peers: peers,
		Health:    &central.HealthInfo{Service: "gmuxd", Version: "2.1.0", NodeID: "node-local", Status: "ready", Hostname: "workstation", Sessions: central.SessionCounts{LocalAlive: 667, Dead: 333}},
		Launchers: launchers, DefaultLauncher: "shell", PeerProjects: peerProjects, PeerDiscovered: peerDiscovered,
	}
}

func TestRealisticComposedWorldSizeCharacterization(t *testing.T) {
	// Characterization only: this session-bootstrap PR intentionally does not
	// impose a world maximum or claim world transport framing.
	data, err := json.Marshal(realisticThousandSessionWorld())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("realistic composed 1000-session world payload=%d", len(data))
}

// Subscribe captures the baseline and installs the subscriber under the same
// mutex BroadcastFrames uses. This deterministic boundary is what lets the
// handler finish baseline epoch N and then deliver a concurrently committed
// replacement as epoch N+1 without a lost mutation.
func TestFanoutSubscribeSnapshotBoundary(t *testing.T) {
	f := newSSEFanout()
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "before"}}}})
	initial, ch, cancel := f.Subscribe()
	defer cancel()
	if got := initial.Frames.Sessions.Sessions[0].ID; got != "before" {
		t.Fatalf("initial=%q", got)
	}

	done := make(chan struct{})
	go func() {
		f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "after"}}}})
		close(done)
	}()
	select {
	case msg := <-ch:
		if got := msg.Frames.Sessions.Sessions[0].ID; got != "after" {
			t.Fatalf("queued=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation after subscribe was lost")
	}
	<-done
}
