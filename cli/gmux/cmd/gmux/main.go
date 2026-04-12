package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/binhash"
	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
	"github.com/gmuxapp/gmux/cli/gmux/internal/naming"
	"github.com/gmuxapp/gmux/cli/gmux/internal/ptyserver"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/workspace"
	"github.com/gmuxapp/gmux/packages/paths"
)

// version is set at build time via -ldflags "-X main.version=..."
// Falls back to "dev" for local builds.
var version = "dev"

func main() {
	log.SetPrefix("gmux: ")
	log.SetFlags(0)

	args := os.Args[1:]

	// No args → open the UI in a browser.
	if len(args) == 0 {
		ensureGmuxd()

		// Wait for gmuxd to be reachable before opening browser.
		client := gmuxdClient()
		baseURL := gmuxdBaseURL()
		var healthBody []byte
		ready := false
		for range 15 {
			if resp, err := client.Get(baseURL + "/v1/health"); err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					healthBody = body
					ready = true
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !ready {
			log.Fatalf("gmuxd is not running (check %s/gmuxd.log for errors)", os.TempDir())
		}

		// Parse health response for TCP address and auth token.
		listenAddr := parseHealthField(healthBody, "listen")
		token := parseHealthField(healthBody, "auth_token")

		browserURL := "http://" + listenAddr
		if token != "" {
			browserURL = fmt.Sprintf("http://%s/auth/login?token=%s", listenAddr, token)
		}

		// Print access URLs.
		fmt.Fprintf(os.Stderr, "  local:  http://%s\n", listenAddr)
		if tsURL := parseTailscaleURL(healthBody); tsURL != "" {
			fmt.Fprintf(os.Stderr, "  remote: %s\n", maskTailscaleURL(tsURL))
		}
		if updateVer := parseUpdateAvailable(healthBody); updateVer != "" {
			fmt.Fprintf(os.Stderr, "  update: %s available — %s\n", updateVer, upgradeHint())
		}

		openBrowser(browserURL)
		return
	}

	// Nested gmux detection: if we're running interactively inside an
	// existing gmux session, re-exec as a detached headless process instead
	// of doing PTY passthrough (which would nest PTY-within-PTY). The
	// detached process registers with gmuxd and the session appears in the
	// gmux UI. The original process returns immediately to the parent shell.
	if os.Getenv("GMUX") == "1" && localterm.IsInteractive() {
		self, err := os.Executable()
		if err != nil {
			log.Fatalf("cannot find own binary: %v", err)
		}
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			log.Fatalf("cannot open %s: %v", os.DevNull, err)
		}
		defer devNull.Close()

		cmd := exec.Command(self, os.Args[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		if err := cmd.Start(); err != nil {
			log.Fatalf("failed to start background session: %v", err)
		}
		cmd.Process.Release()
		fmt.Fprintf(os.Stderr, "started %s in background (visible in gmux)\n", strings.Join(args, " "))
		return
	}

	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine cwd: %v", err)
	}

	sessionID := naming.SessionID()
	socketDir := os.Getenv("GMUX_SOCKET_DIR")
	if socketDir == "" {
		socketDir = "/tmp/gmux-sessions"
	}
	sockPath := filepath.Join(socketDir, sessionID+".sock")

	// Resolve adapter — registered adapters first, shell fallback
	registry := adapter.NewRegistry()
	for _, a := range adapters.All {
		registry.Register(a)
	}
	registry.SetFallback(adapters.DefaultFallback())
	a := registry.Resolve(args)

	// Get adapter-specific env vars
	adapterEnv := a.Env(adapter.EnvContext{
		Cwd:        workDir,
		SessionID:  sessionID,
		SocketPath: sockPath,
	})

	// Detect VCS workspace root and remotes for grouping related sessions.
	wsRoot := workspace.DetectRoot(workDir)
	remotes := workspace.DetectRemotes(wsRoot)

	// Create in-memory session state
	state := session.New(session.Config{
		ID:            sessionID,
		Command:       args,
		Cwd:           workDir,
		Kind:          a.Name(),
		WorkspaceRoot: wsRoot,
		Remotes:       remotes,
		SocketPath:    sockPath,
		BinaryHash:    binhash.Self(),
		RunnerVersion: version,
	})

	// Common env vars — set for every child, per ADR-0005
	env := []string{
		"GMUX=1",
		"GMUX_SOCKET=" + sockPath,
		"GMUX_SESSION_ID=" + sessionID,
		"GMUX_ADAPTER=" + a.Name(),
		"GMUX_RUNNER_VERSION=" + version,
	}
	env = append(env, adapterEnv...)

	interactive := localterm.IsInteractive()

	// Determine initial PTY size — use terminal size if interactive
	ptyCfg := ptyserver.Config{
		Command:    args,
		Cwd:        workDir,
		Env:        env,
		SocketPath: sockPath,
		Adapter:    a,
		State:      state,
	}
	// Always try to inherit terminal dimensions from the parent.
	// Even non-interactive launches (background, piped) benefit from
	// a real size: the PTY and virtual terminal start correctly sized
	// instead of falling back to 80x24.
	if cols, rows, err := localterm.TerminalSize(); err == nil {
		ptyCfg.Cols = cols
		ptyCfg.Rows = rows
	}

	if !interactive {
		fmt.Printf("session:  %s\n", sessionID)
		fmt.Printf("adapter:  %s\n", a.Name())
		fmt.Printf("command:  %s\n", strings.Join(args, " "))
	}

	// Start PTY server
	srv, err := ptyserver.New(ptyCfg)
	if err != nil {
		log.Fatalf("failed to start: %v", err)
	}

	state.SetRunning(srv.Pid())

	if !interactive {
		fmt.Printf("pid:      %d\n", srv.Pid())
		fmt.Printf("socket:   %s\n", srv.SocketPath())
		fmt.Println("serving...")
	}

	// Auto-start gmuxd if not running (one-shot, never retried), then register.
	ensureGmuxd()
	go registerWithGmuxd(sessionID, sockPath)

	if interactive {
		// Transparent mode: attach local terminal to the PTY
		attach, err := localterm.New(localterm.Config{
			PTYWriter: ptyWriterFunc(func(p []byte) (int, error) {
				return srv.WritePTY(p)
			}),
			ResizeFn: srv.Resize,
		})
		if err != nil {
			log.Fatalf("failed to attach terminal: %v", err)
		}
		srv.SetLocalOutput(attach)

		// In interactive mode:
		// - SIGHUP → detach local terminal, keep session running
		// - SIGINT/SIGTERM are consumed by raw mode and forwarded to child via PTY
		//   (but we still catch them on gmux in case raw mode is somehow bypassed)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-srv.Done():
			// Child exited — detach and exit
			attach.Detach()
		case <-attach.Done():
			// Local terminal gone (stdin closed) — session continues headless
			srv.SetLocalOutput(nil)
			// Wait for child to exit (session persists, accessible via web UI)
			<-srv.Done()
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				// Terminal closed — detach, keep session alive
				attach.Detach()
				srv.SetLocalOutput(nil)
				// Continue running headless until child exits
				<-srv.Done()
			} else {
				// SIGINT/SIGTERM — clean shutdown
				attach.Detach()
				srv.Shutdown()
			}
		}
	} else {
		// Non-interactive: original behavior
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-srv.Done():
			// Child exited
		case sig := <-sigCh:
			fmt.Printf("\nreceived %v, shutting down...\n", sig)
			srv.Shutdown()
		}
	}

	exitCode := srv.ExitCode()
	state.SetExited(exitCode)

	// Deregister from gmuxd (best-effort)
	deregisterFromGmuxd(sessionID)

	if !interactive {
		fmt.Printf("exited:   %d\n", exitCode)
	}
	os.Exit(exitCode)
}

