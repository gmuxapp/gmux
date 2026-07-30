package peering

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// loopNode models one gmuxd in a reciprocal peer link, with the same
// causal chain production has:
//
//	peer SSE snapshot.sessions -> Manager.ReplacePeerSessions
//	  -> EventHooks.PeerSessionsDirty  (composer MarkDirty)
//	  -> compose + fanout               (broadcast)
//	  -> /v1/events?as=peer subscribers get the owned-only frame
//
// The node's owned sessions never change, so a correct system must
// reach quiescence: each side ships its snapshot once (plus reconnect
// re-sends) and then goes silent.
type loopNode struct {
	name   string
	owned  []SessionProjection
	server *httptest.Server

	mu   sync.Mutex
	subs map[chan []SessionProjection]struct{}

	sent           atomic.Int64 // snapshot.sessions frames written to the wire
	projectUpdates atomic.Int64 // projects-update events written to the wire
	projectFetches atomic.Int64 // GET /v1/projects served
	slug           atomic.Value // string: slug served by /v1/projects
}

func newLoopNode(t *testing.T, name string, ownedCount int) *loopNode {
	t.Helper()
	n := &loopNode{name: name, subs: map[chan []SessionProjection]struct{}{}}
	n.slug.Store("proj-" + name)
	for i := 0; i < ownedCount; i++ {
		n.owned = append(n.owned, SessionProjection{
			ID:      fmt.Sprintf("%c%06d1", name[0], i),
			Adapter: "claude",
			Alive:   true,
			Cwd:     "/home/u/work",
			Command: []string{"claude", "--resume"},
			Status:  &SessionStatus{Active: false},
		})
	}
	if n.owned == nil {
		n.owned = []SessionProjection{} // keep nil reserved for the world-frame marker
	}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		// apiclient requires the {"ok":true,"data":{...}} envelope; a bare
		// object makes GetHealth/GetProjects fail with ok=false and
		// fetchProjects bail before any of the peering logic runs.
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"version": "test", "node_id": name}})
		case "/v1/projects":
			n.projectFetches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{
				"configured": []any{map[string]any{"slug": n.slug.Load().(string), "match": []any{map[string]any{"path": "/w"}}}},
				"discovered": []any{},
			}})
		case "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			ch := make(chan []SessionProjection, 64)
			n.mu.Lock()
			n.subs[ch] = struct{}{}
			n.mu.Unlock()
			defer func() {
				n.mu.Lock()
				delete(n.subs, ch)
				n.mu.Unlock()
			}()
			write := func(rows []SessionProjection) bool {
				if rows == nil {
					// World frame stand-in: every world frame the fanout
					// ships to ?as=peer carries projects-update
					// (central_helpers.go), which makes the peer re-fetch
					// /v1/projects. That is loop B's cycle.
					if _, err := fmt.Fprint(w, "event: projects-update\ndata: {\"type\":\"projects-update\"}\n\n"); err != nil {
						return false
					}
					flusher.Flush()
					n.projectUpdates.Add(1)
					return true
				}
				b, _ := json.Marshal(map[string]any{"sessions": rows})
				if _, err := fmt.Fprintf(w, "event: snapshot.sessions\ndata: %s\n\n", b); err != nil {
					return false
				}
				flusher.Flush()
				n.sent.Add(1)
				return true
			}
			// Initial frame, exactly like the fanout's cached snapshot.
			if !write(n.owned) {
				return
			}
			for {
				select {
				case <-r.Context().Done():
					return
				case rows := <-ch:
					if !write(rows) {
						return
					}
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(n.server.Close)
	return n
}

// broadcast is the composer+fanout stand-in: any dirty signal recomposes
// and ships the (unchanged) owned-only frame to every ?as=peer subscriber.
func (n *loopNode) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs {
		select {
		case ch <- n.owned:
		default:
		}
	}
}

// worldDirty is the world-kind stand-in: any world recompose ships a frame
// that carries projects-update to every ?as=peer subscriber.
func (n *loopNode) worldDirty() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs {
		select {
		case ch <- nil:
		default:
		}
	}
}

