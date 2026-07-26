package central

import (
	"reflect"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
)

// fakePeerSource returns a swappable point-in-time PeerWorld and counts
// calls.
type fakePeerSource struct {
	mu    sync.Mutex
	world PeerWorld
	calls int
}

func (f *fakePeerSource) PeerWorld() PeerWorld {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.world
}

func (f *fakePeerSource) set(w PeerWorld) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.world = w
}

func (f *fakePeerSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func placementView(peer, session, slug string, pos int) centralstore.LocalPeerPlacementView {
	return centralstore.LocalPeerPlacementView{
		PeerKey: centralstore.PeerKey(peer), SessionID: session,
		ProjectEntryID: 1, ProjectSlug: slug, SiblingScope: "r", Position: pos,
	}
}

// TestPeerWorldJoinDropsDisconnectedLocalPeerRows: a durable Local-peer
// placement row is emitted only when the PeerSource reports a connected
// session for it; stale placement rows (peer gone, session gone) are
// invisible. The projection itself joins at the wire layer.
func TestPeerWorldJoinDropsDisconnectedLocalPeerRows(t *testing.T) {
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{
			Projects: centralstore.ProjectCatalog{{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "p"}},
			LocalPeerPlacements: []centralstore.LocalPeerPlacementView{
				placementView("box", "connected", "p", 0),
				placementView("box", "stale", "p", 1),
				placementView("gone-peer", "s", "p", 2),
			},
		}, nil
	}}
	source := &fakePeerSource{world: PeerWorld{
		LocalPeerSessions: map[LocalPeerSessionKey]struct{}{
			{PeerKey: "box", SessionID: "connected"}: {},
		},
	}}
	sink := &blockingSink{out: make(chan Batch, 1)}
	c := New(reader, nil, sink, WithPeerSource(source))
	startComposer(t, c)

	c.MarkDirty(false, true)
	b := recvBatch(t, sink.out)
	if b.Projects == nil {
		t.Fatalf("batch=%#v", b)
	}
	rows := b.Projects.LocalPeerPlacements
	if len(rows) != 1 || rows[0].SessionID != "connected" {
		t.Fatalf("joined rows=%#v", rows)
	}
	if rows[0].ProjectSlug != "p" || rows[0].Position != 0 {
		t.Fatalf("placement facts lost: %#v", rows[0])
	}
}