// ensureGmuxd checks if gmuxd is reachable and starts it if not.
// If a daemon is running but reports a different version, it is replaced
// so the child process always talks to a compatible daemon.
// Called once at startup — if gmuxd dies later, we don't restart it.
// Returns true if gmuxd was started (or replaced) by this call.
func ensureGmuxd() bool {
	if !gmuxdNeedsStart() {
		return false
	}

	gmuxdBin := findGmuxdBin()
	if gmuxdBin == "" {
		log.Printf("warning: gmuxd not found (install it alongside gmux or add it to PATH)")
		return false
	}

	// gmuxd run starts in the foreground; we background it ourselves.
	return startGmuxd(gmuxdBin, []string{"run"})
}

// gmuxdNeedsStart checks the running daemon.
func gmuxdNeedsStart() bool {
	// "dev" builds never replace — avoids churn during development.
	if version == "dev" {
		return !gmuxdHealthy(500 * time.Millisecond)
	}

	client := gmuxdClient()
	client.Timeout = 500 * time.Millisecond
	resp, err := client.Get(gmuxdBaseURL() + "/v1/health")
	if err != nil {
		return true // not running
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true // not healthy
	}

	var health struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(body, &health) != nil {
		return false // can't parse, leave it alone
	}

	// Same version: no action needed. Different version: replace.
	return health.Data.Version != version
}

