package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

type discoverTestAdapter struct {
	name      string
	available bool
}

func (a discoverTestAdapter) Name() string                      { return a.name }
func (a discoverTestAdapter) Discover() bool                    { return a.available }
func (a discoverTestAdapter) Match(_ []string) bool             { return false }
func (a discoverTestAdapter) Env(_ adapter.EnvContext) []string { return nil }
func (a discoverTestAdapter) Monitor(_ []byte) *adapter.Event { return nil }
func (a discoverTestAdapter) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{ID: a.name, Label: a.name}}
}

func TestDiscoverAvailableAdaptersRunsAll(t *testing.T) {
	available := discoverAvailableAdapters([]adapter.Adapter{
		discoverTestAdapter{name: "pi", available: true},
		discoverTestAdapter{name: "opencode", available: false},
		discoverTestAdapter{name: "shell", available: true},
	})

	if !available["pi"] {
		t.Fatal("expected pi to be available")
	}
	if available["opencode"] {
		t.Fatal("expected opencode to be unavailable")
	}
	if !available["shell"] {
		t.Fatal("expected shell to be available")
	}
}

func TestLaunchersForAdaptersFiltersUnavailable(t *testing.T) {
	adapterList := []adapter.Adapter{
		discoverTestAdapter{name: "pi", available: true},
		discoverTestAdapter{name: "opencode", available: false},
		discoverTestAdapter{name: "shell", available: true},
	}

	launchers := launchersForAdapters(adapterList, map[string]bool{
		"pi":       true,
		"opencode": false,
		"shell":    true,
	})

	if len(launchers) != 2 {
		t.Fatalf("expected 2 available launchers, got %#v", launchers)
	}
	for _, l := range launchers {
		if !l.Available {
			t.Fatalf("expected launcher to be available: %#v", l)
		}
		if l.ID == "opencode" {
			t.Fatalf("did not expect unavailable launcher in config: %#v", l)
		}
	}
	if launchers[0].ID != "pi" || launchers[1].ID != "shell" {
		t.Fatalf("unexpected launcher order: %#v", launchers)
	}
}

func TestDiscoverLaunchersUsesCompiledAdapters(t *testing.T) {
	cfg := discoverLaunchers()
	if cfg.DefaultLauncher != "shell" {
		t.Fatalf("expected default launcher shell, got %q", cfg.DefaultLauncher)
	}
	if len(cfg.Launchers) < 1 {
		t.Fatalf("expected at least 1 launcher, got %d", len(cfg.Launchers))
	}

	seenShell := false
	for _, l := range cfg.Launchers {
		if !l.Available {
			t.Fatalf("did not expect unavailable launcher in config: %#v", l)
		}
		if l.ID == "shell" {
			seenShell = true
		}
	}
	if !seenShell {
		t.Fatalf("expected shell launcher in %#v", cfg.Launchers)
	}
	if got := cfg.Launchers[len(cfg.Launchers)-1].ID; got != "shell" {
		t.Fatalf("expected shell last, got %q", got)
	}
}

func TestRunNoArgsPrintsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage: gmuxd") {
		t.Fatalf("expected usage output, got %q", stdout.String())
	}
}

func TestRunHelpCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr, got %q", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "Usage: gmuxd") {
		t.Fatalf("expected usage output, got %q", got)
	}
}

func TestRunVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if got := stdout.String(); !strings.Contains(got, version) {
		t.Fatalf("expected version output, got %q", got)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "unknown command") {
		t.Fatalf("expected error output, got %q", got)
	}
}

func TestRunStartRejectsUnknownOption(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"start", "--wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "unknown option") {
		t.Fatalf("expected unknown option error, got %q", got)
	}
}

func TestRunRunRejectsUnknownOption(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"run", "--wat"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "unknown option") {
		t.Fatalf("expected unknown option error, got %q", got)
	}
}

func TestRunStopNoRunningDaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"stop"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "no running daemon") {
		t.Fatalf("expected 'no running daemon', got %q", stdout.String())
	}
}

func TestRunStatusNoRunningDaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("expected 'not running', got %q", stderr.String())
	}
}

func TestRunStatusWithRunningDaemon(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemon(t, "0.9.0")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "0.9.0") {
		t.Errorf("expected version in output, got %q", out)
	}
	if !strings.Contains(out, "socket:") {
		t.Errorf("expected socket path in output, got %q", out)
	}
}

func TestRunStatusShowsSessionsAndPeers(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemonFull(t)
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()

	// Session summary with local/remote breakdown.
	if !strings.Contains(out, "3 alive (2 local, 1 remote)") {
		t.Errorf("expected session summary, got %q", out)
	}
	if !strings.Contains(out, "5 dead") {
		t.Errorf("expected dead count, got %q", out)
	}

	// Connected peer with session count.
	if !strings.Contains(out, "desktop (1 session)") {
		t.Errorf("expected connected peer, got %q", out)
	}

	// Disconnected peer with error.
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected disconnected peer error, got %q", out)
	}
}

func TestRunAuthNoRunningDaemon(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"auth"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("expected 'not running', got %q", stderr.String())
	}
}

func TestRunAuthWithRunningDaemon(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemon(t, "0.9.0")
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"auth"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "127.0.0.1:8790") {
		t.Errorf("expected listen address in output, got %q", out)
	}
	if !strings.Contains(out, "/auth/login?token=") {
		t.Errorf("expected auth URL in output, got %q", out)
	}
	if !strings.Contains(out, "test-token-abc") {
		t.Errorf("expected token in output, got %q", out)
	}
}

func TestRunLogPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"log-path"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	want := filepath.Join(dir, "gmux", "gmuxd.log")
	if got != want {
		t.Errorf("log-path = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

func TestUsageIncludesNewCommands(t *testing.T) {
	var stdout bytes.Buffer
	printUsage(&stdout)
	out := stdout.String()
	for _, cmd := range []string{"start", "stop", "status", "auth", "remote", "log-path"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage missing command %q", cmd)
		}
	}
	// Old commands should not appear.
	for _, old := range []string{"shutdown", "auth-link", "--replace"} {
		if strings.Contains(out, old) {
			t.Errorf("usage should not contain old command %q", old)
		}
	}
}

// startTestSocketDaemon starts a minimal HTTP server on a Unix socket
// at the standard SocketPath() location under a temp XDG_STATE_HOME.
// Returns the state dir (for t.Setenv) and a cleanup func.
func startTestSocketDaemon(t *testing.T, ver string) (stateDir string, cleanup func()) {
	t.Helper()
	stateDir = t.TempDir()
	sockDir := filepath.Join(stateDir, "gmux")
	os.MkdirAll(sockDir, 0o700)
	sockPath := filepath.Join(sockDir, "gmuxd.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := map[string]any{
			"service":    "gmuxd",
			"version":    ver,
			"status":     "ready",
			"listen":     "127.0.0.1:8790",
			"auth_token": "test-token-abc",
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	return stateDir, func() {
		srv.Close()
		os.Remove(sockPath)
	}
}

// startTestSocketDaemonFull starts a mock daemon that returns sessions and peers.
func startTestSocketDaemonFull(t *testing.T) (stateDir string, cleanup func()) {
	t.Helper()
	stateDir = t.TempDir()
	sockDir := filepath.Join(stateDir, "gmux")
	os.MkdirAll(sockDir, 0o700)
	sockPath := filepath.Join(sockDir, "gmuxd.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := map[string]any{
			"service":    "gmuxd",
			"version":    "1.0.0",
			"status":     "ready",
			"listen":     "127.0.0.1:8790",
			"auth_token": "test-token-abc",
			"sessions": map[string]int{
				"local_alive":  2,
				"remote_alive": 1,
				"dead":         5,
			},
			"peers": []map[string]any{
				{"name": "desktop", "url": "https://desktop.ts.net", "status": "connected", "session_count": 1},
				{"name": "server", "url": "https://server.ts.net", "status": "disconnected", "session_count": 0, "last_error": "connection refused"},
			},
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	return stateDir, func() {
		srv.Close()
		os.Remove(sockPath)
	}
}

// Verify unixipc package is properly usable.
func TestUnixIPCReplace(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	// Replace on nonexistent socket should succeed.
	if err := unixipc.Replace(sockPath); err != nil {
		t.Fatal(err)
	}
}

func TestRunSubcommandHelp(t *testing.T) {
	tests := []struct {
		cmd      string
		contains string
	}{
		{"start", "background"},
		{"run", "foreground"},
		{"restart", "background"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		code := run([]string{tt.cmd, "--help"}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("%s --help: exit code %d, want 0", tt.cmd, code)
		}
		if !strings.Contains(stdout.String(), tt.contains) {
			t.Errorf("%s --help: expected %q in output, got %q", tt.cmd, tt.contains, stdout.String())
		}
	}
}

func TestSnapshotPumpRoute(t *testing.T) {
	cases := []struct {
		eventType    string
		wantSessions bool
		wantWorld    bool
	}{
		// Session changes only fire the sessions snapshot.
		{"session-upsert", true, false},
		{"session-remove", true, false},

		// Peer status only changes the world bundle.
		{"peer-status", false, true},

		// projects-update fires both kinds. Regression guard: prior
		// versions of the pump routed projects-update only to the
		// world coalescer, leaving session ProjectSlug / ProjectIndex
		// stamps unflushed after a projects.json edit.
		{"projects-update", true, true},

		// Activity is delivered separately (bare bus); the pump must
		// not coalesce it.
		{"session-activity", false, false},

		// Unknown / future event types are silently ignored.
		{"", false, false},
		{"unknown-type", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			gotSessions, gotWorld := snapshotPumpRoute(tc.eventType)
			if gotSessions != tc.wantSessions || gotWorld != tc.wantWorld {
				t.Errorf("snapshotPumpRoute(%q) = (sessions=%v, world=%v), want (sessions=%v, world=%v)",
					tc.eventType, gotSessions, gotWorld, tc.wantSessions, tc.wantWorld)
			}
		})
	}
}

func TestShouldForwardActivity(t *testing.T) {
	// Local peer "dc" is a devcontainer; "hub-b" is a network peer.
	isLocalPeer := func(name string) bool { return name == "dc" }

	cases := []struct {
		name      string
		asPeer    bool
		sessionID string
		want      bool
	}{
		// Browser sees everything, regardless of namespace.
		{"browser local session", false, "sess-1", true},
		{"browser devcontainer session", false, "sess-1@dc", true},
		{"browser network-peer session", false, "sess-1@hub-b", true},

		// Hub (asPeer) only sees activity for sessions this node owns.
		{"asPeer local session", true, "sess-1", true},
		{"asPeer devcontainer session", true, "sess-1@dc", true},
		{"asPeer network-peer session dropped", true, "sess-1@hub-b", false},

		// Defense: nil isLocalPeer means “no locals”, so any namespaced
		// id is dropped for asPeer (e.g. peerManager not yet wired).
		{"asPeer nil isLocalPeer drops namespaced", true, "sess-1@dc", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := isLocalPeer
			if tc.name == "asPeer nil isLocalPeer drops namespaced" {
				lookup = nil
			}
			got := shouldForwardActivity(tc.asPeer, tc.sessionID, lookup)
			if got != tc.want {
				t.Errorf("shouldForwardActivity(asPeer=%v, %q) = %v, want %v",
					tc.asPeer, tc.sessionID, got, tc.want)
			}
		})
	}
}

func TestIsAllowedPeerProxyPath(t *testing.T) {
	cases := []struct {
		name   string
		method string
		sub    string
		want   bool
	}{
		// Allowed: project session reorder.
		{"reorder allowed", http.MethodPatch, "v1/projects/gmux/sessions", true},
		{"reorder allowed weird slug", http.MethodPatch, "v1/projects/with-dash/sessions", true},

		// Method must be PATCH.
		{"GET denied", http.MethodGet, "v1/projects/gmux/sessions", false},
		{"POST denied", http.MethodPost, "v1/projects/gmux/sessions", false},
		{"DELETE denied", http.MethodDelete, "v1/projects/gmux/sessions", false},

		// Path shape: must be projects/<slug>/sessions.
		{"reorder root denied", http.MethodPatch, "v1/projects", false},
		{"reorder add denied", http.MethodPatch, "v1/projects/add", false},
		{"projects bare denied", http.MethodPatch, "v1/projects/gmux", false},
		{"sessions endpoint denied", http.MethodPatch, "v1/sessions/sess-1/kill", false},
		{"unrelated path denied", http.MethodPatch, "v1/health", false},

		// Defense: never allow without the v1/ prefix even if shape matches.
		{"missing v1 prefix denied", http.MethodPatch, "projects/gmux/sessions", false},

		// Defense: don't accept random suffixes after /sessions.
		{"trailing path denied", http.MethodPatch, "v1/projects/gmux/sessions/extra", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAllowedPeerProxyPath(tc.method, tc.sub)
			if got != tc.want {
				t.Errorf("isAllowedPeerProxyPath(%q, %q) = %v, want %v",
					tc.method, tc.sub, got, tc.want)
			}
		})
	}
}

func TestIsOwnedEvent(t *testing.T) {
	// Devcontainers are surfaced as Local peers; "tower" is a network
	// peer that should NOT be treated as owned by this node.
	isLocalPeer := func(name string) bool { return name == "dc" }

	cases := []struct {
		name string
		ev   store.Event
		want bool
	}{
		// session-upsert ownership comes from the embedded Session's Peer.
		{
			name: "upsert local session",
			ev:   store.Event{Type: "session-upsert", ID: "sess-1", Session: &store.Session{ID: "sess-1"}},
			want: true,
		},
		{
			name: "upsert devcontainer session",
			ev:   store.Event{Type: "session-upsert", ID: "sess-1@dc", Session: &store.Session{ID: "sess-1@dc", Peer: "dc"}},
			want: true,
		},
		{
			name: "upsert network-peer session dropped",
			ev:   store.Event{Type: "session-upsert", ID: "sess-1@tower", Session: &store.Session{ID: "sess-1@tower", Peer: "tower"}},
			want: false,
		},
		// Defense: a nil Session pointer (shouldn't happen on the wire
		// today, but cheap to handle): treat as owned rather than drop
		// silently and lose a removal-pair upsert.
		{
			name: "upsert nil session passes through",
			ev:   store.Event{Type: "session-upsert", ID: "sess-1"},
			want: true,
		},

		// session-remove only carries an ID; ownership is inferred from
		// the namespace suffix on the ID.
		{name: "remove local id", ev: store.Event{Type: "session-remove", ID: "sess-1"}, want: true},
		{name: "remove devcontainer id", ev: store.Event{Type: "session-remove", ID: "sess-1@dc"}, want: true},
		{name: "remove network-peer id dropped", ev: store.Event{Type: "session-remove", ID: "sess-1@tower"}, want: false},

		// Unknown event types pass through (the SSE handler switches on
		// Type before calling, so this is just a defensive default).
		{name: "unknown type passes through", ev: store.Event{Type: "something-else", ID: "sess-1@tower"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isOwnedEvent(tc.ev, isLocalPeer)
			if got != tc.want {
				t.Errorf("isOwnedEvent(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}

	t.Run("nil isLocalPeer drops any namespaced id", func(t *testing.T) {
		// Defense: if peerManager isn't wired (early startup, test
		// harnesses), we can't confirm a peer is Local, so the safe
		// default for asPeer dispatch is "not owned".
		ev := store.Event{Type: "session-remove", ID: "sess-1@dc"}
		if got := isOwnedEvent(ev, nil); got != false {
			t.Errorf("isOwnedEvent with nil isLocalPeer = %v, want false", got)
		}
	})
}
