package tsdiscovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"tailscale.com/tailcfg"
	"tailscale.com/types/netmap"
)

// mockManager records AddPeer/RemovePeer calls.
type mockManager struct {
	added   []string
	removed []string
}

func (m *mockManager) AddPeer(cfg config.PeerConfig, _ ...peering.PeerOption) {
	m.added = append(m.added, cfg.Name)
}
func (m *mockManager) RemovePeer(name string) {
	m.removed = append(m.removed, name)
}

// gmuxHealth returns a valid /v1/health response for the given hostname.
func gmuxHealth(hostname string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"service":  "gmuxd",
				"hostname": hostname,
				"status":   "ready",
			},
		})
	}
}

// makeNode creates a tailcfg.Node suitable for netmap tests.
func makeNode(stableID tailcfg.StableNodeID, name, hostname string, online bool) tailcfg.NodeView {
	n := &tailcfg.Node{
		StableID: stableID,
		Name:     name + ".", // tailscale FQDN has trailing dot
		Hostinfo: (&tailcfg.Hostinfo{Hostname: hostname}).View(),
		Online:   &online,
	}
	return n.View()
}

// makeNetMap builds a NetworkMap from a list of peer nodes.
func makeNetMap(peers ...tailcfg.NodeView) *netmap.NetworkMap {
	return &netmap.NetworkMap{
		Peers: peers,
	}
}

func TestProbe_GmuxInstance(t *testing.T) {
	srv := httptest.NewTLSServer(gmuxHealth("desktop"))
	defer srv.Close()

	w := New(Config{})
	w.client = srv.Client()

	fqdn := srv.URL[len("https://"):]
	name, ok := w.probe(context.Background(), fqdn)
	if !ok {
		t.Fatal("expected probe to succeed")
	}
	if name != "desktop" {
		t.Errorf("hostname = %q, want %q", name, "desktop")
	}
}

func TestProbe_NonGmuxService(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not gmux</html>"))
	}))
	defer srv.Close()

	w := New(Config{})
	w.client = srv.Client()

	fqdn := srv.URL[len("https://"):]
	_, ok := w.probe(context.Background(), fqdn)
	if ok {
		t.Fatal("expected probe to fail for non-gmux service")
	}
}

func TestProbe_ConnectionRefused(t *testing.T) {
	w := New(Config{})
	_, ok := w.probe(context.Background(), "127.0.0.1:1") // nothing listening
	if ok {
		t.Fatal("expected probe to fail for unreachable host")
	}
}

func TestIsManualPeer(t *testing.T) {
	w := New(Config{
		ManualPeerURLs: []string{
			"https://gmux.tailnet.ts.net",
			"https://other.tailnet.ts.net/",
		},
	})

	tests := []struct {
		fqdn string
		want bool
	}{
		{"gmux.tailnet.ts.net", true},
		{"other.tailnet.ts.net", true},
		{"unknown.tailnet.ts.net", false},
	}
	for _, tt := range tests {
		if got := w.isManualPeer(tt.fqdn); got != tt.want {
			t.Errorf("isManualPeer(%q) = %v, want %v", tt.fqdn, got, tt.want)
		}
	}
}