// TestPeerWorldOverlayPassesThroughAndResets: peers/health/launchers and
// cached peer projections pass through verbatim; after the source resets
// (peer disappearance), the next MarkDirty pass emits without any trace of
// the previous projections — peer-owned snapshots survive nowhere.
func TestPeerWorldOverlayPassesThroughAndResets(t *testing.T) {
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{
			LocalPeerPlacements: []centralstore.LocalPeerPlacementView{placementView("box", "s", "p", 0)},
		}, nil
	}}
	full := PeerWorld{
		Peers:           []peering.PeerInfo{{Name: "box", Status: "connected", Local: true}},
		Health:          &HealthInfo{Hostname: "hub"},
		Launchers:       []peering.LauncherDef{{ID: "shell"}, {ID: "claude"}},
		DefaultLauncher: "shell",
		PeerProjects:    map[string][]peering.SpokeProject{"net-peer": {{Slug: "remote-project"}}},
		PeerDiscovered:  map[string][]peering.SpokeDiscovered{"net-peer": {{SuggestedSlug: "suggestion"}}},
		LocalPeerSessions: map[LocalPeerSessionKey]struct{}{
			{PeerKey: "box", SessionID: "s"}: {},
		},
	}
	source := &fakePeerSource{world: full}
	sink := &blockingSink{out: make(chan Batch, 1)}
	c := New(reader, nil, sink, WithPeerSource(source))
	startComposer(t, c)

	c.MarkDirty(false, true)
	b := recvBatch(t, sink.out)
	p := b.Projects
	if !reflect.DeepEqual(p.Peers, full.Peers) || !reflect.DeepEqual(p.Health, full.Health) ||
		!reflect.DeepEqual(p.Launchers, full.Launchers) || p.DefaultLauncher != "shell" ||
		!reflect.DeepEqual(p.PeerProjects, full.PeerProjects) || !reflect.DeepEqual(p.PeerDiscovered, full.PeerDiscovered) {
		t.Fatalf("overlay not passed through: %#v", p)
	}
	if len(p.LocalPeerPlacements) != 1 {
		t.Fatalf("join=%#v", p.LocalPeerPlacements)
	}

	// Peer disappears: the source resets; the peer manager marks world
	// dirty (its contract). The recomposed payload retains nothing.
	source.set(PeerWorld{})
	c.MarkDirty(false, true)
	b = recvBatch(t, sink.out)
	p = b.Projects
	if p.Peers != nil || p.PeerProjects != nil || p.PeerDiscovered != nil || len(p.LocalPeerPlacements) != 0 {
		t.Fatalf("peer projections survived source reset: %#v", p)
	}

	// A reset to non-nil-but-empty values (how a real peer manager would
	// report "no peers" after a disconnect) passes those empty values
	// through verbatim and still drops every Local-peer row: an empty
	// LocalPeerSessions map means nothing is connected.
	source.set(PeerWorld{
		Peers:             []peering.PeerInfo{},
		PeerProjects:      map[string][]peering.SpokeProject{},
		LocalPeerSessions: map[LocalPeerSessionKey]struct{}{},
	})
	c.MarkDirty(false, true)
	b = recvBatch(t, sink.out)
	p = b.Projects
	if !reflect.DeepEqual(p.Peers, []peering.PeerInfo{}) || !reflect.DeepEqual(p.PeerProjects, map[string][]peering.SpokeProject{}) {
		t.Fatalf("empty overlay values not passed through: %#v", p)
	}
	if len(p.LocalPeerPlacements) != 0 || p.LocalPeerPlacements == nil {
		t.Fatalf("empty-session-map join=%#v", p.LocalPeerPlacements)
	}
}

// TestNilPeerSourceDropsAllLocalPeerRows: without a PeerSource, no session
// is connected by definition, so every durable Local-peer placement row is
// dropped and the overlay fields stay zero.
func TestNilPeerSourceDropsAllLocalPeerRows(t *testing.T) {
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{
			LocalPeerPlacements: []centralstore.LocalPeerPlacementView{placementView("box", "s", "p", 0)},
		}, nil
	}}
	sink := &blockingSink{out: make(chan Batch, 1)}
	c := New(reader, nil, sink)
	startComposer(t, c)

	c.MarkDirty(false, true)
	b := recvBatch(t, sink.out)
	if b.Projects == nil || len(b.Projects.LocalPeerPlacements) != 0 || b.Projects.Peers != nil {
		t.Fatalf("payload=%#v", b.Projects)
	}
	if b.Projects.LocalPeerPlacements == nil {
		t.Fatal("joined slice must be non-nil for deterministic wire shape")
	}
}

// TestPeerSourceReadOncePerProjectsPassOnly: the peer world is captured
// exactly once per projects-kind pass and never for a sessions-only pass —
// point-in-time, no I/O amplification, MarkDirty remains the peer-manager
// dirt entry point.
func TestPeerSourceReadOncePerProjectsPassOnly(t *testing.T) {
	reader := &fakeReader{}
	source := &fakePeerSource{}
	sink := &blockingSink{out: make(chan Batch, 1)}
	c := New(reader, nil, sink, WithPeerSource(source))
	startComposer(t, c)

	c.MarkDirty(true, false) // sessions only
	recvBatch(t, sink.out)
	if source.callCount() != 0 {
		t.Fatalf("peer world read on a sessions-only pass (%d calls)", source.callCount())
	}

	c.MarkDirty(false, true) // world only (peer-manager dirt)
	recvBatch(t, sink.out)
	if source.callCount() != 1 {
		t.Fatalf("peer world reads = %d, want 1", source.callCount())
	}

	c.MarkDirty(true, true) // cross-kind: matched pair, still one capture
	b := recvBatch(t, sink.out)
	if b.Sessions == nil || b.Projects == nil {
		t.Fatalf("cross-kind batch=%#v", b)
	}
	if source.callCount() != 2 {
		t.Fatalf("peer world reads = %d, want 2", source.callCount())
	}
}
