package peering

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// ── ID namespace helpers ──

func TestNamespaceID(t *testing.T) {
	if got := NamespaceID("sess-abc", "server"); got != "sess-abc@server" {
		t.Errorf("NamespaceID = %q, want %q", got, "sess-abc@server")
	}
}

func TestParseID_Local(t *testing.T) {
	orig, peer := ParseID("sess-abc123")
	if orig != "sess-abc123" || peer != "" {
		t.Errorf("ParseID local = (%q, %q), want (%q, %q)", orig, peer, "sess-abc123", "")
	}
}

func TestParseID_Remote(t *testing.T) {
	orig, peer := ParseID("sess-abc123@server")
	if orig != "sess-abc123" || peer != "server" {
		t.Errorf("ParseID remote = (%q, %q), want (%q, %q)", orig, peer, "sess-abc123", "server")
	}
}

func TestParseID_ChainedMultiLayer(t *testing.T) {
	// Multi-layer: sess-xyz@project-a@server
	// Split on last @ → original="sess-xyz@project-a", peer="server"
	orig, peer := ParseID("sess-xyz@project-a@server")
	if orig != "sess-xyz@project-a" || peer != "server" {
		t.Errorf("ParseID chained = (%q, %q), want (%q, %q)", orig, peer, "sess-xyz@project-a", "server")
	}
}

func TestParseID_Roundtrip(t *testing.T) {
	original := "sess-abc123"
	peerName := "dev-box"
	namespaced := NamespaceID(original, peerName)
	gotOrig, gotPeer := ParseID(namespaced)
	if gotOrig != original || gotPeer != peerName {
		t.Errorf("roundtrip: got (%q, %q), want (%q, %q)", gotOrig, gotPeer, original, peerName)
	}
}

// ── Mock ProjectionSink ──

// mockSink captures ProjectionSink calls for verification in tests.
type mockSink struct {
	mu          sync.Mutex
	replaced    map[string][]SessionProjection
	removed     []string
	activities  []string
	worldEvents []string
}

func newMockSink() *mockSink {
	return &mockSink{replaced: make(map[string][]SessionProjection)}
}

func (m *mockSink) ReplacePeerSessions(peer string, sessions []SessionProjection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replaced[peer] = append([]SessionProjection(nil), sessions...)
}

func (m *mockSink) RemovePeerSessions(peer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, peer)
	delete(m.replaced, peer)
}

func (m *mockSink) SessionActivity(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activities = append(m.activities, id)
}

func (m *mockSink) PeerWorldChanged(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.worldEvents = append(m.worldEvents, name)
}

func (m *mockSink) AliveSessionCount(peer string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, s := range m.replaced[peer] {
		if s.Alive {
			count++
		}
	}
	return count
}

func (m *mockSink) sessions(peer string) []SessionProjection {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.replaced[peer]
	out := make([]SessionProjection, len(s))
	copy(out, s)
	return out
}

func (m *mockSink) findSession(peer, id string) (SessionProjection, bool) {
	sessions := m.sessions(peer)
	for _, s := range sessions {
		if s.ID == id {
			return s, true
		}
	}
	return SessionProjection{}, false
}

// ── SSE integration ──

// spoke is a test HTTP server that behaves like a gmuxd spoke.
type spoke struct {
	*httptest.Server
	mu          sync.Mutex
	sessions    []SessionProjection
	sseClients  []chan string
	sseConnects int
}

// push sends a raw SSE frame to all connected SSE clients.
func (s *spoke) push(eventType string, payload any) {
	data, _ := json.Marshal(payload)
	frame := fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
	s.mu.Lock()
	for _, ch := range s.sseClients {
		select {
		case ch <- frame:
		default:
		}
	}
	s.mu.Unlock()
}

// pushSnapshot emits the current sk.sessions slice as a
// snapshot.sessions SSE event. This is how the protocol-2 spoke
// announces both initial state and any subsequent change — there
// are no per-event session-upsert / session-remove frames.
func (s *spoke) pushSnapshot() {
	s.mu.Lock()
	list := make([]SessionProjection, len(s.sessions))
	copy(list, s.sessions)
	s.mu.Unlock()
	s.push("snapshot.sessions", map[string]any{"sessions": list})
}

// setSessions atomically replaces the spoke's sessions and emits
// the resulting snapshot.
func (s *spoke) setSessions(list []SessionProjection) {
	s.mu.Lock()
	s.sessions = list
	s.mu.Unlock()
	s.pushSnapshot()
}