func TestNormFQDN(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"desktop.tailnet.ts.net.", "desktop.tailnet.ts.net"},
		{"desktop.tailnet.ts.net", "desktop.tailnet.ts.net"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normFQDN(tt.in); got != tt.want {
			t.Errorf("normFQDN(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLooksLikeGmux(t *testing.T) {
	w := New(Config{HostnamePrefix: "gmux"})
	tests := []struct {
		hostname string
		want     bool
	}{
		{"gmux", true},
		{"gmux-dev", true},
		{"gmux-dev-kauri", true},
		{"gmuxfoo", false}, // no dash separator
		{"desktop", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := w.looksLikeGmux(tt.hostname); got != tt.want {
			t.Errorf("looksLikeGmux(%q) = %v, want %v", tt.hostname, got, tt.want)
		}
	}
}

func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	mgr := &mockManager{}

	w := New(Config{
		Manager:  mgr,
		StateDir: dir,
	})

	w.SetCacheEntry("node-1", "desktop.ts.net", "desktop", true)
	w.SetCacheEntry("node-2", "phone.ts.net", "", false)
	w.saveState()

	w2 := New(Config{
		Manager:  mgr,
		StateDir: dir,
	})

	state := w2.StateForTest()
	if !state["desktop.ts.net"] {
		t.Error("expected desktop.ts.net to be gmux")
	}
	if state["phone.ts.net"] {
		t.Error("expected phone.ts.net to NOT be gmux")
	}
}

func TestStatePersistence_ReregistersOnLoad(t *testing.T) {
	dir := t.TempDir()

	state := discoveryState{
		Devices: map[string]*deviceEntry{
			"node-1": {FQDN: "desktop.ts.net", PeerName: "desktop", IsGmux: true, ProbedAt: time.Now().Format(time.RFC3339)},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, "tailscale-discovery.json"), data, 0o600)

	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: dir,
	})
	w.Start()
	w.Stop()

	if len(mgr.added) != 1 || mgr.added[0] != "desktop" {
		t.Errorf("expected AddPeer(desktop), got %v", mgr.added)
	}
}

func TestManualPeerURLs(t *testing.T) {
	peers := []config.PeerConfig{
		{Name: "dev", URL: "https://dev.ts.net"},
		{Name: "prod", URL: "http://10.0.0.5:8790"},
	}
	urls := ManualPeerURLs(peers)
	if len(urls) != 2 || urls[0] != "https://dev.ts.net" {
		t.Errorf("ManualPeerURLs = %v", urls)
	}
}

// ── handleNetMap tests ──

func TestHandleNetMap_NewOnlinePeer(t *testing.T) {
	srv := httptest.NewTLSServer(gmuxHealth("desktop"))
	defer srv.Close()

	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	fqdn := srv.URL[len("https://"):]
	nm := makeNetMap(makeNode("node-1", fqdn, "desktop", true))

	w.handleNetMap(context.Background(), nm)

	if len(mgr.added) != 1 || mgr.added[0] != "desktop" {
		t.Errorf("expected AddPeer(desktop), got %v", mgr.added)
	}
	state := w.StateForTest()
	if !state[fqdn] {
		t.Error("expected device to be cached as gmux")
	}
}

func TestHandleNetMap_PeerComesOnline(t *testing.T) {
	srv := httptest.NewTLSServer(gmuxHealth("laptop"))
	defer srv.Close()

	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	fqdn := srv.URL[len("https://"):]

	// First netmap: peer is offline.
	nm1 := makeNetMap(makeNode("node-1", fqdn, "laptop", false))
	w.handleNetMap(context.Background(), nm1)

	if len(mgr.added) != 0 {
		t.Fatalf("expected no AddPeer for offline device, got %v", mgr.added)
	}

	// Second netmap: peer comes online.
	nm2 := makeNetMap(makeNode("node-1", fqdn, "laptop", true))
	w.handleNetMap(context.Background(), nm2)

	if len(mgr.added) != 1 || mgr.added[0] != "laptop" {
		t.Errorf("expected AddPeer(laptop) after coming online, got %v", mgr.added)
	}
}

func TestHandleNetMap_PeerLeavesNetwork(t *testing.T) {
	srv := httptest.NewTLSServer(gmuxHealth("desktop"))
	defer srv.Close()

	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	fqdn := srv.URL[len("https://"):]

	// First: peer appears online and gets discovered.
	nm1 := makeNetMap(makeNode("node-1", fqdn, "desktop", true))
	w.handleNetMap(context.Background(), nm1)

	if len(mgr.added) != 1 {
		t.Fatalf("expected AddPeer, got %v", mgr.added)
	}

	// Second: peer disappears from netmap entirely.
	nm2 := makeNetMap() // empty
	w.handleNetMap(context.Background(), nm2)

	if len(mgr.removed) != 1 || mgr.removed[0] != "desktop" {
		t.Errorf("expected RemovePeer(desktop), got %v", mgr.removed)
	}
	state := w.StateForTest()
	if _, exists := state[fqdn]; exists {
		t.Error("expected device to be removed from cache")
	}
}