// wireEvents counts everything this node puts on (or serves off) the peer
// link: snapshot.sessions frames, projects-update frames, and /v1/projects
// requests. A node drives all three paths, so a quiescence assertion that
// looks at only one of them can pass while another spins.
func (n *loopNode) wireEvents() int64 {
	return n.sent.Load() + n.projectUpdates.Load() + n.projectFetches.Load()
}

// loopBEvents counts everything loop B puts on the wire in both directions.
func (n *loopNode) loopBEvents() int64 { return n.projectUpdates.Load() + n.projectFetches.Load() }

// TestReciprocalPeerLinkReachesQuiescence wires two nodes as mutual peers
// and asserts the link goes silent once both snapshots have converged.
//
// Without no-op suppression in managerProjectionSink.ReplacePeerSessions
// this spins at CPU speed and the frame counters run into the thousands.
//
// The assertion covers every wire path the pair drives, not just
// snapshot.sessions: since PeerWorldDirty was added to the sessions path,
// these same two connections also carry projects-update frames and
// /v1/projects fetches, and a test named "reaches quiescence" that ignored
// them could pass with loop B spinning at HTTP speed.
func TestReciprocalPeerLinkReachesQuiescence(t *testing.T) {
	a := newLoopNode(t, "node-a", 8)
	b := newLoopNode(t, "node-b", 8)

	mgrA := NewProjectionManager(
		[]config.PeerConfig{{Name: "node-b", URL: b.server.URL}},
		"node-a", nil, EventHooks{PeerSessionsDirty: a.broadcast, PeerWorldDirty: a.worldDirty},
	)
	mgrB := NewProjectionManager(
		[]config.PeerConfig{{Name: "node-a", URL: a.server.URL}},
		"node-b", nil, EventHooks{PeerSessionsDirty: b.broadcast, PeerWorldDirty: b.worldDirty},
	)
	mgrA.Start()
	defer mgrA.Stop()
	mgrB.Start()
	defer mgrB.Stop()

	// Wait for convergence: both managers hold the other's 8 sessions.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(mgrA.SessionProjections()) == 8 && len(mgrB.SessionProjections()) == 8 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(mgrA.SessionProjections()); got != 8 {
		t.Fatalf("node-a never converged: %d sessions", got)
	}
	if got := len(mgrB.SessionProjections()); got != 8 {
		t.Fatalf("node-b never converged: %d sessions", got)
	}

	// Quiescence window: after convergence nothing changes, so neither side
	// should produce further traffic on any path. Tolerance is the sum of
	// the two loops' in-flight allowances: <=2 straggler snapshot.sessions
	// frames (one per direction, still being written when the window opens)
	// plus loop B's <=4 (a projects-update and its fetch, per direction).
	const tolerance = 6
	settle := a.wireEvents() + b.wireEvents()
	time.Sleep(500 * time.Millisecond)
	after := a.wireEvents() + b.wireEvents()
	if delta := after - settle; delta > tolerance {
		t.Fatalf("reciprocal link did not quiesce: %d wire events in 500ms "+
			"(a sess=%d upd=%d fetch=%d, b sess=%d upd=%d fetch=%d); "+
			"identical snapshots or unchanged catalogs are being re-propagated "+
			"in a feedback loop", delta,
			a.sent.Load(), a.projectUpdates.Load(), a.projectFetches.Load(),
			b.sent.Load(), b.projectUpdates.Load(), b.projectFetches.Load())
	}
}