// spokeServer creates a test HTTP server that behaves like a gmuxd spoke.
// It serves GET /v1/events as an SSE stream and GET /v1/sessions.
func spokeServer(t *testing.T, token string, sessions []SessionProjection) *spoke {
	t.Helper()

	sk := &spoke{sessions: sessions}

	sk.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth check.
		if token != "" {
			got := r.Header.Get("Authorization")
			if got != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		switch r.URL.Path {
		case "/v1/events":
			sk.mu.Lock()
			sk.sseConnects++
			sk.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher := w.(http.Flusher)

			// Initial snapshot.sessions as the protocol-2 spoke does.
			sk.mu.Lock()
			initial := make([]SessionProjection, len(sk.sessions))
			copy(initial, sk.sessions)
			sk.mu.Unlock()
			data, _ := json.Marshal(map[string]any{"sessions": initial})
			fmt.Fprintf(w, "event: snapshot.sessions\ndata: %s\n\n", data)
			flusher.Flush()

			// Hold connection open and forward pushed events.
			ch := make(chan string, 16)
			sk.mu.Lock()
			sk.sseClients = append(sk.sseClients, ch)
			sk.mu.Unlock()

			for {
				select {
				case <-r.Context().Done():
					return
				case msg := <-ch:
					fmt.Fprint(w, msg)
					flusher.Flush()
				}
			}

		case "/v1/sessions":
			sk.mu.Lock()
			list := make([]SessionProjection, len(sk.sessions))
			copy(list, sk.sessions)
			sk.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": list})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))

	t.Cleanup(sk.Close)
	return sk
}

// waitForSessions polls until the mock sink has the expected number of
// sessions for a peer, or times out.
func waitForSessions(t *testing.T, sink *mockSink, peer string, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if len(sink.sessions(peer)) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d sessions from %s, got %d", want, peer, len(sink.sessions(peer)))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestPeerSubscribe_PreservesTitles is the regression guard for the
// title-overwrite bug: a remote session arrives with Title already
// resolved by the spoke but with empty ShellTitle/AdapterTitle
// (those are internal, intentionally off-wire). The hub must not
// re-resolve the title or it would replace the spoke's value with
// the adapter-name fallback (e.g. "codex" instead of "fix remote bug").
func TestPeerSubscribe_PreservesTitles(t *testing.T) {
	sink := newMockSink()
	sessions := []SessionProjection{
		{
			ID:      "sess-1",
			Adapter: "codex",
			Alive:   true,
			Title:   "fix remote bug",
			// ShellTitle/AdapterTitle intentionally empty: that's
			// how they arrive on the wire.
		},
	}

	sk := spokeServer(t, "", sessions)
	cfg := config.PeerConfig{Name: "server", URL: sk.URL}

	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()
	waitForSessions(t, sink, "server", 1)

	got, ok := sink.findSession("server", "sess-1@server")
	if !ok {
		t.Fatal("expected sess-1@server")
	}
	if got.Title != "fix remote bug" {
		t.Errorf("Title = %q, want %q (spoke-resolved title must survive hub upsert)", got.Title, "fix remote bug")
	}

	mgr.Stop()
}

// PeerStatus must surface how each peer was added (Config.Source) so the
// UI can group hosts into devcontainer / manual sections.
func TestPeerStatus_CarriesSource(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager([]config.PeerConfig{
		{Name: "dc", URL: "http://b:8790", Source: config.SourceDevcontainer, Local: true},
		{Name: "hand", URL: "http://c:8790", Source: config.SourceManual},
	}, "test-host", sink, EventHooks{})

	got := map[string]string{}
	for _, p := range mgr.PeerStatus() {
		got[p.Name] = p.Source
	}
	for name, want := range map[string]string{
		"dc":   config.SourceDevcontainer,
		"hand": config.SourceManual,
	} {
		if got[name] != want {
			t.Errorf("PeerStatus()[%q].Source = %q, want %q", name, got[name], want)
		}
	}
}

func TestPeerSubscribe_InitialSessions(t *testing.T) {
	sink := newMockSink()
	sessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "fix-auth"},
		{ID: "sess-2", Adapter: "shell", Alive: true, Slug: "bash"},
	}

	token := "test-token-abc"
	sk := spokeServer(t, token, sessions)

	cfg := config.PeerConfig{
		Name:  "server",
		URL:   sk.URL,
		Token: token,
	}

	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()
	waitForSessions(t, sink, "server", 2)

	// Verify sessions are namespaced correctly.
	s1, ok := sink.findSession("server", "sess-1@server")
	if !ok {
		t.Fatal("expected sess-1@server in sink")
	}
	if s1.Peer != "server" {
		t.Errorf("peer = %q, want %q", s1.Peer, "server")
	}
	if s1.Adapter != "pi" {
		t.Errorf("adapter = %q, want %q", s1.Adapter, "pi")
	}
	if s1.SocketPath != "" {
		t.Errorf("socket_path should be cleared for remote sessions, got %q", s1.SocketPath)
	}

	s2, ok := sink.findSession("server", "sess-2@server")
	if !ok {
		t.Fatal("expected sess-2@server in sink")
	}
	if s2.Adapter != "shell" {
		t.Errorf("adapter = %q, want %q", s2.Adapter, "shell")
	}

	mgr.Stop()

	// After stop, peer sessions should be cleaned up.
	if _, ok := sink.findSession("server", "sess-1@server"); ok {
		t.Error("sessions should be removed after stop")
	}
}

func TestPeerSubscribe_AuthFailure(t *testing.T) {
	sink := newMockSink()
	sk := spokeServer(t, "correct-token", nil)

	cfg := config.PeerConfig{
		Name:  "server",
		URL:   sk.URL,
		Token: "wrong-token",
	}

	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()

	// Give it time to attempt and fail.
	time.Sleep(200 * time.Millisecond)

	// Should have no sessions and be in disconnected/connecting state.
	if len(sink.sessions("server")) != 0 {
		t.Error("should have no sessions with wrong token")
	}

	peer, _ := mgr.FindPeer("anything@server")
	if peer == nil {
		t.Fatal("FindPeer should return the peer even if disconnected")
	}
	// Status should be connecting or disconnected (in backoff loop).
	status := peer.Status()
	if status == StatusConnected {
		t.Error("should not be connected with wrong token")
	}

	mgr.Stop()
}

func TestPeerSubscribe_SocketPathCleared(t *testing.T) {
	sink := newMockSink()
	sessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, SocketPath: "/tmp/gmux-sessions/sess-1.sock"},
	}

	sk := spokeServer(t, "", sessions)

	cfg := config.PeerConfig{
		Name:  "dev",
		URL:   sk.URL,
		Token: "", // no auth for simplicity
	}

	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()
	waitForSessions(t, sink, "dev", 1)

	sess, _ := sink.findSession("dev", "sess-1@dev")
	if sess.SocketPath != "" {
		t.Errorf("socket_path = %q, want empty (cleared for remote)", sess.SocketPath)
	}

	mgr.Stop()
}

