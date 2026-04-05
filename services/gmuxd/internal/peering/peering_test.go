package peering

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
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

// ── SSE integration ──

// spoke is a test HTTP server that behaves like a gmuxd spoke.
type spoke struct {
	*httptest.Server
	mu         sync.Mutex
	sseClients []chan string
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

// spokeServer creates a test HTTP server that behaves like a gmuxd spoke.
// It serves GET /v1/events as an SSE stream and GET /v1/sessions.
func spokeServer(t *testing.T, token string, sessions []store.Session) *spoke {
	t.Helper()

	sk := &spoke{}

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
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher := w.(http.Flusher)

			// Send current sessions as initial upserts.
			sk.mu.Lock()
			for _, s := range sessions {
				s := s
				ev := store.Event{Type: "session-upsert", ID: s.ID, Session: &s}
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: session-upsert\ndata: %s\n\n", data)
			}
			sk.mu.Unlock()
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
			list := make([]store.Session, len(sessions))
			copy(list, sessions)
			sk.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": list})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))

	t.Cleanup(sk.Close)
	return sk
}

func TestPeerSubscribe_InitialSessions(t *testing.T) {
	st := store.New()
	sessions := []store.Session{
		{ID: "sess-1", Kind: "pi", Alive: true, ResumeKey: "fix-auth"},
		{ID: "sess-2", Kind: "shell", Alive: true, ResumeKey: "bash"},
	}

	token := "test-token-abc"
	sk := spokeServer(t, token, sessions)

	cfg := config.PeerConfig{
		Name:  "server",
		URL:   sk.URL,
		Token: token,
	}

	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()
	waitForSessions(t, st, "server", 2)

	// Verify sessions are namespaced correctly.
	s1, ok := st.Get("sess-1@server")
	if !ok {
		t.Fatal("expected sess-1@server in store")
	}
	if s1.Peer != "server" {
		t.Errorf("peer = %q, want %q", s1.Peer, "server")
	}
	if s1.Kind != "pi" {
		t.Errorf("kind = %q, want %q", s1.Kind, "pi")
	}
	if s1.SocketPath != "" {
		t.Errorf("socket_path should be cleared for remote sessions, got %q", s1.SocketPath)
	}

	s2, ok := st.Get("sess-2@server")
	if !ok {
		t.Fatal("expected sess-2@server in store")
	}
	if s2.Kind != "shell" {
		t.Errorf("kind = %q, want %q", s2.Kind, "shell")
	}

	mgr.Stop()

	// After stop, peer sessions should be cleaned up.
	if _, ok := st.Get("sess-1@server"); ok {
		t.Error("sessions should be removed after stop")
	}
}

func TestPeerSubscribe_AuthFailure(t *testing.T) {
	st := store.New()
	sk := spokeServer(t, "correct-token", nil)

	cfg := config.PeerConfig{
		Name:  "server",
		URL:   sk.URL,
		Token: "wrong-token",
	}

	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()

	// Give it time to attempt and fail.
	time.Sleep(200 * time.Millisecond)

	// Should have no sessions and be in disconnected/connecting state.
	if len(st.ListByPeer("server")) != 0 {
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
	st := store.New()
	sessions := []store.Session{
		{ID: "sess-1", Kind: "pi", Alive: true, SocketPath: "/tmp/gmux-sessions/sess-1.sock"},
	}

	sk := spokeServer(t, "", sessions)

	cfg := config.PeerConfig{
		Name:  "dev",
		URL:   sk.URL,
		Token: "", // no auth for simplicity
	}

	// The spoke server doesn't check auth when token is empty, but our
	// Peer always sends the header. Use a server that accepts any auth.
	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()
	waitForSessions(t, st, "dev", 1)

	sess, _ := st.Get("sess-1@dev")
	if sess.SocketPath != "" {
		t.Errorf("socket_path = %q, want empty (cleared for remote)", sess.SocketPath)
	}

	mgr.Stop()
}

func TestFindPeer_Local(t *testing.T) {
	mgr := NewManager(nil, store.New())
	peer, origID := mgr.FindPeer("sess-abc123")
	if peer != nil {
		t.Error("FindPeer should return nil for local session")
	}
	if origID != "sess-abc123" {
		t.Errorf("origID = %q, want %q", origID, "sess-abc123")
	}
}

func TestFindPeer_Remote(t *testing.T) {
	cfg := []config.PeerConfig{{Name: "server", URL: "http://example.com", Token: "t"}}
	mgr := NewManager(cfg, store.New())

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
	cfg := []config.PeerConfig{{Name: "server", URL: "http://example.com", Token: "t"}}
	mgr := NewManager(cfg, store.New())

	peer, _ := mgr.FindPeer("sess-abc@unknown")
	if peer != nil {
		t.Error("FindPeer should return nil for unknown peer")
	}
}

func TestPeerStatus(t *testing.T) {
	st := store.New()
	st.Upsert(store.Session{ID: "s1@server", Kind: "pi", Alive: true, Peer: "server"})
	st.Upsert(store.Session{ID: "s2@server", Kind: "shell", Alive: true, Peer: "server"})

	cfg := []config.PeerConfig{
		{Name: "server", URL: "http://10.0.0.5:8790", Token: "t"},
		{Name: "dev", URL: "http://10.0.0.6:8790", Token: "t"},
	}
	mgr := NewManager(cfg, st)

	infos := mgr.PeerStatus()
	if len(infos) != 2 {
		t.Fatalf("PeerStatus = %d entries, want 2", len(infos))
	}

	// Find server info.
	var serverInfo *PeerInfo
	for i := range infos {
		if infos[i].Name == "server" {
			serverInfo = &infos[i]
		}
	}
	if serverInfo == nil {
		t.Fatal("missing server in PeerStatus")
	}
	if serverInfo.SessionCount != 2 {
		t.Errorf("server session_count = %d, want 2", serverInfo.SessionCount)
	}
	if serverInfo.Status != "disconnected" {
		t.Errorf("server status = %q, want %q (not started)", serverInfo.Status, "disconnected")
	}
}

func TestPeerStatusEventBroadcast(t *testing.T) {
	st := store.New()
	sk := spokeServer(t, "", []store.Session{
		{ID: "sess-1", Kind: "pi", Alive: true, ResumeKey: "test"},
	})

	// Subscribe to store events before starting the manager.
	ch, cancel := st.Subscribe()
	defer cancel()

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()

	// Collect peer-status events until we see "connected" transition.
	deadline := time.After(2 * time.Second)
	var gotPeerStatus bool
	for !gotPeerStatus {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for peer-status event")
		case ev := <-ch:
			if ev.Type == "peer-status" && ev.ID == "server" {
				gotPeerStatus = true
			}
		}
	}

	mgr.Stop()
}

func TestPeerSubscribe_SessionRemoveEvent(t *testing.T) {
	st := store.New()
	initialSessions := []store.Session{
		{ID: "sess-1", Kind: "pi", Alive: true, ResumeKey: "fix-auth"},
		{ID: "sess-2", Kind: "shell", Alive: true, ResumeKey: "bash"},
	}

	sk := spokeServer(t, "", initialSessions)

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()

	// Wait for initial sessions.
	waitForSessions(t, st, "server", 2)

	// Push a remove event for sess-1.
	sk.push("session-remove", store.Event{Type: "session-remove", ID: "sess-1"})

	// Wait for removal.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := st.Get("sess-1@server"); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for session removal")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// sess-2 should still exist.
	if _, ok := st.Get("sess-2@server"); !ok {
		t.Error("sess-2@server should still exist")
	}

	mgr.Stop()
}

func TestPeerSubscribe_ActivityForwarded(t *testing.T) {
	st := store.New()
	initialSessions := []store.Session{
		{ID: "sess-1", Kind: "pi", Alive: true, ResumeKey: "fix-auth"},
	}

	sk := spokeServer(t, "", initialSessions)

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()

	// Wait for initial session.
	waitForSessions(t, st, "server", 1)

	// Subscribe to store events to capture the activity broadcast.
	ch, cancel := st.Subscribe()
	defer cancel()

	// Push an activity event.
	sk.push("session-activity", store.Event{Type: "session-activity", ID: "sess-1"})

	// Wait for the activity event with the namespaced ID.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for activity event")
		case ev := <-ch:
			if ev.Type == "session-activity" && ev.ID == "sess-1@server" {
				mgr.Stop()
				return // success
			}
			// Ignore other events (upserts from cleanup, etc.)
		}
	}
}