// findGmuxdBin locates the gmuxd binary: sibling first, then PATH.
func findGmuxdBin() string {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "gmuxd")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("gmuxd"); err == nil {
		return p
	}
	return ""
}

// startGmuxd launches gmuxd in the background with the given args.
func startGmuxd(gmuxdBin string, args []string) bool {
	// Log gmuxd output to a file so users can diagnose startup failures.
	logPath := filepath.Join(os.TempDir(), "gmuxd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logFile = nil
	}

	cmd := exec.Command(gmuxdBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		log.Printf("warning: could not start gmuxd: %v", err)
		if logFile != nil {
			logFile.Close()
		}
		return false
	}
	go func() {
		cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
	}()

	return true
}

// gmuxdClient returns an HTTP client connected to gmuxd via Unix socket.
func gmuxdClient() *http.Client {
	sockPath := paths.SocketPath()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 2*time.Second)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// gmuxdBaseURL returns the base URL for gmuxd HTTP requests.
// The host is ignored by the Unix socket transport.
func gmuxdBaseURL() string {
	return "http://localhost"
}

func gmuxdHealthy(timeout time.Duration) bool {
	client := gmuxdClient()
	client.Timeout = timeout
	resp, err := client.Get(gmuxdBaseURL() + "/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func registerWithGmuxd(sessionID, socketPath string) {
	baseURL := gmuxdBaseURL()

	payload, _ := json.Marshal(map[string]string{
		"session_id":  sessionID,
		"socket_path": socketPath,
	})

	// Retry a few times — gmux may start before the HTTP server is ready
	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		client := gmuxdClient()
		resp, err := client.Post(baseURL+"/v1/register", "application/json", bytes.NewReader(payload))
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return
		}
	}
}

// ptyWriterFunc is an adapter to use a function as an io.Writer.
type ptyWriterFunc func([]byte) (int, error)

func (f ptyWriterFunc) Write(p []byte) (int, error) { return f(p) }

// openBrowser opens the gmux UI. Prefers Chrome/Chromium in --app mode
// for a standalone window; falls back to the default browser.
func openBrowser(url string) {

	// Strategy: default browser if Chromium-based → app mode, else
	// any installed Chromium → app mode, else system default.
	if tryDefaultBrowserAppMode(url) {
		return
	}
	if tryAnyChromiumAppMode(url) {
		return
	}

	// Fallback: default browser (normal tab).
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

// tryDefaultBrowserAppMode checks if the user's default browser is
// Chromium-based and launches it in --app mode.
func tryDefaultBrowserAppMode(url string) bool {
	switch runtime.GOOS {
	case "darwin":
		bundleID := defaultBrowserBundleID()
		if binary, ok := macOSChromiumBinary(bundleID); ok {
			return startDetached(exec.Command(binary, "--app="+url))
		}
	default:
		desktop := defaultDesktopBrowser()
		if isChromiumDesktop(desktop) {
			// The default browser is Chromium-based — xdg-open won't pass
			// --app, but the binary should be on PATH with a known name.
			return tryAnyChromiumAppMode(url)
		}
	}
	return false
}

// tryAnyChromiumAppMode finds any installed Chromium-based browser and
// launches it with --app.
func tryAnyChromiumAppMode(url string) bool {
	switch runtime.GOOS {
	case "darwin":
		// macOS: Chrome.app doesn't put a binary on $PATH.
		// Check known .app bundle locations directly.
		home, _ := os.UserHomeDir()
		appDirs := []string{"/Applications", filepath.Join(home, "Applications")}
		for _, app := range []string{"Google Chrome", "Chromium"} {
			for _, dir := range appDirs {
				binary := filepath.Join(dir, app+".app", "Contents", "MacOS", app)
				if _, err := os.Stat(binary); err == nil {
					if startDetached(exec.Command(binary, "--app="+url)) {
						return true
					}
				}
			}
		}
	default:
		for _, name := range []string{"google-chrome-stable", "google-chrome", "chromium-browser", "chromium"} {
			if p, err := exec.LookPath(name); err == nil {
				if startDetached(exec.Command(p, "--app="+url)) {
					return true
				}
			}
		}
	}
	return false
}

// startDetached starts a command in a new session so it outlives gmux.
func startDetached(cmd *exec.Cmd) bool {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start() == nil
}

// --- default browser detection ---

// defaultBrowserBundleID returns the macOS bundle ID of the default
// HTTPS handler (e.g. "com.google.chrome"). Returns "" if Safari is
// the implicit default or detection fails.
func defaultBrowserBundleID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	plistPath := filepath.Join(home,
		"Library", "Preferences", "com.apple.LaunchServices",
		"com.apple.launchservices.secure.plist")
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", plistPath).Output()
	if err != nil {
		return ""
	}
	var plist struct {
		LSHandlers []struct {
			URLScheme string `json:"LSHandlerURLScheme"`
			RoleAll   string `json:"LSHandlerRoleAll"`
		} `json:"LSHandlers"`
	}
	if err := json.Unmarshal(out, &plist); err != nil {
		return ""
	}
	for _, h := range plist.LSHandlers {
		if strings.EqualFold(h.URLScheme, "https") {
			return h.RoleAll
		}
	}
	return "" // Safari is implicit default
}