func TestFindPeer_Local(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	peer, origID := mgr.FindPeer("sess-abc123")
	if peer != nil {
		t.Error("FindPeer should return nil for local session")
	}
	if origID != "sess-abc123" {
		t.Errorf("origID = %q, want %q", origID, "sess-abc123")
	}
}

func TestFindPeer_Remote(t *testing.T) {
	sink := newMockSink()
	cfg := []config.PeerConfig{{Name: "server", URL: "http://example.com", Token: "t"}}
	mgr := NewProjectionManager(cfg, "test-host", sink, EventHooks{})

	peer, origID := mgr.FindPeer("sess-abc@server")
	if peer == nil {
		t.Fatal("FindPeer should return peer for remote session")
	}
	if peer.Config.Name != "server" {
		t.Errorf("peer name = %q, want %q", peer.Config.Name, "server")
	}
	if origID != "sess-abc" {
		t.Errorf("origID = %q, want %q", origID, "sess-abc")
	}
}

func TestFindPeer_UnknownPeer(t *testing.T) {
	sink := newMockSink()
	cfg := []config.PeerConfig{{Name: "server", URL: "http://example.com", Token: "t"}}
	mgr := NewProjectionManager(cfg, "test-host", sink, EventHooks{})

	peer, _ := mgr.FindPeer("sess-abc@unknown")
	if peer != nil {
		t.Error("FindPeer should return nil for unknown peer")
	}
}

func TestPeerStatusEventBroadcast(t *testing.T) {
	sink := newMockSink()
	sk := spokeServer(t, "", []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "test"},
	})

	hooks := EventHooks{
		PeerWorldDirty: func() {
			// Track world changes
		},
	}

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, hooks)
	mgr.Start()

	// Wait for connection.
	waitForSessions(t, sink, "server", 1)

	mgr.Stop()
}

func TestPeerSubscribe_SessionRemoveEvent(t *testing.T) {
	sink := newMockSink()
	initialSessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "fix-auth"},
		{ID: "sess-2", Adapter: "shell", Alive: true, Slug: "bash"},
	}

	sk := spokeServer(t, "", initialSessions)

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()

	// Wait for initial sessions.
	waitForSessions(t, sink, "server", 2)

	// Drop sess-1 from the spoke's snapshot. The hub diffs and removes.
	sk.setSessions([]SessionProjection{
		{ID: "sess-2", Adapter: "shell", Alive: true, Slug: "bash"},
	})

	// Wait for removal.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := sink.findSession("server", "sess-1@server"); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for session removal")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// sess-2 should still exist.
	if _, ok := sink.findSession("server", "sess-2@server"); !ok {
		t.Error("sess-2@server should still exist")
	}

	mgr.Stop()
}

func TestPeerSubscribe_ActivityForwarded(t *testing.T) {
	sink := newMockSink()
	initialSessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "fix-auth"},
	}

	sk := spokeServer(t, "", initialSessions)

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()

	// Wait for initial session.
	waitForSessions(t, sink, "server", 1)

	// Push an activity event.
	sk.push("session-activity", map[string]any{"type": "session-activity", "id": "sess-1"})

	// Wait for the activity event with the namespaced ID.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for activity event")
		default:
			sink.mu.Lock()
			found := false
			for _, a := range sink.activities {
				if a == "sess-1@server" {
					found = true
					break
				}
			}
			sink.mu.Unlock()
			if found {
				mgr.Stop()
				return // success
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPeerSubscribe_NewSessionViaPush(t *testing.T) {
	sink := newMockSink()

	// Start with one session.
	sk := spokeServer(t, "", []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "initial"},
	})

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()

	waitForSessions(t, sink, "server", 1)

	// Add a new session to the spoke and re-emit the snapshot.
	sk.setSessions([]SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "initial"},
		{ID: "sess-new", Adapter: "shell", Alive: true, Slug: "new-one"},
	})

	waitForSessions(t, sink, "server", 2)

	got, ok := sink.findSession("server", "sess-new@server")
	if !ok {
		t.Fatal("expected sess-new@server in sink")
	}
	if got.Adapter != "shell" {
		t.Errorf("adapter = %q, want %q", got.Adapter, "shell")
	}
	if got.Peer != "server" {
		t.Errorf("peer = %q, want %q", got.Peer, "server")
	}

	mgr.Stop()
}

