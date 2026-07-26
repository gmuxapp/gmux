package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// TestComposedPeerSessionCountRequestsWorldRecompose pins the production wiring
// for the world fields derived from peer sessions.
//
// peers[].session_count (peering/world.go) and PeerWorld.LocalPeerSessions come
// from the same projection the sessions kind is built from, but the peering
// sessions hook only marks the sessions kind dirty. Before the reciprocal-loop
// fix the snapshot storm kept the world recomposing continuously, so the count
// was accidentally always fresh; once the link quiesces, a missing world dirty
// leaves the browser holding a stale session_count indefinitely.
func TestComposedPeerSessionCountRequestsWorldRecompose(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	spoke := newComposedSpoke(t, nil)
	mgr := peering.NewProjectionManager([]config.PeerConfig{{Name: "box", URL: spoke.URL}}, "self", nil, peering.EventHooks{})
	var mu sync.Mutex
	var dirt [][2]bool
	adapter := &centralPeerAdapter{
		manager: mgr, store: st,
		dirty:  func(s, w bool) { mu.Lock(); dirt = append(dirt, [2]bool{s, w}); mu.Unlock() },
		health: func() central.HealthInfo { return central.HealthInfo{} },
	}
	mgr.SetEventHooks(adapter.hooks())
	mgr.Start()
	defer mgr.Stop()

	peerCount := func() int {
		for _, p := range adapter.PeerWorld().Peers {
			if p.Name == "box" {
				return p.SessionCount
			}
		}
		return -1
	}
	eventually(t, func() bool {
		for _, p := range adapter.PeerWorld().Peers {
			if p.Name == "box" && p.Status == "connected" {
				return true
			}
		}
		return false
	})
	since := func(baseline int) (sessions, world bool, all [][2]bool) {
		mu.Lock()
		defer mu.Unlock()
		all = append([][2]bool(nil), dirt[baseline:]...)
		for _, d := range all {
			sessions = sessions || d[0]
			world = world || d[1]
		}
		return
	}
	mark := func() int { mu.Lock(); defer mu.Unlock(); return len(dirt) }

	// A new session on the peer changes the alive count: both kinds must
	// be invalidated or the world snapshot serves a stale session_count.
	base := mark()
	rows := []peering.SessionProjection{{ID: "s1", Alive: true, Adapter: "claude"}}
	data, _ := json.Marshal(map[string]any{"sessions": rows})
	spoke.frames <- fmt.Sprintf("event: snapshot.sessions\ndata: %s\n\n", data)
	eventually(t, func() bool { return len(mgr.SessionProjections()) == 1 })
	eventually(t, func() bool { _, w, _ := since(base); return w })
	if s, w, all := since(base); !s || !w {
		t.Fatalf("session set changed (count=%d) but dirt=%v: want both sessions and world invalidated", peerCount(), all)
	}
	if got := peerCount(); got != 1 {
		t.Fatalf("world session_count = %d, want 1", got)
	}

	// Pure metadata churn stays on the sessions kind: promoting it to a world
	// recompose would put avoidable world frames (and the peer /v1/projects
	// fetches they trigger) back on the wire.
	base = mark()
	rows[0].Title = "renamed"
	data, _ = json.Marshal(map[string]any{"sessions": rows})
	spoke.frames <- fmt.Sprintf("event: snapshot.sessions\ndata: %s\n\n", data)
	eventually(t, func() bool { s, _, _ := since(base); return s })
	if _, w, all := since(base); w {
		t.Fatalf("title-only change requested a world recompose: dirt=%v", all)
	}

	// An identical re-delivery must invalidate nothing at all: that is the
	// reciprocal-loop guard, observed at the daemon adapter level. A trailing
	// marker frame makes the assertion deterministic — the spoke writes frames
	// in order, so once the marker's dirt is visible the no-op frame has been
	// fully processed.
	base = mark()
	data, _ = json.Marshal(map[string]any{"sessions": rows})
	spoke.frames <- fmt.Sprintf("event: snapshot.sessions\ndata: %s\n\n", data)
	marker := []peering.SessionProjection{{ID: "s1", Alive: true, Adapter: "claude", Title: "marker"}}
	data, _ = json.Marshal(map[string]any{"sessions": marker})
	spoke.frames <- fmt.Sprintf("event: snapshot.sessions\ndata: %s\n\n", data)
	eventually(t, func() bool { return peerCount() == 1 })
	eventually(t, func() bool { _, _, all := since(base); return len(all) > 0 })
	if _, _, all := since(base); len(all) != 1 {
		t.Fatalf("identical snapshot re-delivery marked dirty: %v (want only the trailing marker's invalidation)", all)
	}
	rows = marker

	// The session exiting drops the alive count: world must refresh again.
	base = mark()
	rows[0].Alive = false
	rows[0].ExitCode = new(int)
	data, _ = json.Marshal(map[string]any{"sessions": rows})
	spoke.frames <- fmt.Sprintf("event: snapshot.sessions\ndata: %s\n\n", data)
	eventually(t, func() bool { return peerCount() == 0 })
	if s, w, all := since(base); !s || !w {
		t.Fatalf("session exit: dirt=%v, want both kinds", all)
	}
}
