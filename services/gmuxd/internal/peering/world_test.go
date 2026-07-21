package peering

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// TestWorldProjectionNormalizesNilCachesToEmptySlices pins the browser
// contract for peer_projects / peer_discovered map values: a peer whose
// fetched projects/discovered lists are nil (spoke advertised none) must
// project as EMPTY slices, never nil — nil marshals to JSON null and the
// UI crashes iterating it ("rows is not iterable"). Legacy
// composePeerDiscovered normalized the same way ("peers not yet fetched
// appear with an empty list").
func TestWorldProjectionNormalizesNilCachesToEmptySlices(t *testing.T) {
	p := newPeer(config.PeerConfig{Name: "quiet", URL: "http://spoke.invalid"}, newMockSink(), nil)
	p.mu.Lock()
	p.cachedProjects = nil
	p.cachedDiscovered = nil
	p.projectsLoaded = true // fetched, spoke advertised nothing
	p.mu.Unlock()

	m := &Manager{peers: map[string]*managedPeer{"quiet": {peer: p}}}
	w := m.WorldProjection()

	if v, ok := w.PeerProjects["quiet"]; !ok || v == nil {
		t.Fatalf("PeerProjects[quiet]=%#v ok=%v, want non-nil empty slice", v, ok)
	}
	if v, ok := w.PeerDiscovered["quiet"]; !ok || v == nil {
		t.Fatalf("PeerDiscovered[quiet]=%#v ok=%v, want non-nil empty slice", v, ok)
	}
	// The wire-level symptom: the map value must marshal as [], not null.
	b, err := json.Marshal(w.PeerDiscovered)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("peer_discovered marshals null: %s", b)
	}
}