func TestPeerSubscribe_ProjectStampsPropagateFromOrigin(t *testing.T) {
	// The origin stamps ProjectSlug / ProjectIndex on owned sessions
	// (ADR 0002). Those stamps must round-trip across the SSE wire so
	// the receiving hub can render (peer, slug) folders without
	// re-running match rules locally.
	sink := newMockSink()

	originSess := SessionProjection{
		ID:           "sess-1",
		Adapter:      "pi",
		Alive:        true,
		Slug:         "fix-auth",
		ProjectSlug:  "gmux",
		ProjectIndex: 3,
	}
	sk := spokeServer(t, "", []SessionProjection{originSess})

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	waitForSessions(t, sink, "server", 1)

	got, ok := sink.findSession("server", "sess-1@server")
	if !ok {
		t.Fatal("expected sess-1@server in sink")
	}
	if got.ProjectSlug != "gmux" {
		t.Errorf("ProjectSlug = %q, want %q", got.ProjectSlug, "gmux")
	}
	if got.ProjectIndex != 3 {
		t.Errorf("ProjectIndex = %d, want 3", got.ProjectIndex)
	}
}

func TestPeerSubscribe_DisclaimedSessionRoundTripsAsZero(t *testing.T) {
	// A disclaimed session (origin has no project for it) emits with
	// no project_slug / project_index on the wire. The receiver
	// decodes both as zero values, which the receiver-side projection
	// treats as "fall through to local match rules".
	sink := newMockSink()

	origin := SessionProjection{
		ID:      "sess-1",
		Adapter: "pi",
		Alive:   true,
		Slug:    "loose",
		// ProjectSlug == "", ProjectIndex == 0.
	}
	sk := spokeServer(t, "", []SessionProjection{origin})

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	waitForSessions(t, sink, "server", 1)

	got, ok := sink.findSession("server", "sess-1@server")
	if !ok {
		t.Fatal("expected sess-1@server in sink")
	}
	if got.ProjectSlug != "" {
		t.Errorf("ProjectSlug = %q, want empty", got.ProjectSlug)
	}
	if got.ProjectIndex != 0 {
		t.Errorf("ProjectIndex = %d, want 0", got.ProjectIndex)
	}
}

func TestManagerStop_CleansUpSessions(t *testing.T) {
	sink := newMockSink()
	sink.ReplacePeerSessions("server", []SessionProjection{
		{ID: "s1@server", Adapter: "pi", Alive: true, Peer: "server"},
	})

	cfg := []config.PeerConfig{{Name: "server", URL: "http://10.0.0.5:8790", Token: "t"}}
	mgr := NewProjectionManager(cfg, "test-host", sink, EventHooks{})
	// Don't start — just test Stop cleanup.
	mgr.Stop()

	// Verify sessions were removed
	if _, ok := sink.findSession("server", "s1@server"); ok {
		t.Error("peer session should be removed on stop")
	}
}

// ── Dynamic peer management ──

func TestManager_AddPeer(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	if mgr.HasPeers() {
		t.Fatal("should have no peers initially")
	}

	mgr.AddPeer(config.PeerConfig{Name: "dev", URL: "http://172.17.0.2:8790", Token: "tok"})

	if !mgr.HasPeers() {
		t.Fatal("should have peers after AddPeer")
	}
	if mgr.GetPeer("dev") == nil {
		t.Fatal("GetPeer should return the added peer")
	}
	infos := mgr.PeerStatus()
	if len(infos) != 1 || infos[0].Name != "dev" {
		t.Errorf("PeerStatus = %+v, want 1 entry named 'dev'", infos)
	}
}

func TestManager_AddPeerIdempotent(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	cfg := config.PeerConfig{Name: "dev", URL: "http://172.17.0.2:8790", Token: "tok"}
	mgr.AddPeer(cfg)
	mgr.AddPeer(cfg) // duplicate, should be no-op

	if len(mgr.PeerStatus()) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(mgr.PeerStatus()))
	}
}

func TestManager_RemovePeer(t *testing.T) {
	sink := newMockSink()
	sk := spokeServer(t, "", []SessionProjection{
		{ID: "s1", Adapter: "pi", Alive: true},
	})

	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	mgr.AddPeer(config.PeerConfig{Name: "dev", URL: sk.URL, Token: ""})

	// Wait for session to appear.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := sink.findSession("dev", "s1@dev"); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for session")
		case <-time.After(10 * time.Millisecond):
		}
	}

	mgr.RemovePeer("dev")

	if mgr.GetPeer("dev") != nil {
		t.Fatal("peer should be removed")
	}
	if _, ok := sink.findSession("dev", "s1@dev"); ok {
		t.Fatal("peer sessions should be cleaned up")
	}
}

// The OnPeerRemoved callback is the GC hook that lets the projects
// manager prune namespaced session keys (`id@<peer>`) when a Local
// peer (devcontainer) is destroyed. Verify the callback fires with
// the correct peer name and wasLocal flag for both Local and network
// peers; the projects-side cleanup relies on the wasLocal branch.
func TestManager_RemovePeer_FiresOnPeerRemoved(t *testing.T) {
	type call struct {
		name  string
		local bool
	}

	run := func(t *testing.T, peerLocal bool) {
		t.Helper()
		sink := newMockSink()
		sk := spokeServer(t, "", nil)
		mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
		var got []call
		var mu sync.Mutex
		mgr.OnPeerRemoved = func(name string, wasLocal bool) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, call{name, wasLocal})
		}
		mgr.Start()
		defer mgr.Stop()

		mgr.AddPeer(config.PeerConfig{Name: "dev", URL: sk.URL, Token: "", Local: peerLocal})
		mgr.RemovePeer("dev")

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("expected 1 callback, got %d", len(got))
		}
		if got[0].name != "dev" || got[0].local != peerLocal {
			t.Errorf("callback got (%q, %v), want (dev, %v)", got[0].name, got[0].local, peerLocal)
		}
	}

	t.Run("local peer", func(t *testing.T) { run(t, true) })
	t.Run("network peer", func(t *testing.T) { run(t, false) })
}

