package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func projectRouteSpoke(t *testing.T, frames wire.Frames) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	registerGetProjectsRoute(mux, func(*http.Request) (wire.Frames, error) { return frames, nil }, func(string) bool { return false })
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: snapshot.sessions\ndata: {\"sessions\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"service": "gmuxd"}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestPeerDiscoveredContractDoesNotRelayCThroughB(t *testing.T) {
	// C owns this unmatched session. B sees the same session only as a network
	// mirror. Both servers use the production GET /v1/projects route; A then
	// consumes them through real Peer.fetchProjects caches and composition.
	cFrames := wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{
		{ID: "from-c", Adapter: "shell", Cwd: "/c", Alive: true},
	}}}
	bFrames := wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{
		{ID: "from-c@c", Peer: "c", Adapter: "shell", Cwd: "/c", Alive: true},
	}}}
	b := projectRouteSpoke(t, bFrames)
	c := projectRouteSpoke(t, cFrames)
	manager := peering.NewProjectionManager([]config.PeerConfig{
		{Name: "b", URL: b.URL},
		{Name: "c", URL: c.URL},
	}, "a", nil, peering.EventHooks{})
	manager.Start()
	defer manager.Stop()

	var buckets map[string][]peering.SpokeDiscovered
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		buckets = composePeerDiscovered(manager)
		if bRows, bOK := buckets["b"]; bOK && bRows != nil {
			if cRows, cOK := buckets["c"]; cOK && len(cRows) == 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	bRows, bOK := buckets["b"]
	if !bOK || bRows == nil || len(bRows) != 0 {
		t.Fatalf("A peer_discovered[b] = %#v (present=%v), want present non-nil empty slice", bRows, bOK)
	}
	cRows := buckets["c"]
	if len(cRows) != 1 || cRows[0].SuggestedSlug != "c" || cRows[0].Paths[0] != "/c" {
		t.Fatalf("A peer_discovered[c] = %#v, want C row in source order", cRows)
	}
}
