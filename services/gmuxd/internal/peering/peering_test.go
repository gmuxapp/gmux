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

	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
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

	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
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
	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
	mgr.Start()
	waitForSessions(t, st, "dev", 1)

	sess, _ := st.Get("sess-1@dev")
	if sess.SocketPath != "" {
		t.Errorf("socket_path = %q, want empty (cleared for remote)", sess.SocketPath)
	}

	mgr.Stop()
}

func TestFindPeer_Local(t *testing.T) {
	mgr := NewManager(nil, store.New(), "test-host")
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
	mgr := NewManager(cfg, store.New(), "test-host")

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
	mgr := NewManager(cfg, store.New(), "test-host")

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
	mgr := NewManager(cfg, st, "test-host")

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
	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
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
	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
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
	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
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
	mgr := NewManager([]config.PeerConfig{cfg}, st, "test-host")
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
	mgr := NewManager(cfg, st, "test-host")
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
	mgr := NewManager(nil, st, "test-host")
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
	mgr := NewManager(nil, st, "test-host")
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

	mgr := NewManager(nil, st, "test-host")
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
	mgr := NewManager(nil, st, "test-host")
	mgr.Start()
	defer mgr.Stop()

	// Should not panic.
	mgr.RemovePeer("nonexistent")
}

func TestPeerFetchConfig(t *testing.T) {
	// Spoke returns a launch config.
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"default_launcher":"shell","launchers":[{"id":"shell","label":"Shell"}]}}`))
	}))
	defer spoke.Close()

	st := store.New()
	p := newPeer(config.PeerConfig{Name: "dev", URL: spoke.URL, Token: "tok123"}, st, nil)

	data := p.FetchConfig(t.Context())
	if data == nil {
		t.Fatal("FetchConfig returned nil")
	}

	var cfg struct {
		DefaultLauncher string `json:"default_launcher"`
		Launchers       []struct {
			ID string `json:"id"`
		} `json:"launchers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.DefaultLauncher != "shell" {
		t.Errorf("default_launcher = %q, want %q", cfg.DefaultLauncher, "shell")
	}
	if len(cfg.Launchers) != 1 || cfg.Launchers[0].ID != "shell" {
		t.Errorf("launchers = %+v, want [{ID:shell}]", cfg.Launchers)
	}
}

func TestPeerFetchConfig_BadToken(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer spoke.Close()

	st := store.New()
	p := newPeer(config.PeerConfig{Name: "dev", URL: spoke.URL, Token: "wrong"}, st, nil)

	data := p.FetchConfig(t.Context())
	if data != nil {
		t.Errorf("FetchConfig should return nil for 401, got %s", data)
	}
}

func TestManagerPeerConfigs(t *testing.T) {
	// Two spokes with different launchers.
	spoke1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"default_launcher":"shell","launchers":[{"id":"shell"}]}}`))
	}))
	defer spoke1.Close()

	spoke2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"data":{"default_launcher":"pi","launchers":[{"id":"shell"},{"id":"pi"}]}}`))
	}))
	defer spoke2.Close()

	// Build a manager with two peers that are "connected".
	st := store.New()
	cfgs := []config.PeerConfig{
		{Name: "laptop", URL: spoke1.URL, Token: "t1"},
		{Name: "workstation", URL: spoke2.URL, Token: "t2"},
	}
	mgr := NewManager(cfgs, st, "test-host")

	// Simulate connected status so PeerConfigs includes them.
	for _, mp := range mgr.peers {
		mp.peer.setStatus(StatusConnected)
	}

	results := mgr.PeerConfigs(t.Context())
	if len(results) != 2 {
		t.Fatalf("expected 2 peer configs, got %d", len(results))
	}

	if results["laptop"] == nil {
		t.Error("missing laptop config")
	}
	if results["workstation"] == nil {
		t.Error("missing workstation config")
	}

	// Verify workstation has pi launcher.
	var wsCfg struct {
		DefaultLauncher string `json:"default_launcher"`
	}
	json.Unmarshal(results["workstation"], &wsCfg)
	if wsCfg.DefaultLauncher != "pi" {
		t.Errorf("workstation default_launcher = %q, want %q", wsCfg.DefaultLauncher, "pi")
	}
}

func TestManagerPeerConfigs_SkipsDisconnected(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("disconnected peer should not be queried")
	}))
	defer spoke.Close()

	st := store.New()
	mgr := NewManager([]config.PeerConfig{
		{Name: "offline", URL: spoke.URL, Token: "tok"},
	}, st, "test-host")
	// Leave status as disconnected (default).

	results := mgr.PeerConfigs(t.Context())
	if len(results) != 0 {
		t.Errorf("expected empty results for disconnected peer, got %d", len(results))
	}
}