func TestManager_RemoveNonexistentIsNoop(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	// Should not panic.
	mgr.RemovePeer("nonexistent")
}

func TestManager_IsLocalPeer(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	// Network peer.
	mgr.AddPeer(config.PeerConfig{Name: "server", URL: "http://10.0.0.5:8790", Token: "t"})
	// Local peer (devcontainer).
	mgr.AddPeer(config.PeerConfig{Name: "container-abc", URL: "http://172.17.0.2:8790", Token: "t", Local: true})

	if mgr.IsLocalPeer("server") {
		t.Error("network peer should not be local")
	}
	if !mgr.IsLocalPeer("container-abc") {
		t.Error("devcontainer peer should be local")
	}
	if mgr.IsLocalPeer("unknown") {
		t.Error("unknown peer should not be local")
	}
}

func TestPeerFetchHealth(t *testing.T) {
	// Spoke returns health with version and launchers.
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"version":"0.8.0","default_launcher":"shell","launchers":[{"id":"shell","label":"Shell"}]}}`))
	}))
	defer spoke.Close()

	sink := newMockSink()
	p := newPeer(config.PeerConfig{Name: "dev", URL: spoke.URL, Token: "tok123"}, sink, nil)

	p.fetchHealth(t.Context())
	h, ok := p.CachedHealth()
	if !ok {
		t.Fatal("CachedHealth returned false after fetchHealth")
	}
	if h.Version != "0.8.0" {
		t.Errorf("version = %q, want %q", h.Version, "0.8.0")
	}
	if h.DefaultLauncher != "shell" {
		t.Errorf("default_launcher = %q, want %q", h.DefaultLauncher, "shell")
	}
	if len(h.Launchers) != 1 || h.Launchers[0].ID != "shell" {
		t.Errorf("launchers = %+v, want [{ID:shell}]", h.Launchers)
	}
}

func TestPeerFetchHealth_BadToken(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer spoke.Close()

	sink := newMockSink()
	p := newPeer(config.PeerConfig{Name: "dev", URL: spoke.URL, Token: "wrong"}, sink, nil)

	p.fetchHealth(t.Context())
	_, ok := p.CachedHealth()
	if ok {
		t.Error("CachedHealth should return false for 401")
	}
}

func TestPeerStatus_IncludesHealthData(t *testing.T) {
	// Two spokes with different launchers.
	spoke1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"version":"0.8.0","default_launcher":"shell","launchers":[{"id":"shell"}]}}`))
	}))
	defer spoke1.Close()

	spoke2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"version":"0.7.9","default_launcher":"pi","node_id":"node_ws","launchers":[{"id":"shell"},{"id":"pi"}]}}`))
	}))
	defer spoke2.Close()

	sink := newMockSink()
	cfgs := []config.PeerConfig{
		{Name: "laptop", URL: spoke1.URL, Token: "t1"},
		{Name: "workstation", URL: spoke2.URL, Token: "t2"},
	}
	mgr := NewProjectionManager(cfgs, "test-host", sink, EventHooks{})

	for _, mp := range mgr.peers {
		mp.peer.setStatus(StatusConnected)
		mp.peer.fetchHealth(t.Context())
	}

	infos := mgr.PeerStatus()
	if len(infos) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(infos))
	}

	var ws *PeerInfo
	for i := range infos {
		if infos[i].Name == "workstation" {
			ws = &infos[i]
		}
	}
	if ws == nil {
		t.Fatal("missing workstation")
	}
	if ws.Version != "0.7.9" {
		t.Errorf("version = %q, want %q", ws.Version, "0.7.9")
	}
	// node_id flows from the spoke's health to the roster so the viewer
	// can anchor references on it (refs #270).
	if ws.NodeID != "node_ws" {
		t.Errorf("node_id = %q, want %q", ws.NodeID, "node_ws")
	}
	if ws.DefaultLauncher != "pi" {
		t.Errorf("default_launcher = %q, want %q", ws.DefaultLauncher, "pi")
	}
	if len(ws.Launchers) != 2 {
		t.Errorf("launchers count = %d, want 2", len(ws.Launchers))
	}
}

func TestPeerStatus_NoHealthBeforeFetch(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("disconnected peer should not be queried")
	}))
	defer spoke.Close()

	sink := newMockSink()
	mgr := NewProjectionManager([]config.PeerConfig{
		{Name: "offline", URL: spoke.URL, Token: "tok"},
	}, "test-host", sink, EventHooks{})

	infos := mgr.PeerStatus()
	if len(infos) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(infos))
	}
	if infos[0].Version != "" {
		t.Errorf("version should be empty before fetch, got %q", infos[0].Version)
	}
	if infos[0].Launchers != nil {
		t.Errorf("launchers should be nil before fetch")
	}
}

func TestCachedHealth_PersistsAcrossDisconnect(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"version":"0.8.0","launchers":[]}}`))
	}))
	defer spoke.Close()

	sink := newMockSink()
	p := newPeer(config.PeerConfig{Name: "dev", URL: spoke.URL}, sink, nil)

	if _, ok := p.CachedHealth(); ok {
		t.Fatal("expected no cached health before fetch")
	}

	p.fetchHealth(t.Context())
	if _, ok := p.CachedHealth(); !ok {
		t.Fatal("expected cached health after fetch")
	}

	// Cache survives disconnect. The spoke's version and launchers
	// don't change because our connection dropped.
	p.setStatus(StatusDisconnected)
	if h, ok := p.CachedHealth(); !ok || h.Version != "0.8.0" {
		t.Fatal("expected cache to persist across disconnect")
	}
}