// TestReplacePeerSessionsSuppressesEqualSnapshot is the unit-level guard:
// re-delivering a byte-identical projection must not fire dirty hooks.
func TestReplacePeerSessionsSuppressesEqualSnapshot(t *testing.T) {
	var dirty atomic.Int64
	m := NewProjectionManager(nil, "self", nil, EventHooks{PeerSessionsDirty: func() { dirty.Add(1) }})
	rows := []SessionProjection{{
		ID: "s1@p", Peer: "p", Adapter: "claude", Alive: true,
		Command: []string{"claude"}, Remotes: map[string]string{"origin": "git@x"},
		Status: &SessionStatus{Active: true},
	}}
	m.ReplacePeerSessions("p", rows)
	if dirty.Load() != 1 {
		t.Fatalf("first snapshot: dirty=%d, want 1", dirty.Load())
	}
	// Fresh but equal value (different backing arrays/pointers).
	again := []SessionProjection{{
		ID: "s1@p", Peer: "p", Adapter: "claude", Alive: true,
		Command: []string{"claude"}, Remotes: map[string]string{"origin": "git@x"},
		Status: &SessionStatus{Active: true},
	}}
	m.ReplacePeerSessions("p", again)
	if dirty.Load() != 1 {
		t.Fatalf("equal snapshot re-delivered: dirty=%d, want 1 (no-op must be suppressed)", dirty.Load())
	}
	// A real change must still fire.
	changed := []SessionProjection{{
		ID: "s1@p", Peer: "p", Adapter: "claude", Alive: false,
		Command: []string{"claude"}, Remotes: map[string]string{"origin": "git@x"},
		Status: &SessionStatus{Active: true},
	}}
	m.ReplacePeerSessions("p", changed)
	if dirty.Load() != 2 {
		t.Fatalf("changed snapshot: dirty=%d, want 2", dirty.Load())
	}
}

// TestReciprocalPeerLinkQuiescesProjectsUpdateLoop covers loop B, the
// world/projects half of the feedback loop, which is independent of and
// self-sustaining without loop A:
//
//	world frame -> projects-update on ?as=peer -> peer fetchProjects
//	  -> onStatus -> PeerWorldDirty -> world frame (back the other way)
//
// Both nodes serve a stable project catalog, so after the first fetch each
// side must go silent. Without the unchanged-guard in fetchProjects this
// spins at HTTP speed (tens of thousands of events and /v1/projects requests
// per second).
func TestReciprocalPeerLinkQuiescesProjectsUpdateLoop(t *testing.T) {
	a := newLoopNode(t, "node-a", 0)
	b := newLoopNode(t, "node-b", 0)

	mgrA := NewProjectionManager(
		[]config.PeerConfig{{Name: "node-b", URL: b.server.URL}},
		"node-a", nil, EventHooks{PeerSessionsDirty: a.broadcast, PeerWorldDirty: a.worldDirty},
	)
	mgrB := NewProjectionManager(
		[]config.PeerConfig{{Name: "node-a", URL: a.server.URL}},
		"node-b", nil, EventHooks{PeerSessionsDirty: b.broadcast, PeerWorldDirty: b.worldDirty},
	)
	mgrA.Start()
	defer mgrA.Stop()
	mgrB.Start()
	defer mgrB.Stop()

	cachedSlug := func(m *Manager, peer string) string {
		p := m.GetPeer(peer)
		if p == nil {
			return ""
		}
		if rows, ok := p.CachedProjects(); ok && len(rows) == 1 {
			return rows[0].Slug
		}
		return ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cachedSlug(mgrA, "node-b") == "proj-node-b" && cachedSlug(mgrB, "node-a") == "proj-node-a" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := cachedSlug(mgrA, "node-b"); got != "proj-node-b" {
		t.Fatalf("node-a never loaded node-b's projects: %q", got)
	}
	if got := cachedSlug(mgrB, "node-a"); got != "proj-node-a" {
		t.Fatalf("node-b never loaded node-a's projects: %q", got)
	}

	settle := a.loopBEvents() + b.loopBEvents()
	time.Sleep(500 * time.Millisecond)
	after := a.loopBEvents() + b.loopBEvents()
	if delta := after - settle; delta > 4 {
		t.Fatalf("loop B did not quiesce: %d projects-update/fetch events in 500ms "+
			"(a upd=%d fetch=%d, b upd=%d fetch=%d); an unchanged re-fetch is being "+
			"reported as a status change", delta,
			a.projectUpdates.Load(), a.projectFetches.Load(),
			b.projectUpdates.Load(), b.projectFetches.Load())
	}

	// Liveness: suppression must not deafen the link. A genuine catalog
	// change on B still has to reach A.
	b.slug.Store("proj-changed")
	b.worldDirty()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cachedSlug(mgrA, "node-b") == "proj-changed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := cachedSlug(mgrA, "node-b"); got != "proj-changed" {
		t.Fatalf("real project change not propagated: cached slug %q", got)
	}
}