func TestHandleNetMap_SkipsManualPeer(t *testing.T) {
	srv := httptest.NewTLSServer(gmuxHealth("desktop"))
	defer srv.Close()

	fqdn := srv.URL[len("https://"):]
	mgr := &mockManager{}
	w := New(Config{
		Manager:        mgr,
		StateDir:       t.TempDir(),
		SelfFQDN:       "self.ts.net",
		ManualPeerURLs: []string{"https://" + fqdn},
	})
	w.client = srv.Client()

	nm := makeNetMap(makeNode("node-1", fqdn, "desktop", true))
	w.handleNetMap(context.Background(), nm)

	if len(mgr.added) != 0 {
		t.Errorf("expected no AddPeer for manual peer, got %v", mgr.added)
	}
}

func TestHandleNetMap_OfflinePeersList(t *testing.T) {
	mgr := &mockManager{}
	w := New(Config{
		Manager:        mgr,
		StateDir:       t.TempDir(),
		SelfFQDN:       "self.ts.net",
		HostnamePrefix: "gmux",
	})

	nm := makeNetMap(
		makeNode("node-1", "gmux-dev.ts.net", "gmux-dev", false),
		makeNode("node-2", "desktop.ts.net", "desktop", false),
		makeNode("node-3", "gmux-hs.ts.net", "gmux-hs", true), // online, won't be in offline list
	)

	w.handleNetMap(context.Background(), nm)

	offline := w.OfflinePeers()
	if len(offline) != 1 {
		t.Fatalf("expected 1 offline peer, got %d: %+v", len(offline), offline)
	}
	if offline[0].Name != "gmux-dev" {
		t.Errorf("offline peer name = %q, want %q", offline[0].Name, "gmux-dev")
	}
}

func TestHandleNetMap_ConfirmedGmuxNotReprobed(t *testing.T) {
	probeCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount++
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"service": "gmuxd", "hostname": "desktop"},
		})
	}))
	defer srv.Close()

	fqdn := srv.URL[len("https://"):]
	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	nm := makeNetMap(makeNode("node-1", fqdn, "desktop", true))

	// First netmap: probes and discovers.
	w.handleNetMap(context.Background(), nm)
	if probeCount != 1 {
		t.Fatalf("expected 1 probe, got %d", probeCount)
	}

	// Second identical netmap: should NOT re-probe (confirmed gmux).
	w.handleNetMap(context.Background(), nm)
	if probeCount != 1 {
		t.Errorf("expected no re-probe for confirmed gmux, got %d probes", probeCount)
	}
}

func TestHandleNetMap_SelfExcluded(t *testing.T) {
	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "gmux.my.ts.net",
	})

	// Self node in the netmap (matching SelfFQDN).
	nm := makeNetMap(makeNode("self-node", "gmux.my.ts.net", "gmux", true))
	w.handleNetMap(context.Background(), nm)

	if len(mgr.added) != 0 {
		t.Errorf("expected self to be excluded, got %v", mgr.added)
	}
}