// TestMutualPeers_NoRecursion verifies that two peers referencing each
// other's /v1/health do not create a request storm. Each side fetches
// health once per connection; CachedHealth reads from memory.
func TestMutualPeers_NoRecursion(t *testing.T) {
	var muA, muB sync.Mutex
	hitsA, hitsB := 0, 0

	spokeA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		muA.Lock()
		hitsA++
		muA.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"version":"0.8.0","default_launcher":"shell"}}`))
	}))
	defer spokeA.Close()

	spokeB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		muB.Lock()
		hitsB++
		muB.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"version":"0.8.0","default_launcher":"pi"}}`))
	}))
	defer spokeB.Close()

	sinkA := newMockSink()
	peerAtoB := newPeer(config.PeerConfig{Name: "B", URL: spokeB.URL}, sinkA, nil)

	sinkB := newMockSink()
	peerBtoA := newPeer(config.PeerConfig{Name: "A", URL: spokeA.URL}, sinkB, nil)

	peerAtoB.fetchHealth(t.Context())
	peerBtoA.fetchHealth(t.Context())

	// Read cached health multiple times.
	for range 10 {
		peerAtoB.CachedHealth()
		peerBtoA.CachedHealth()
	}

	// Each spoke should have been hit exactly once (by fetchHealth).
	muA.Lock()
	muB.Lock()
	if hitsA != 1 {
		t.Errorf("spokeA hit %d times, want 1", hitsA)
	}
	if hitsB != 1 {
		t.Errorf("spokeB hit %d times, want 1", hitsB)
	}
	muB.Unlock()
	muA.Unlock()
}

func TestManager_FindPeerDynamic(t *testing.T) {
	sink := newMockSink()
	mgr := NewProjectionManager(nil, "test-host", sink, EventHooks{})
	mgr.Start()
	defer mgr.Stop()

	mgr.AddPeer(config.PeerConfig{Name: "dev", URL: "http://172.17.0.2:8790", Token: "tok"})

	peer, origID := mgr.FindPeer("sess-abc@dev")
	if peer == nil {
		t.Fatal("FindPeer should resolve dynamically added peer")
	}
	if origID != "sess-abc" {
		t.Errorf("origID = %q, want %q", origID, "sess-abc")
	}
}

// ── ForwardLaunch ──