func TestPeerSubscribe_NewSessionViaPush(t *testing.T) {
	st := store.New()

	// Start with one session.
	sk := spokeServer(t, "", []store.Session{
		{ID: "sess-1", Kind: "pi", Alive: true, ResumeKey: "initial"},
	})

	cfg := config.PeerConfig{Name: "server", URL: sk.URL, Token: ""}
	mgr := NewManager([]config.PeerConfig{cfg}, st)
	mgr.Start()

	waitForSessions(t, st, "server", 1)

	// Push a new session that wasn't in the initial set.
	newSess := store.Session{ID: "sess-new", Kind: "shell", Alive: true, ResumeKey: "new-one"}
	sk.push("session-upsert", store.Event{
		Type: "session-upsert", ID: "sess-new", Session: &newSess,
	})

	waitForSessions(t, st, "server", 2)

	got, ok := st.Get("sess-new@server")
	if !ok {
		t.Fatal("expected sess-new@server in store")
	}
	if got.Kind != "shell" {
		t.Errorf("kind = %q, want %q", got.Kind, "shell")
	}
	if got.Peer != "server" {
		t.Errorf("peer = %q, want %q", got.Peer, "server")
	}

	mgr.Stop()
}

// waitForSessions polls until the store has the expected number of sessions
// for a peer, or times out.
func waitForSessions(t *testing.T, st *store.Store, peer string, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if len(st.ListByPeer(peer)) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d sessions from %s, got %d", want, peer, len(st.ListByPeer(peer)))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestManagerStop_CleansUpSessions(t *testing.T) {
	st := store.New()
	st.Upsert(store.Session{ID: "local-1", Kind: "pi", Alive: true})
	st.Upsert(store.Session{ID: "s1@server", Kind: "pi", Alive: true, Peer: "server"})

	cfg := []config.PeerConfig{{Name: "server", URL: "http://10.0.0.5:8790", Token: "t"}}
	mgr := NewManager(cfg, st)
	// Don't start — just test Stop cleanup.
	mgr.Stop()

	if _, ok := st.Get("local-1"); !ok {
		t.Error("local session should not be removed")
	}
	if _, ok := st.Get("s1@server"); ok {
		t.Error("peer session should be removed on stop")
	}
}

// ── Dynamic peer management ──

func TestManager_AddPeer(t *testing.T) {
	st := store.New()
	mgr := NewManager(nil, st)
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
	st := store.New()
	mgr := NewManager(nil, st)
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
	st := store.New()
	sk := spokeServer(t, "", []store.Session{
		{ID: "s1", Kind: "pi", Alive: true},
	})

	mgr := NewManager(nil, st)
	mgr.Start()
	defer mgr.Stop()

	mgr.AddPeer(config.PeerConfig{Name: "dev", URL: sk.URL, Token: ""})

	// Wait for session to appear.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := st.Get("s1@dev"); ok {
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
	if _, ok := st.Get("s1@dev"); ok {
		t.Fatal("peer sessions should be cleaned up")
	}
}

func TestManager_RemoveNonexistentIsNoop(t *testing.T) {
	st := store.New()
	mgr := NewManager(nil, st)
	mgr.Start()
	defer mgr.Stop()

	// Should not panic.
	mgr.RemovePeer("nonexistent")
}

func TestManager_FindPeerDynamic(t *testing.T) {
	st := store.New()
	mgr := NewManager(nil, st)
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
