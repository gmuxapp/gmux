package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
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

func TestComposePeerProjectsSkipsLocalPeers(t *testing.T) {
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: snapshot.sessions\ndata: {\"sessions\":[]}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"version": "test"}})
		case "/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"data": map[string]any{
					"configured": []map[string]any{{
						"slug":  "gmux",
						"match": []map[string]any{{"path": "/work/gmux"}},
					}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer spoke.Close()

	mgr := peering.NewManager([]config.PeerConfig{
		{Name: "tower", URL: spoke.URL},
		{Name: "devcontainer", URL: spoke.URL, Local: true},
	}, store.New(), "test-host")
	mgr.Start()
	defer mgr.Stop()

	waitForCachedProjects := func(name string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p := mgr.GetPeer(name); p != nil {
				if _, ok := p.CachedProjects(); ok {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s project cache", name)
	}
	waitForCachedProjects("tower")
	waitForCachedProjects("devcontainer")

	got := composePeerProjects(mgr)
	if _, ok := got["devcontainer"]; ok {
		t.Fatalf("composePeerProjects included Local peer: %#v", got)
	}
	projects, ok := got["tower"]
	if !ok {
		t.Fatalf("composePeerProjects missing network peer: %#v", got)
	}
	if len(projects) != 1 || projects[0].Slug != "gmux" || projects[0].LaunchCwd != "/work/gmux" {
		t.Fatalf("tower projects = %#v, want gmux with launch cwd", projects)
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

func TestBuildLaunchArgs(t *testing.T) {
	cmd := []string{"claude", "--continue", "-p", "hi"}

	t.Run("fresh launch: __run then -- then command verbatim", func(t *testing.T) {
		got := buildLaunchArgs("", 0, 0, cmd)
		want := append([]string{"__run", "--"}, cmd...)
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("restart: directives precede --, then the command", func(t *testing.T) {
		got := buildLaunchArgs("sess-abc", 142, 47, cmd)
		want := []string{
			"__run",
			"--resume-id=sess-abc",
			"--initial-cols=142",
			"--initial-rows=47",
			"--",
			"claude", "--continue", "-p", "hi",
		}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("zero dims omit the size flags", func(t *testing.T) {
		got := buildLaunchArgs("sess-1", 0, 0, cmd)
		want := append([]string{"__run", "--resume-id=sess-1", "--"}, cmd...)
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("command flags survive intact (-- terminator)", func(t *testing.T) {
		// The `--` terminator delivers the command verbatim even when its
		// own args look like directive flags.
		got := buildLaunchArgs("sess-1", 80, 24, []string{"weirdcli", "--resume-id=evil"})
		want := []string{
			"__run",
			"--resume-id=sess-1",
			"--initial-cols=80",
			"--initial-rows=24",
			"--",
			"weirdcli", "--resume-id=evil",
		}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// sendSSE must surface write errors so the events handler can treat
// them as a client disconnect and tear the subscriber down (#243).
func TestSendSSEPropagatesWriteError(t *testing.T) {
	if err := sendSSE(failWriter{}, "snapshot.sessions", map[string]int{"a": 1}); err == nil {
		t.Fatal("expected write error to propagate")
	}
	var buf bytes.Buffer
	if err := sendSSE(&buf, "snapshot.sessions", map[string]int{"a": 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "event: snapshot.sessions\ndata: {\"a\":1}\n\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
	// A marshal failure skips the frame; it is a payload bug, not a
	// connection problem, so no error is returned.
	if err := sendSSE(failWriter{}, "x", make(chan int)); err != nil {
		t.Errorf("marshal failure should not report a connection error, got %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// fakeSSEWriter implements the optional ResponseController interfaces
// (FlushError, SetWriteDeadline) so frame-level behavior — fresh
// deadline per write, flush errors surfacing as disconnects — can be
// verified without a real connection.
type fakeSSEWriter struct {
	buf       bytes.Buffer
	writeErr  error
	flushErr  error
	deadlines []time.Time
}

func (f *fakeSSEWriter) Header() http.Header { return http.Header{} }
func (f *fakeSSEWriter) WriteHeader(int)     {}
func (f *fakeSSEWriter) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}
func (f *fakeSSEWriter) FlushError() error { return f.flushErr }
func (f *fakeSSEWriter) SetWriteDeadline(t time.Time) error {
	f.deadlines = append(f.deadlines, t)
	return nil
}

// Every SSE write (frame or heartbeat) must arm a fresh write deadline
// and surface write/flush failures so the events handler tears the
// subscriber down instead of leaking it on a half-dead client (#243).
func TestSSEFrameDeadlineAndErrorPropagation(t *testing.T) {
	fw := &fakeSSEWriter{}
	rc := http.NewResponseController(fw)

	if err := sendSSEFrame(rc, fw, "snapshot.sessions", map[string]int{"a": 1}); err != nil {
		t.Fatalf("healthy write: %v", err)
	}
	if err := sendSSEComment(rc, fw); err != nil {
		t.Fatalf("healthy heartbeat: %v", err)
	}
	if got := len(fw.deadlines); got != 2 {
		t.Fatalf("expected a fresh deadline per write, got %d", got)
	}
	if !fw.deadlines[1].After(time.Now().Add(sseWriteTimeout - time.Minute)) {
		t.Errorf("deadline not re-armed into the future: %v", fw.deadlines[1])
	}
	if !strings.HasSuffix(fw.buf.String(), ":\n\n") {
		t.Errorf("heartbeat comment missing, got %q", fw.buf.String())
	}

	fw = &fakeSSEWriter{writeErr: errors.New("broken pipe")}
	rc = http.NewResponseController(fw)
	if err := sendSSEFrame(rc, fw, "x", 1); err == nil {
		t.Error("frame write error not propagated")
	}
	if err := sendSSEComment(rc, fw); err == nil {
		t.Error("heartbeat write error not propagated")
	}

	fw = &fakeSSEWriter{flushErr: errors.New("deadline exceeded")}
	rc = http.NewResponseController(fw)
	if err := sendSSEFrame(rc, fw, "x", 1); err == nil {
		t.Error("frame flush error not propagated")
	}
	if err := sendSSEComment(rc, fw); err == nil {
		t.Error("heartbeat flush error not propagated")
	}
}

func TestManualPeerResponsePreservesBareWebContract(t *testing.T) {
	peer := centralstore.ManualPeer{Name: "host"}
	for _, tc := range []struct {
		outcome centralstore.PeerUpsertOutcome
		key     string
	}{
		{centralstore.PeerUnchanged, "already_connected"},
		{centralstore.PeerUpdated, "updated"},
	} {
		b, err := json.Marshal(manualPeerResponse(peer, tc.outcome))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["peer"] == nil || got[tc.key] == nil {
			t.Fatalf("response=%s", b)
		}
		if got["ok"] != nil || got["data"] != nil {
			t.Fatalf("wrapped response=%s", b)
		}
	}
}