func TestHandleNetMap_ConfirmedGmuxReconnects(t *testing.T) {
	// A confirmed gmux peer goes offline then comes back online.
	// It should NOT be re-probed (the peer manager handles reconnection).
	probeCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount++
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"service": "gmuxd", "hostname": "desktop"},
		})
	}))
	defer srv.Close()

	fqdn := srv.URL[len("https://"):]
	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	// Netmap 1: peer online, gets probed and confirmed.
	w.handleNetMap(context.Background(), makeNetMap(
		makeNode("node-1", fqdn, "desktop", true),
	))
	if probeCount != 1 {
		t.Fatalf("expected 1 probe, got %d", probeCount)
	}

	// Netmap 2: peer goes offline.
	w.handleNetMap(context.Background(), makeNetMap(
		makeNode("node-1", fqdn, "desktop", false),
	))

	// Netmap 3: peer comes back online. Should NOT re-probe.
	w.handleNetMap(context.Background(), makeNetMap(
		makeNode("node-1", fqdn, "desktop", true),
	))
	if probeCount != 1 {
		t.Errorf("expected no re-probe for confirmed gmux peer, got %d total probes", probeCount)
	}
}

func TestHandleNetMap_FQDNChange(t *testing.T) {
	// A peer's FQDN changes (e.g. tailnet rename). Should re-probe.
	var lastProbeFQDN string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastProbeFQDN = r.Host
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"service": "gmuxd", "hostname": "desktop"},
		})
	}))
	defer srv.Close()

	fqdn := srv.URL[len("https://"):]
	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	// Netmap 1: initial discovery.
	w.handleNetMap(context.Background(), makeNetMap(
		makeNode("node-1", fqdn, "desktop", true),
	))
	if len(mgr.added) != 1 {
		t.Fatalf("expected 1 AddPeer, got %v", mgr.added)
	}

	// Netmap 2: same node ID, but the Name (FQDN) field now contains
	// a different value. Since both FQDNs resolve to the same test
	// server, we just need to verify the re-probe fires.
	// Trick: the test server listens on fqdn, but we can't use a
	// different FQDN (it won't resolve). Instead, manually set prev
	// to make the FQDN differ while keeping the same test server
	// reachable for the probe.
	w.mu.Lock()
	w.prev["node-1"] = peerSnapshot{fqdn: "old.ts.net", hostname: "desktop", online: true}
	w.mu.Unlock()

	mgr.added = nil
	w.handleNetMap(context.Background(), makeNetMap(
		makeNode("node-1", fqdn, "desktop", true),
	))
	// The FQDN changed from "old.ts.net" to fqdn, so re-probe should fire.
	if len(mgr.added) != 1 {
		t.Errorf("expected re-probe after FQDN change, got %v AddPeer calls", mgr.added)
	}
	_ = lastProbeFQDN // used by the server handler
}

func TestHandleNetMap_NonGmuxCooldown(t *testing.T) {
	// A non-gmux device should not be re-probed on every netmap update.
	probeCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount++
		w.Write([]byte("not gmux"))
	}))
	defer srv.Close()

	fqdn := srv.URL[len("https://"):]
	mgr := &mockManager{}
	w := New(Config{
		Manager:  mgr,
		StateDir: t.TempDir(),
		SelfFQDN: "self.ts.net",
	})
	w.client = srv.Client()

	nm := makeNetMap(makeNode("node-1", fqdn, "desktop", true))

	// First netmap: probes, finds non-gmux.
	w.handleNetMap(context.Background(), nm)
	if probeCount != 1 {
		t.Fatalf("expected 1 probe, got %d", probeCount)
	}

	// Second netmap (immediately after): should be skipped by cooldown.
	w.handleNetMap(context.Background(), nm)
	if probeCount != 1 {
		t.Errorf("expected cooldown to prevent re-probe, got %d total probes", probeCount)
	}
}

// TestMakeNode_HostinfoView verifies our test helper produces valid
// Hostinfo views (regression guard for the NodeView API).
func TestMakeNode_HostinfoView(t *testing.T) {
	nv := makeNode("n1", "host.ts.net", "myhostname", true)
	if !nv.Hostinfo().Valid() {
		t.Fatal("expected valid Hostinfo")
	}
	if got := nv.Hostinfo().Hostname(); got != "myhostname" {
		t.Errorf("Hostinfo().Hostname() = %q, want %q", got, "myhostname")
	}
	if got := nv.Online().Get(); !got {
		t.Error("expected Online() = true")
	}
}