func TestManager_FindPeerDynamic(t *testing.T) {
	st := store.New()
	mgr := NewManager(nil, st, "test-host")
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

func TestStripPeerField(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips peer",
			in:   `{"launcher_id":"shell","cwd":"/root","peer":"dev"}`,
			want: `{"cwd":"/root","launcher_id":"shell"}`,
		},
		{
			name: "no peer is noop",
			in:   `{"launcher_id":"shell","cwd":"/root"}`,
			want: `{"cwd":"/root","launcher_id":"shell"}`,
		},
		{
			name: "empty peer is removed too",
			in:   `{"launcher_id":"shell","peer":""}`,
			want: `{"launcher_id":"shell"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := stripPeerField([]byte(tt.in))
			if err != nil {
				t.Fatalf("stripPeerField: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("stripPeerField = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestStripPeerField_InvalidJSON(t *testing.T) {
	_, err := stripPeerField([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

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
	peer := newPeer(cfg, store.New(), nil)

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
	st := store.New()
	st.Upsert(store.Session{ID: "alive@server", Kind: "shell", Alive: true, Peer: "server"})
	st.Upsert(store.Session{ID: "dead@server", Kind: "pi", Alive: false, Peer: "server", Command: []string{"pi"}})

	cfg := []config.PeerConfig{
		{Name: "server", URL: "http://10.0.0.5:8790", Token: "t"},
	}
	mgr := NewManager(cfg, st, "test-host")

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
	st := store.New()
	st.Upsert(store.Session{ID: "sess-1", Kind: "shell", Alive: true}) // our local session

	sk := spokeServer(t, "", []store.Session{
		// The spoke has our session in its store as "sess-1@test-host".
		{ID: "sess-1@test-host", Kind: "shell", Alive: true, Peer: "test-host"},
	})

	mgr := NewManager([]config.PeerConfig{
		{Name: "remote", URL: sk.URL},
	}, st, "test-host")
	mgr.Start()

	// Wait a moment for the SSE to be processed.
	time.Sleep(200 * time.Millisecond)

	// sess-1@test-host@remote should NOT exist (self-echo dropped).
	if _, ok := st.Get("sess-1@test-host@remote"); ok {
		t.Error("self-echo should be dropped, but sess-1@test-host@remote exists in store")
	}

	// Our original local session should still be there.
	if _, ok := st.Get("sess-1"); !ok {
		t.Error("local session sess-1 should still exist")
	}

	mgr.Stop()
}

func TestForwardingFilter_KnownPeerSessionDropped(t *testing.T) {
	// Two peers: "alpha" and "beta". Beta has alpha's session forwarded
	// as "sess-a@alpha". Since we subscribe to alpha directly, we should
	// drop the forwarded copy from beta.
	st := store.New()

	skAlpha := spokeServer(t, "", []store.Session{
		{ID: "sess-a", Kind: "shell", Alive: true},
	})
	skBeta := spokeServer(t, "", []store.Session{
		{ID: "sess-b", Kind: "shell", Alive: true},
		{ID: "sess-a@alpha", Kind: "shell", Alive: true, Peer: "alpha"}, // forwarded
	})

	mgr := NewManager([]config.PeerConfig{
		{Name: "alpha", URL: skAlpha.URL},
		{Name: "beta", URL: skBeta.URL},
	}, st, "test-host")
	mgr.Start()

	waitForSessions(t, st, "alpha", 1)
	waitForSessions(t, st, "beta", 1) // only sess-b, not sess-a@alpha

	// Direct session from alpha: present.
	if _, ok := st.Get("sess-a@alpha"); !ok {
		t.Error("expected direct session sess-a@alpha")
	}

	// Beta's own session: present.
	if _, ok := st.Get("sess-b@beta"); !ok {
		t.Error("expected sess-b@beta")
	}

	// Forwarded session from beta: absent.
	if _, ok := st.Get("sess-a@alpha@beta"); ok {
		t.Error("forwarded sess-a@alpha@beta should be dropped")
	}

	mgr.Stop()
}

func TestForwardingFilter_UnknownPeerSessionKept(t *testing.T) {
	// Peer "remote" has a devcontainer session "sess-d@devcontainer".
	// We don't know "devcontainer" as a direct peer, so it should be kept.
	st := store.New()

	sk := spokeServer(t, "", []store.Session{
		{ID: "sess-1", Kind: "shell", Alive: true},
		{ID: "sess-d@devcontainer", Kind: "pi", Alive: true, Peer: "devcontainer"},
	})

	mgr := NewManager([]config.PeerConfig{
		{Name: "remote", URL: sk.URL},
	}, st, "test-host")
	mgr.Start()

	waitForSessions(t, st, "remote", 2) // both should arrive

	if _, ok := st.Get("sess-1@remote"); !ok {
		t.Error("expected sess-1@remote")
	}
	if _, ok := st.Get("sess-d@devcontainer@remote"); !ok {
		t.Error("expected sess-d@devcontainer@remote (devcontainer session should be kept)")
	}

	mgr.Stop()
}