// macOSChromiumBinary maps a bundle ID to its binary path if it's a
// known Chromium-based browser.
func macOSChromiumBinary(bundleID string) (string, bool) {
	// Map bundle IDs → .app names for known Chromium-based browsers.
	appNames := map[string]string{
		"com.google.chrome":          "Google Chrome",
		"org.chromium.chromium":      "Chromium",
		"company.thebrowser.browser": "Arc",
		"com.brave.browser":          "Brave Browser",
		"com.microsoft.edgemac":      "Microsoft Edge",
	}
	appName, ok := appNames[strings.ToLower(bundleID)]
	if !ok {
		return "", false
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
		binary := filepath.Join(dir, appName+".app", "Contents", "MacOS", appName)
		if _, err := os.Stat(binary); err == nil {
			return binary, true
		}
	}
	return "", false
}

// defaultDesktopBrowser returns the .desktop file name of the default
// web browser on Linux (e.g. "google-chrome.desktop").
func defaultDesktopBrowser() string {
	out, err := exec.Command("xdg-settings", "get", "default-web-browser").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isChromiumDesktop returns true if the .desktop name looks Chromium-based.
func isChromiumDesktop(desktop string) bool {
	d := strings.ToLower(desktop)
	return strings.Contains(d, "chrome") || strings.Contains(d, "chromium")
}

// parseHealthField extracts a string field from the data object
// of a /v1/health JSON response.
func parseHealthField(body []byte, field string) string {
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	raw, ok := resp.Data[field]
	if !ok {
		return ""
	}
	var val string
	if json.Unmarshal(raw, &val) != nil {
		return ""
	}
	return val
}

// parseTailscaleURL extracts the tailscale_url from a /v1/health JSON response.
func parseTailscaleURL(body []byte) string {
	var resp struct {
		Data struct {
			TailscaleURL string `json:"tailscale_url"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &resp) == nil {
		return resp.Data.TailscaleURL
	}
	return ""
}

// parseUpdateAvailable extracts update_available from a /v1/health JSON response.
func parseUpdateAvailable(body []byte) string {
	var resp struct {
		Data struct {
			UpdateAvailable string `json:"update_available"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &resp) == nil {
		return resp.Data.UpdateAvailable
	}
	return ""
}

// upgradeHint returns the appropriate upgrade command based on how gmux was installed.
func upgradeHint() string {
	self, err := os.Executable()
	if err != nil {
		return "curl -sSfL https://gmux.app/install.sh | sh"
	}
	// Resolve symlinks to find the real binary location
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		real = self
	}
	// Check if we're inside a Homebrew prefix
	if strings.Contains(real, "/Cellar/") || strings.Contains(real, "/homebrew/") {
		return "brew upgrade gmuxapp/tap/gmux"
	}
	return "curl -sSfL https://gmux.app/install.sh | sh"
}

// maskTailscaleURL masks the tailnet name for privacy.
// "https://gmux.angler-map.ts.net" → "https://gmux.an******.ts.net"
func maskTailscaleURL(url string) string {
	// Find the tailnet part: between first dot after hostname and .ts.net
	tsNet := ".ts.net"
	idx := strings.Index(url, tsNet)
	if idx < 0 {
		return url
	}
	// Find the start of the tailnet name (after "https://gmux.")
	schemeEnd := strings.Index(url, "://")
	if schemeEnd < 0 {
		return url
	}
	hostStart := schemeEnd + 3
	// Find first dot after the hostname prefix
	dotIdx := strings.Index(url[hostStart:], ".")
	if dotIdx < 0 {
		return url
	}
	tailnetStart := hostStart + dotIdx + 1
	tailnetName := url[tailnetStart:idx]
	if len(tailnetName) <= 2 {
		return url
	}
	masked := tailnetName[:2] + "****"
	return url[:tailnetStart] + masked + url[idx:]
}

func deregisterFromGmuxd(sessionID string) {
	baseURL := gmuxdBaseURL()

	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})
	client := gmuxdClient()
	resp, err := client.Post(baseURL+"/v1/deregister", "application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	resp.Body.Close()
}
