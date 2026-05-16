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
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
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

func TestRunStatusShowsSessionList(t *testing.T) {
	stateDir, cleanup := startTestSocketDaemonFull(t)
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()

	// Alive session: ID, pid, socket, title, terminal size, status label.
	if !strings.Contains(out, "sess-aabbccdd") {
		t.Errorf("expected alive session ID in output, got:\n%s", out)
	}
	if !strings.Contains(out, "99001") {
		t.Errorf("expected pid 99001 in output, got:\n%s", out)
	}
	if !strings.Contains(out, "PID") {
		t.Errorf("expected PID column header in output, got:\n%s", out)
	}
	if !strings.Contains(out, "/tmp/gmux-sessions/sess-aabbccdd.sock") {
		t.Errorf("expected socket path in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Fix scrollback bug") {
		t.Errorf("expected session title in output, got:\n%s", out)
	}
	if !strings.Contains(out, "thinking...") {
		t.Errorf("expected status label in output, got:\n%s", out)
	}
	if !strings.Contains(out, "80\u00d724") {
		t.Errorf("expected terminal size (80\u00d724) in output, got:\n%s", out)
	}

	// Dead session: ID and resumable marker.
	if !strings.Contains(out, "sess-11223344") {
		t.Errorf("expected dead session ID in output, got:\n%s", out)
	}
	if !strings.Contains(out, "resumable") {
		t.Errorf("expected resumable in output, got:\n%s", out)
	}
	// Exit code for dead session.
	if !strings.Contains(out, "code=0") {
		t.Errorf("expected exit code in output, got:\n%s", out)
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
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		exitCode := 0
		exitedAt := time.Now().Add(-37 * time.Minute).Format(time.RFC3339)
		sessions := []map[string]any{
			{
				"id":           "sess-aabbccdd",
				"kind":         "pi",
				"title":        "Fix scrollback bug",
				"alive":        true,
				"pid":          99001,
				"cwd":          "/home/user/project",
				"socket_path":  "/tmp/gmux-sessions/sess-aabbccdd.sock",
				"terminal_cols": 80,
				"terminal_rows": 24,
				"status":       map[string]any{"label": "thinking...", "working": true},
				"started_at":   time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			},
			{
				"id":        "sess-11223344",
				"kind":      "shell",
				"title":     "bash",
				"alive":     false,
				"cwd":       "/home/user/other",
				"exit_code": &exitCode,
				"exited_at": exitedAt,
				"command":   []string{"bash"},
				"resumable": true,
			},
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": sessions})
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

// ── fsGuardPath tests ──

func TestFsGuardPath(t *testing.T) {
	root := "/home/user/myproject"

	tests := []struct {
		name    string
		rel     string
		wantAbs string
		wantErr bool
	}{
		{"empty rel resolves to root", "", root, false},
		{"dot rel resolves to root", ".", root, false},
		{"simple file", "README.md", root + "/README.md", false},
		{"nested path", "src/main.go", root + "/src/main.go", false},
		{"deep nested", "a/b/c/d.txt", root + "/a/b/c/d.txt", false},
		{"dot-dot escape", "../etc/passwd", "", true},
		{"double dot-dot", "../../root", "", true},
		{"absolute path injection", "/etc/passwd", "", true},
		{"embedded dot-dot", "src/../../etc/passwd", "", true},
		{"trailing slash cleaned", "src/", root + "/src", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fsGuardPath(root, tt.rel)
			if tt.wantErr {
				if err == nil {
					t.Errorf("fsGuardPath(%q, %q) = %q, want error", root, tt.rel, got)
				}
				return
			}
			if err != nil {
				t.Errorf("fsGuardPath(%q, %q) unexpected error: %v", root, tt.rel, err)
				return
			}
			if got != tt.wantAbs {
				t.Errorf("fsGuardPath(%q, %q) = %q, want %q", root, tt.rel, got, tt.wantAbs)
			}
		})
	}
}

func TestFsListDir(t *testing.T) {
	dir := t.TempDir()

	// Create: a hidden file, two regular files, a subdirectory.
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(".hidden", "secret")
	writeFile("b_file.txt", "b")
	writeFile("a_file.txt", "a")
	if err := os.Mkdir(filepath.Join(dir, "mydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	entries, err := fsListDir(dir, false)
	if err != nil {
		t.Fatalf("fsListDir: %v", err)
	}

	// Expect: hidden file excluded, dirs first, then files alpha.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Name != "mydir" || entries[0].Type != "dir" {
		t.Errorf("entries[0] = %+v, want dir mydir", entries[0])
	}
	if entries[1].Name != "a_file.txt" || entries[1].Type != "file" {
		t.Errorf("entries[1] = %+v, want file a_file.txt", entries[1])
	}
	if entries[2].Name != "b_file.txt" || entries[2].Type != "file" {
		t.Errorf("entries[2] = %+v, want file b_file.txt", entries[2])
	}

	// With showHidden=true the hidden file should appear (sorted before others).
	allEntries, err := fsListDir(dir, true)
	if err != nil {
		t.Fatalf("fsListDir(showHidden): %v", err)
	}
	if len(allEntries) != 4 {
		t.Fatalf("expected 4 entries with showHidden, got %d: %+v", len(allEntries), allEntries)
	}
	hiddenSeen := false
	for _, e := range allEntries {
		if e.Name == ".hidden" {
			hiddenSeen = true
		}
	}
	if !hiddenSeen {
		t.Error("expected .hidden to appear when showHidden=true")
	}
}

func TestFileOpenerFor(t *testing.T) {
	cfg := config.DefaultFileOpeners()
	cases := []struct {
		path string
		want string
	}{
		// markdown
		{"README.md", "glow"},
		{"docs/NOTES.MD", "glow"},
		// images
		{"photo.png", "chafa"},
		{"photo.PNG", "chafa"},
		{"banner.jpg", "chafa"},
		{"banner.jpeg", "chafa"},
		{"anim.gif", "chafa"},
		{"preview.webp", "chafa"},
		{"icon.bmp", "chafa"},
		{"logo.svg", "chafa"},
		{"scan.tiff", "chafa"},
		{"scan.tif", "chafa"},
		{"favicon.ico", "chafa"},
		{"pic.avif", "chafa"},
		// everything else → helix
		{"main.go", "hx"},
		{"config.toml", "hx"},
		{"script.sh", "hx"},
		{"data.json", "hx"},
		{"no_extension", "hx"},
	}
	for _, tc := range cases {
		got := fileOpenerFor(tc.path, cfg)
		if got != tc.want {
			t.Errorf("fileOpenerFor(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestFileOpenerForWithConfig(t *testing.T) {
	defaultCfg := config.DefaultFileOpeners()

	// Default behaviour preserved.
	if got := fileOpenerFor("README.md", defaultCfg); got != "glow" {
		t.Errorf("md default: got %q, want glow", got)
	}
	if got := fileOpenerFor("photo.png", defaultCfg); got != "chafa" {
		t.Errorf("png default: got %q, want chafa", got)
	}
	if got := fileOpenerFor("main.go", defaultCfg); got != "hx" {
		t.Errorf("go default: got %q, want hx", got)
	}

	// User overrides a single extension.
	custom := defaultCfg
	custom.Extensions = make(map[string]string)
	for k, v := range defaultCfg.Extensions {
		custom.Extensions[k] = v
	}
	custom.Extensions["go"] = "nano"
	if got := fileOpenerFor("main.go", custom); got != "nano" {
		t.Errorf("go override: got %q, want nano", got)
	}
	// Other defaults still intact.
	if got := fileOpenerFor("README.md", custom); got != "glow" {
		t.Errorf("md after override: got %q, want glow", got)
	}

	// User changes the default fallback.
	custom.Default = "vim"
	if got := fileOpenerFor("unknown.xyz", custom); got != "vim" {
		t.Errorf("custom default: got %q, want vim", got)
	}
}

func TestParseGitShortstat(t *testing.T) {
	cases := []struct {
		input string
		files int
		ins   int
		del   int
	}{
		// typical full line
		{" 5 files changed, 120 insertions(+), 34 deletions(-)", 5, 120, 34},
		// single file, insertions only
		{" 1 file changed, 1 insertion(+)", 1, 1, 0},
		// single file, deletions only
		{" 1 file changed, 1 deletion(-)", 1, 0, 1},
		// no insertions
		{" 2 files changed, 5 deletions(-)", 2, 0, 5},
		// no deletions
		{" 3 files changed, 10 insertions(+)", 3, 10, 0},
		// empty / not a git repo
		{"", 0, 0, 0},
		// trailing newline
		{" 2 files changed, 3 insertions(+), 1 deletion(-)\n", 2, 3, 1},
	}
	for _, tc := range cases {
		files, ins, del := parseGitShortstat(tc.input)
		if files != tc.files || ins != tc.ins || del != tc.del {
			t.Errorf("parseGitShortstat(%q) = (%d, %d, %d), want (%d, %d, %d)",
				tc.input, files, ins, del, tc.files, tc.ins, tc.del)
		}
	}
}