// TestForwardLaunch_StripsPeerField verifies the hub → spoke forward path
// sends a body without the "peer" field. Without this, the spoke would
// try to forward the request again to a peer of its own with that name.
func TestForwardLaunch_StripsPeerField(t *testing.T) {
	var received []byte
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/launch" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"data":{"pid":1}}`))
	}))
	defer spoke.Close()

	cfg := config.PeerConfig{Name: "dev", URL: spoke.URL, Token: "tok"}
	sink := newMockSink()
	peer := newPeer(cfg, sink, nil)

	body := `{"launcher_id":"shell","cwd":"/root","peer":"dev"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/launch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	peer.ForwardLaunch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("ForwardLaunch status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	// Parse what the spoke received and make sure "peer" is absent.
	var got map[string]any
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("spoke received invalid JSON %q: %v", received, err)
	}
	if _, present := got["peer"]; present {
		t.Errorf("spoke received body %q still contains 'peer' field", received)
	}
	if got["launcher_id"] != "shell" || got["cwd"] != "/root" {
		t.Errorf("other fields lost: got %v", got)
	}
}

func TestPeerStatusCountsOnlyAlive(t *testing.T) {
	sink := newMockSink()
	sink.ReplacePeerSessions("server", []SessionProjection{
		{ID: "alive@server", Adapter: "shell", Alive: true, Peer: "server"},
		{ID: "dead@server", Adapter: "pi", Alive: false, Peer: "server", Command: []string{"pi"}},
	})

	cfg := []config.PeerConfig{
		{Name: "server", URL: "http://10.0.0.5:8790", Token: "t"},
	}
	mgr := NewProjectionManager(cfg, "test-host", sink, EventHooks{})

	infos := mgr.PeerStatus()
	if len(infos) != 1 {
		t.Fatalf("PeerStatus = %d entries, want 1", len(infos))
	}
	if infos[0].SessionCount != 1 {
		t.Errorf("session_count = %d, want 1 (only alive sessions)", infos[0].SessionCount)
	}
}

// ── Forwarding filter ──

func TestForwardingFilter_SelfEchoPrevented(t *testing.T) {
	// Simulates the mutual-subscription loop:
	// Peer "remote" sends us a session "sess-1@test-host" — that's our
	// own session echoed back. It should be dropped.
	sink := newMockSink()

	sk := spokeServer(t, "", []SessionProjection{
		// The spoke has our session in its store as "sess-1@test-host".
		{ID: "sess-1@test-host", Adapter: "shell", Alive: true, Peer: "test-host"},
	})

	mgr := NewProjectionManager([]config.PeerConfig{
		{Name: "remote", URL: sk.URL},
	}, "test-host", sink, EventHooks{})
	mgr.Start()

	// Wait a moment for the SSE to be processed.
	time.Sleep(200 * time.Millisecond)

	// sess-1@test-host@remote should NOT exist (self-echo dropped).
	if _, ok := sink.findSession("remote", "sess-1@test-host@remote"); ok {
		t.Error("self-echo should be dropped, but sess-1@test-host@remote exists in sink")
	}

	mgr.Stop()
}

func TestForwardingFilter_KnownPeerSessionDropped(t *testing.T) {
	// Two peers: "alpha" and "beta". Beta has alpha's session forwarded
	// as "sess-a@alpha". Since we subscribe to alpha directly, we should
	// drop the forwarded copy from beta.
	sink := newMockSink()

	skAlpha := spokeServer(t, "", []SessionProjection{
		{ID: "sess-a", Adapter: "shell", Alive: true},
	})
	skBeta := spokeServer(t, "", []SessionProjection{
		{ID: "sess-b", Adapter: "shell", Alive: true},
		{ID: "sess-a@alpha", Adapter: "shell", Alive: true, Peer: "alpha"}, // forwarded
	})

	mgr := NewProjectionManager([]config.PeerConfig{
		{Name: "alpha", URL: skAlpha.URL},
		{Name: "beta", URL: skBeta.URL},
	}, "test-host", sink, EventHooks{})
	mgr.Start()

	waitForSessions(t, sink, "alpha", 1)
	waitForSessions(t, sink, "beta", 1) // only sess-b, not sess-a@alpha

	// Direct session from alpha: present.
	if _, ok := sink.findSession("alpha", "sess-a@alpha"); !ok {
		t.Error("expected direct session sess-a@alpha")
	}

	// Beta's own session: present.
	if _, ok := sink.findSession("beta", "sess-b@beta"); !ok {
		t.Error("expected sess-b@beta")
	}

	// Forwarded session from beta: absent.
	if _, ok := sink.findSession("beta", "sess-a@alpha@beta"); ok {
		t.Error("forwarded sess-a@alpha@beta should be dropped")
	}

	mgr.Stop()
}

func TestForwardingFilter_UnknownPeerSessionKept(t *testing.T) {
	// Peer "remote" has a devcontainer session "sess-d@devcontainer".
	// We don't know "devcontainer" as a direct peer, so it should be kept.
	sink := newMockSink()

	sk := spokeServer(t, "", []SessionProjection{
		{ID: "sess-1", Adapter: "shell", Alive: true},
		{ID: "sess-d@devcontainer", Adapter: "pi", Alive: true, Peer: "devcontainer"},
	})

	mgr := NewProjectionManager([]config.PeerConfig{
		{Name: "remote", URL: sk.URL},
	}, "test-host", sink, EventHooks{})
	mgr.Start()

	waitForSessions(t, sink, "remote", 2) // both should arrive

	if _, ok := sink.findSession("remote", "sess-1@remote"); !ok {
		t.Error("expected sess-1@remote")
	}
	if _, ok := sink.findSession("remote", "sess-d@devcontainer@remote"); !ok {
		t.Error("expected sess-d@devcontainer@remote (devcontainer session should be kept)")
	}

	mgr.Stop()
}

func TestOnSleepDoesNotRestartPeerRemovedWhileOldWorkerDrains(t *testing.T) {
	sink := newMockSink()
	sk := spokeServer(t, "", []SessionProjection{{ID: "s", Adapter: "shell", Alive: true}})
	mgr := NewProjectionManager([]config.PeerConfig{{Name: "remote", URL: sk.URL}}, "test-host", sink, EventHooks{})
	mgr.Start()
	waitForSessions(t, sink, "remote", 1)
	sk.mu.Lock()
	beforeConnects := sk.sseConnects
	sk.mu.Unlock()
	reached := make(chan struct{})
	release := make(chan struct{})
	mgr.beforeSleepRestart = func(string) { close(reached); <-release }
	done := make(chan struct{})
	go func() { mgr.OnSleep(); close(done) }()
	<-reached
	mgr.RemovePeer("remote")
	close(release)
	<-done
	time.Sleep(20 * time.Millisecond)
	sk.mu.Lock()
	connects := sk.sseConnects
	sk.mu.Unlock()
	mgr.mu.RLock()
	_, member := mgr.peers["remote"]
	mgr.mu.RUnlock()
	if member {
		t.Fatal("removed peer remains a member")
	}
	if connects != beforeConnects {
		t.Fatalf("ghost peer restarted: connections %d -> %d", beforeConnects, connects)
	}
	mgr.Stop()
}

func TestOnSleep_ReconnectsAndResyncs(t *testing.T) {
	sink := newMockSink()

	sessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true},
		{ID: "sess-2", Adapter: "shell", Alive: true},
	}
	sk := spokeServer(t, "", sessions)

	mgr := NewProjectionManager([]config.PeerConfig{
		{Name: "remote", URL: sk.URL},
	}, "test-host", sink, EventHooks{})
	mgr.Start()
	waitForSessions(t, sink, "remote", 2)

	// Simulate a session dying on the spoke while hub was "asleep"
	// (no SSE event delivered).
	sk.mu.Lock()
	sk.sessions = []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true},
		// sess-2 is gone
	}
	sk.mu.Unlock()

	// Hub still thinks sess-2 is alive (stale).
	if s, ok := sink.findSession("remote", "sess-2@remote"); !ok || !s.Alive {
		t.Fatal("precondition: sess-2@remote should be alive on hub")
	}

	// Trigger sleep recovery.
	mgr.OnSleep()

	// Wait for reconnection: sess-1 arrives in the fresh dump.
	waitForSessions(t, sink, "remote", 1)

	// sess-1 should be alive (refreshed by the new dump).
	if s, ok := sink.findSession("remote", "sess-1@remote"); !ok || !s.Alive {
		t.Error("sess-1@remote should be alive after OnSleep resync")
	}

	// sess-2 stays in the sink (stale but visible). Sessions persist
	// across reconnects; only intentional peer removal or user dismiss
	// deletes them. The spoke's dump didn't include sess-2, so it
	// retains its last-known state.
	if _, ok := sink.findSession("remote", "sess-2@remote"); !ok {
		t.Error("sess-2@remote should still exist (sessions persist across reconnects)")
	}

	mgr.Stop()
}

// TestDisconnect_SessionsPersist verifies that a network disconnect does
// not remove remote sessions from the sink. Sessions stay visible so the
// user's sidebar is stable across transient connection drops.
func TestDisconnect_SessionsPersist(t *testing.T) {
	sink := newMockSink()
	sessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Title: "important work"},
		{ID: "sess-2", Adapter: "codex", Alive: true, Title: "background task"},
	}

	// spokeServer sends sessions then closes, simulating a disconnect.
	sk := spokeServer(t, "", sessions)
	cfg := config.PeerConfig{Name: "server", URL: sk.URL}

	// Short idle timeout so the test doesn't wait 60s for the default.
	mgr := NewProjectionManager([]config.PeerConfig{cfg}, "test-host", sink, EventHooks{},
		WithStreamIdleTimeout(200*time.Millisecond))
	mgr.Start()
	waitForSessions(t, sink, "server", 2)

	// Spoke closes the SSE stream (simulates disconnect).
	// Wait for the idle timeout to fire + reconnect attempt to start.
	sk.Close()
	time.Sleep(500 * time.Millisecond)

	// Both sessions must still be in the sink.
	if _, ok := sink.findSession("server", "sess-1@server"); !ok {
		t.Error("sess-1@server should persist after disconnect")
	}
	if _, ok := sink.findSession("server", "sess-2@server"); !ok {
		t.Error("sess-2@server should persist after disconnect")
	}

	mgr.Stop()

	// After intentional Stop, sessions ARE removed (Manager.removePeer).
	if _, ok := sink.findSession("server", "sess-1@server"); ok {
		t.Error("sess-1@server should be removed after Stop")
	}
}

// TestApplySessionsSnapshot_DedupsIdenticalState verifies that
// applying the same snapshot twice produces zero session-upsert
// broadcasts on the second pass. This is the property that prevents
// peer-mirroring amplification: a peer emitting snapshots at 20 Hz
// must NOT cause this node to fire N session-upserts per peer-snap
// when nothing actually changed.
func TestApplySessionsSnapshot_DedupsIdenticalState(t *testing.T) {
	sink := newMockSink()
	cfg := config.PeerConfig{Name: "server"}
	p := newPeer(cfg, sink, nil)

	sessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "a", Cwd: "/tmp"},
		{ID: "sess-2", Adapter: "shell", Alive: true, Slug: "b", Cwd: "/home"},
		{ID: "sess-3", Adapter: "pi", Alive: false, Slug: "c", Cwd: "/var"},
	}

	// First application: every session is new, so each one must
	// be recorded.
	p.applySessionsSnapshot(sessions)

	// Second application with byte-identical input. Expectation:
	// the sink's sessions map should be unchanged.
	before := sink.sessions("server")
	p.applySessionsSnapshot(sessions)
	after := sink.sessions("server")

	if len(before) != len(after) {
		t.Errorf("session count changed: %d -> %d", len(before), len(after))
	}
}

// TestApplySessionsSnapshot_BroadcastsOnRealChange is the companion:
// when one field actually changes, the sink reflects the change.
func TestApplySessionsSnapshot_BroadcastsOnRealChange(t *testing.T) {
	sink := newMockSink()
	cfg := config.PeerConfig{Name: "server"}
	p := newPeer(cfg, sink, nil)

	sessions := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "a", Cwd: "/tmp"},
		{ID: "sess-2", Adapter: "shell", Alive: true, Slug: "b", Cwd: "/home"},
	}
	p.applySessionsSnapshot(sessions)

	// Flip alive on sess-2 only.
	sessions2 := []SessionProjection{
		{ID: "sess-1", Adapter: "pi", Alive: true, Slug: "a", Cwd: "/tmp"},
		{ID: "sess-2", Adapter: "shell", Alive: false, Slug: "b", Cwd: "/home"},
	}
	p.applySessionsSnapshot(sessions2)

	// Verify sess-2 is now dead in the sink.
	s2, ok := sink.findSession("server", "sess-2@server")
	if !ok {
		t.Fatal("sess-2@server not found in sink")
	}
	if s2.Alive {
		t.Error("sess-2@server should be dead after update")
	}
}
