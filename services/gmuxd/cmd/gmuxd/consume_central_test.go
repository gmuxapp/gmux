package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func TestAcknowledgeSessionRetriesSameEndpointIncarnationReplacementGeneration(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "runner.sock")
	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	const (
		incarnation = "reused-incarnation"
		token       = "result-1"
	)
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /read", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gmux-Expect-Incarnation") != incarnation || r.URL.Query().Get("token") != token {
			http.Error(w, "wrong acknowledgement identity", http.StatusConflict)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close(); _ = ln.Close() })

	// N's /read succeeds, but post-I/O validation observes replacement N+1.
	// Endpoint and incarnation are deliberately identical: coordinator
	// Generation is the only ownership identity that can force the retry.
	owners := []sessioncoord.Runtime{
		{Generation: 1, Endpoint: endpoint, Incarnation: incarnation},
		{Generation: 2, Endpoint: endpoint, Incarnation: incarnation},
		{Generation: 2, Endpoint: endpoint, Incarnation: incarnation},
		{Generation: 2, Endpoint: endpoint, Incarnation: incarnation},
	}
	original := acknowledgementRuntime
	resolution := 0
	acknowledgementRuntime = func(*sessioncoord.Coordinator, centralstore.SessionID) (sessioncoord.Runtime, bool) {
		if resolution >= len(owners) {
			t.Fatalf("unexpected owner resolution %d", resolution+1)
		}
		owner := owners[resolution]
		resolution++
		return owner, true
	}
	t.Cleanup(func() { acknowledgementRuntime = original })

	if err := acknowledgeSession(context.Background(), &Bootstrap{}, "1eadfenc", token); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("runner requests=%d, want retry against generation 2", got)
	}
	if resolution != len(owners) {
		t.Fatalf("owner resolutions=%d, want %d", resolution, len(owners))
	}
}
