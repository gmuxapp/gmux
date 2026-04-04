package main

import (
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/authtoken"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/binhash"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/netauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/notify"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionfiles"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/update"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/wsproxy"
	"nhooyr.io/websocket"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

type LaunchConfig struct {
	DefaultLauncher string             `json:"default_launcher"`
	Launchers       []adapter.Launcher `json:"launchers"`
}

// discoverLaunchers derives launchers from the compiled adapter set and keeps
// only the adapters that are available on this machine.
func discoverLaunchers() LaunchConfig {
	adapterList := append([]adapter.Adapter{}, adapters.All...)
	adapterList = append(adapterList, adapters.DefaultFallback())

	availableByName := discoverAvailableAdapters(adapterList)
	launchers := launchersForAdapters(adapterList, availableByName)

	log.Printf("launchers: discovered %d adapter(s): %v", len(launchers), launcherStates(launchers))
	return LaunchConfig{
		DefaultLauncher: "shell",
		Launchers:       launchers,
	}
}

func discoverAvailableAdapters(adapterList []adapter.Adapter) map[string]bool {
	availableByName := make(map[string]bool, len(adapterList))

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range adapterList {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			available := a.Discover()
			mu.Lock()
			availableByName[a.Name()] = available
			mu.Unlock()
		}()
	}
	wg.Wait()

	return availableByName
}

func launchersForAdapters(adapterList []adapter.Adapter, availableByName map[string]bool) []adapter.Launcher {
	var launchers []adapter.Launcher
	seen := map[string]struct{}{}

	for _, a := range adapterList {
		launchable, ok := a.(adapter.Launchable)
		if !ok {
			continue
		}
		for _, l := range launchable.Launchers() {
			if _, ok := seen[l.ID]; ok {
				continue
			}
			if !availableByName[a.Name()] {
				continue
			}
			seen[l.ID] = struct{}{}
			l.Available = true
			launchers = append(launchers, l)
		}
	}

	return launchers
}

// resolveGmux finds the gmux binary.
// Priority: sibling to this binary > PATH lookup.
// Both gmuxd and gmux are always installed to the same directory.
func resolveGmux() string {
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "gmux")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("gmux"); err == nil {
		return p
	}
	return ""
}

func launcherStates(ls []adapter.Launcher) []string {
	states := make([]string, len(ls))
	for i, l := range ls {
		state := "unavailable"
		if l.Available {
			state = "available"
		}
		states[i] = fmt.Sprintf("%s(%s)", l.ID, state)
	}
	return states
}

// launchGmux starts a detached gmux process with the given command and cwd.
// Returns the PID on success.
// filterEnvPrefix returns env with any variable starting with prefix removed.
func filterEnvPrefix(env []string, prefix string) []string {
	result := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			result = append(result, e)
		}
	}
	return result
}

func launchGmux(gmuxBin string, command []string, cwd string) (int, error) {
	cmd := exec.Command(gmuxBin, command...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	// Strip all GMUX_* session vars so child processes don't inherit
	// the parent session's identity. Without this, a gmuxd started
	// inside a pi session would leak GMUX_ADAPTER=pi, GMUX_SOCKET,
	// GMUX_SESSION_ID, etc. into every launched session.
	cmd.Env = filterEnvPrefix(os.Environ(), "GMUX_")

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `gmuxd %s

Usage: gmuxd [command]

Commands:
  start              Start the gmux daemon (default if no command given)
  stop               Stop the running daemon
  status             Show daemon health, listeners, and sessions
  auth               Show the auth URL and token
  remote             Set up or check remote access via Tailscale
  version            Show gmuxd version
  help               Show this help

Tip:
  gmux <command>     Run a command; gmux auto-starts gmuxd if needed
  More help: https://gmux.app
`, version)
}



func run(args []string, stdout, stderr io.Writer) int {
	// No args = start daemon (the "put me in a service file" invocation).
	if len(args) == 0 {
		return serve(stderr)
	}

	cmd := args[0]
	args = args[1:]

	switch cmd {
	case "start":
		for _, arg := range args {
			switch arg {
			case "-h", "--help":
				_, _ = fmt.Fprintf(stdout, "Usage: gmuxd start\n\nStarts the daemon, replacing any existing instance.\n")
				return 0
			default:
				_, _ = fmt.Fprintf(stderr, "gmuxd start: unknown option %q\n", arg)
				return 2
			}
		}
		return serve(stderr)
	case "stop":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd stop: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		sock := paths.SocketPath()
		if unixipc.Shutdown(sock) {
			_, _ = fmt.Fprintf(stdout, "gmuxd: stopped\n")
		} else {
			_, _ = fmt.Fprintf(stdout, "gmuxd: no running daemon found\n")
		}
		return 0
	case "status":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd status: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		return runStatus(stdout, stderr)
	case "auth":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd auth: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		return runAuth(stdout, stderr)
	case "remote":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd remote: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		return runRemote(stdout, stderr)
	case "version":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd version: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", version)
		return 0
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "gmuxd: unknown command %q\n\n", cmd)
		printUsage(stderr)
		return 2
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func serve(stderr io.Writer) int {
	gmuxBin := resolveGmux() // resolve once, use everywhere
	if gmuxBin != "" {
		log.Printf("gmux: %s", gmuxBin)
		h := binhash.File(gmuxBin)
		if h != "" {
			discovery.ExpectedRunnerHash = h
			log.Printf("gmux hash: %s…", h[:12])
		}
	}
	launchConfig := discoverLaunchers()

	sessions := store.New()

	// Build command titlers from adapters that implement CommandTitler.
	commandTitlers := make(map[string]func([]string) string)
	for _, a := range adapters.AllAdapters() {
		if ct, ok := a.(adapter.CommandTitler); ok {
			ct := ct // capture for closure
			commandTitlers[a.Name()] = ct.CommandTitle
		}
	}
	sessions.SetCommandTitlers(commandTitlers)

	subs := discovery.NewSubscriptions(sessions)
	pendingResumes := discovery.NewPendingResumes()
	var resumeMu sync.Mutex

	// Start file monitor — watches adapter session directories with inotify
	// to extract title and working status from JSONL files.
	fileMon := discovery.NewFileMonitor(sessions)

	// When a session exits, derive the resume command so it transitions
	// to resumable immediately — no "exited" limbo state.
	subs.OnExit = func(sess *store.Session) bool {
		if cmd := fileMon.ResolveResumeCommand(sess); cmd != nil {
			sess.Command = cmd
			sess.Status = nil // clear exit status for clean resumable display
			return true
		}
		return false
	}
	stopFileMon := make(chan struct{})
	go fileMon.Run(stopFileMon)
	defer close(stopFileMon)

	// Start socket-based discovery (scans /tmp/gmux-sessions/*.sock)
	// Discovery also subscribes to each runner's /events SSE for live updates.
	stopDiscovery := make(chan struct{})
	go discovery.Watch(sessions, subs, fileMon, pendingResumes, 3*time.Second, stopDiscovery)
	defer close(stopDiscovery)

	// Start session file scanner — discovers resumable sessions from
	// adapter session files (e.g. pi's JSONL conversations). Also purges
	// stale dead sessions that were never attributed to a file.
	scanner := sessionfiles.New(sessions)
	stopScanner := make(chan struct{})
	go scanner.Run(30*time.Second, stopScanner)
	defer close(stopScanner)

	// Start background update checker
	updateChecker := update.New(version)

	// ── Presence + Notification router ──

	notifRouter := (*notify.Router)(nil) // assigned after presence table
	presenceTable := presence.New(presence.Callbacks{
		OnClientFocused: func(clientID string) {
			if notifRouter != nil {
				notifRouter.CancelAllPending()
			}
		},
		OnSessionSelected: func(clientID, sessionID string) {
			if notifRouter != nil {
				notifRouter.CancelForSession(sessionID)
			}
		},
	})
	notifRouter = notify.New(presenceTable, sessions, notify.DefaultConfig())
	notifCtx, notifCancel := context.WithCancel(context.Background())
	go notifRouter.Run(notifCtx)
	defer notifCancel()

	mux := http.NewServeMux()

	// tsListener is set below if tailscale is enabled. Declared here so
	// the health handler can include the tailscale URL.
	var tsListener *tsauth.Listener

	// tcpAddr and authToken are resolved after config load. Declared here
	// so the health handler can report the address.
	var tcpAddr string
	var authToken string

	// State directory for persistent files (projects.json, auth-token, etc).
	stateDir := paths.StateDir()

	// Project manager handles concurrent access to projects.json and
	// auto-assignment of sessions to projects.
	projectMgr := projects.NewManager(stateDir)
	projectMgr.Broadcast = func() {
		sessions.Broadcast(store.Event{Type: "projects-update"})
	}

	// Auto-assign sessions to projects when they appear or get a ResumeKey.
	sessionEvents, unsubSessionEvents := sessions.Subscribe()
	defer unsubSessionEvents()
	go func() {
		for ev := range sessionEvents {
			if ev.Type != "session-upsert" || ev.Session == nil {
				continue
			}
			s := ev.Session
			// Only auto-assign alive sessions. Dead resumable sessions
			// stay in the array if already persisted from a previous run,
			// but we don't bulk-add hundreds of old session files on startup.
			if !s.Alive {
				continue
			}
			projectMgr.AutoAssignSession(projects.SessionInfo{
				ID:            s.ID,
				Cwd:           s.Cwd,
				WorkspaceRoot: s.WorkspaceRoot,
				Remotes:       s.Remotes,
				Alive:         s.Alive,
				ResumeKey:     s.ResumeKey,
			})
		}
	}()

	// ── Health + Capabilities ──

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"service": "gmuxd",
			"version": version,
			"node_id": "node-local",
			"status":  "ready",
		}
		if tsListener != nil {
			diag := tsListener.Diag()
			if diag.FQDN != "" {
				data["tailscale_url"] = "https://" + diag.FQDN
			}
			data["tailscale"] = diag
		}
		data["listen"] = tcpAddr
		if v := updateChecker.Available(); v != "" {
			data["update_available"] = v
		}
		// Include auth token only on Unix socket connections (local IPC).
		// On TCP, the requester already proved they have the token.
		if r.RemoteAddr == "@" || strings.HasPrefix(r.RemoteAddr, "/") || r.RemoteAddr == "" {
			data["auth_token"] = authToken
		}
		writeJSON(w, map[string]any{"ok": true, "data": data})
	})

	mux.HandleFunc("/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok": true,
			"data": map[string]any{
				"adapters": []string{"pi", "shell"},
				"transport": map[string]any{
					"kind":   "websocket",
					"replay": true,
				},
			},
		})
	})

	// ── Config ──

	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		cfg := launchConfig
		writeJSON(w, map[string]any{"ok": true, "data": cfg})
	})

	// Frontend config (read from disk on each request so users can edit
	// and refresh without restarting gmuxd).
	mux.HandleFunc("GET /v1/frontend-config", func(w http.ResponseWriter, r *http.Request) {
		theme, themeErr := config.LoadTheme()
		settings, settingsErr := config.LoadSettings()
		if themeErr != nil {
			log.Printf("frontend-config: theme: %v", themeErr)
		}
		if settingsErr != nil {
			log.Printf("frontend-config: settings: %v", settingsErr)
		}
		writeJSON(w, map[string]any{
			"ok": true,
			"data": map[string]any{
				"theme":    theme,
				"settings": settings,
			},
		})
	})

	// ── Projects ──

	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		state, err := projectMgr.Load()
		if err != nil {
			log.Printf("projects: load error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to load projects")
			return
		}

		sessionInfos := buildSessionInfos(sessions)

		writeJSON(w, map[string]any{
			"ok": true,
			"data": map[string]any{
				"configured":             state.Items,
				"discovered":             state.Discovered(sessionInfos),
				"unmatched_active_count": state.UnmatchedActiveCount(sessionInfos),
			},
		})
	})

	mux.HandleFunc("PUT /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}

		var incoming projects.State
		if err := json.Unmarshal(body, &incoming); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}
		if err := incoming.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		err = projectMgr.Update(func(state *projects.State) bool {
			*state = incoming
			return true
		})
		if err != nil {
			log.Printf("projects: save error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /v1/projects/add", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}

		var req struct {
			Remote string   `json:"remote"`
			Paths  []string `json:"paths"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}

		if len(req.Paths) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "paths required")
			return
		}

		// Normalize inputs.
		for i, p := range req.Paths {
			req.Paths[i] = projects.NormalizePath(p)
		}
		if req.Remote != "" {
			req.Remote = projects.NormalizeRemote(req.Remote)
		}

		// Derive slug: prefer remote repo name, fall back to first path basename.
		var slug string
		if req.Remote != "" {
			slug = projects.SlugFromRemote(req.Remote)
		} else {
			slug = projects.SlugFromPath(req.Paths[0])
		}

		var item projects.Item
		err = projectMgr.Update(func(state *projects.State) bool {
			slug = projects.UniqueSlug(slug, state.Items)
			item = projects.Item{
				Slug:   slug,
				Remote: req.Remote,
				Paths:  req.Paths,
			}
			state.Items = append(state.Items, item)
			if err := state.Validate(); err != nil {
				log.Printf("projects: add validation error: %v", err)
				return false
			}
			return true
		})
		if err != nil {
			log.Printf("projects: add error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
			return
		}
		// Populate the new project's sessions array with alive matches
		// immediately, so the frontend sees them on the first fetch.
		projectMgr.AutoAssignAllAlive(buildSessionInfos(sessions))
		writeJSON(w, map[string]any{"ok": true, "data": item})
	})

	// ── Sessions ──

	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "data": sessions.List()})
	})

	// ── Registration (fast path for gmux-run) ──

	mux.HandleFunc("POST /v1/register", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}

		var req struct {
			SessionID  string `json:"session_id"`
			SocketPath string `json:"socket_path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}

		if req.SessionID == "" || req.SocketPath == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "session_id and socket_path required")
			return
		}

		log.Printf("register: %s at %s", req.SessionID, req.SocketPath)
		if err := discovery.Register(sessions, subs, fileMon, req.SocketPath, pendingResumes); err != nil {
			log.Printf("register: failed to query meta for %s: %v", req.SessionID, err)
			writeError(w, http.StatusBadGateway, "runner_unreachable", err.Error())
			return
		}

		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /v1/deregister", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}

		var req struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}

		// Don't remove from store — the exit event from the subscription
		// already marked it alive: false. Just clean up the subscription.
		subs.Unsubscribe(req.SessionID)
		log.Printf("deregister: %s", req.SessionID)
		writeJSON(w, map[string]any{"ok": true})
	})

	// ── Launch ──

	mux.HandleFunc("POST /v1/launch", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}

		var req struct {
			Cwd        string   `json:"cwd"`
			Command    []string `json:"command"`
			LauncherID string   `json:"launcher_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}

		// Resolve command from launcher_id if no explicit command.
		if len(req.Command) == 0 && req.LauncherID != "" {
			cfg := launchConfig
			found := false
			for _, l := range cfg.Launchers {
				if l.ID == req.LauncherID {
					req.Command = l.Command
					found = true
					break
				}
			}
			if !found {
				writeError(w, http.StatusBadRequest, "launcher_unavailable", fmt.Sprintf("launcher %q is not available on this system", req.LauncherID))
				return
			}
		}

		// Empty/nil command means "shell" — use user's $SHELL
		if len(req.Command) == 0 {
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
			req.Command = []string{shell}
		}

		cwd := req.Cwd
		if cwd == "" {
			cwd = os.Getenv("HOME")
		}

		if gmuxBin == "" {
			writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found (install gmux alongside gmuxd)")
			return
		}

		pid, err := launchGmux(gmuxBin, req.Command, cwd)
		if err != nil {
			log.Printf("launch: failed to start gmux: %v", err)
			writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
			return
		}

		log.Printf("launch: started gmux pid=%d cwd=%s cmd=%v", pid, cwd, req.Command)
		writeJSON(w, map[string]any{
			"ok":   true,
			"data": map[string]any{"pid": pid},
		})
	})

	// ── Session Actions ──

	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		sessionID := parts[2]
		action := ""
		if len(parts) == 4 {
			action = parts[3]
		}

		switch action {
		case "attach":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			writeJSON(w, map[string]any{
				"ok": true,
				"data": map[string]any{
					"transport":   "websocket",
					"ws_path":     "/ws/" + sessionID,
					"socket_path": sess.SocketPath,
				},
			})

		case "resume":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			// Serialize resume attempts to prevent double-click races.
			resumeMu.Lock()
			defer resumeMu.Unlock()

			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			if sess.Alive || len(sess.Command) == 0 {
				writeError(w, http.StatusBadRequest, "not_resumable", "session is not resumable")
				return
			}
			if gmuxBin == "" {
				writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found")
				return
			}

			// Record pending resume BEFORE launching so Register() can match.
			pendingResumes.Add(sess.Command, sessionID)

			pid, err := launchGmux(gmuxBin, sess.Command, sess.Cwd)
			if err != nil {
				pendingResumes.Take(sess.Command) // clean up on failure
				log.Printf("resume: failed to start gmux: %v", err)
				writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
				return
			}

			// Don't modify the session here. It stays dead/resumable until
			// the runner calls POST /register and Register() merges it.
			// The frontend shows a local "resuming" indicator.
			log.Printf("resume: started gmux pid=%d for %s cwd=%s", pid, sessionID, sess.Cwd)
			writeJSON(w, map[string]any{
				"ok":   true,
				"data": map[string]any{"pid": pid, "session_id": sessionID},
			})

		case "kill":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			// Send kill to runner — it will SIGTERM the child, which triggers
			// normal exit lifecycle (exit event → subscription updates store).
			// If the runner is unreachable, force-mark dead (stale session).
			if sess.SocketPath != "" && sess.Alive {
				if err := discovery.KillSession(sess.SocketPath); err != nil {
					log.Printf("kill: %s: runner unreachable, forcing dead: %v", sessionID, err)
					sess.Alive = false
					sess.Status = nil
					if fileMon != nil {
						if cmd := fileMon.ResolveResumeCommand(&sess); cmd != nil {
							sess.Command = cmd
						}
					}
					sessions.Upsert(sess)
					subs.Unsubscribe(sessionID)
					if fileMon != nil {
						fileMon.NotifySessionDied(sessionID)
					}
					os.Remove(sess.SocketPath)
				}
			}
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})

		case "read":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			sessions.Update(sessionID, func(sess *store.Session) {
				sess.Unread = false
				if sess.Status != nil && sess.Status.Error {
					sess.Status.Error = false
				}
			})
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})

		case "dismiss":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			// Kill if still alive.
			if sess.SocketPath != "" && sess.Alive {
				if err := discovery.KillSession(sess.SocketPath); err != nil {
					log.Printf("dismiss: %s: runner kill failed: %v", sessionID, err)
				}
			}
			// Clean up shell state file before removing from store.
			if sess.Kind == "shell" {
				adapters.RemoveShellStateFile(sessionID, sess.Cwd)
			}
			// Remove session from its project's sessions array.
			projectMgr.DismissSession(sessionID, sess.ResumeKey)
			// Remove from store — broadcasts session-remove to all clients.
			sessions.Remove(sessionID)
			if subs != nil {
				subs.Unsubscribe(sessionID)
			}
			if fileMon != nil {
				fileMon.NotifySessionDied(sessionID)
			}
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})

		default:
			http.NotFound(w, r)
		}
	})

	// ── WebSocket proxy ──

	wsProxy := wsproxy.New(func(sessionID string) (string, error) {
		sess, ok := sessions.Get(sessionID)
		if !ok {
			return "", fmt.Errorf("session %s not found", sessionID)
		}
		if sess.SocketPath == "" {
			return "", fmt.Errorf("session %s has no socket", sessionID)
		}
		return sess.SocketPath, nil
	}, sessions)
	mux.HandleFunc("/ws/{sessionID}", wsProxy.Handler())

	// ── Presence WebSocket ──

	mux.HandleFunc("/v1/presence", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("presence: accept: %v", err)
			return
		}

		clientID := fmt.Sprintf("client-%d", time.Now().UnixNano())
		client := &presence.Client{
			ID:          clientID,
			Conn:        conn,
			ConnectedAt: time.Now(),
		}

		// Read client-hello first.
		ctx := r.Context()
		_, data, err := conn.Read(ctx)
		if err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		var hello struct {
			Type                   string `json:"type"`
			DeviceType             string `json:"device_type"`
			NotificationPermission string `json:"notification_permission"`
		}
		if err := json.Unmarshal(data, &hello); err == nil && hello.Type == "client-hello" {
			client.DeviceType = hello.DeviceType
			client.NotificationPermission = hello.NotificationPermission
		}

		presenceTable.Add(client)
		log.Printf("presence: client %s connected (%s, notif=%s)", clientID, client.DeviceType, client.NotificationPermission)

		defer func() {
			presenceTable.Remove(clientID)
			conn.Close(websocket.StatusNormalClosure, "")
			log.Printf("presence: client %s disconnected", clientID)
		}()

		// Read state updates until disconnect.
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Type              string  `json:"type"`
				Visibility        string  `json:"visibility"`
				Focused           bool    `json:"focused"`
				SelectedSessionID string  `json:"selected_session_id"`
				LastInteraction   float64 `json:"last_interaction"`
				Permission        string  `json:"permission"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "client-state":
				presenceTable.Update(clientID, presence.ClientState{
					Visibility:        msg.Visibility,
					Focused:           msg.Focused,
					SelectedSessionID: msg.SelectedSessionID,
					LastInteraction:   msg.LastInteraction,
				})
			case "notif-permission":
				presenceTable.SetPermission(clientID, msg.Permission)
			}
		}
	})

	// ── SSE Events ──

	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// Send current state as upserts
		for _, sess := range sessions.List() {
			s := sess
			sendSSE(w, "session-upsert", store.Event{
				Type:    "session-upsert",
				ID:      s.ID,
				Session: &s,
			})
		}
		flusher.Flush()

		// Stream updates
		ch, cancel := sessions.Subscribe()
		defer cancel()

		notify := r.Context().Done()
		for {
			select {
			case <-notify:
				return
			case ev, open := <-ch:
				if !open {
					return
				}
				sendSSE(w, ev.Type, ev)
				flusher.Flush()
			}
		}
	})

	// ── Embedded frontend (SPA fallback) ──

	mux.Handle("/", spaHandler())

	// ── Load config ──

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}

	// ── Resolve TCP listen address and auth token ──

	resolved, err := cfg.ListenAddr()
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	tcpAddr = resolved

	tok, err := authtoken.LoadOrCreate(stateDir)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	authToken = tok

	// ── Replace any existing daemon via Unix socket ──

	sock := paths.SocketPath()
	if err := unixipc.Replace(sock); err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}

	// ── Shutdown endpoint (Unix socket only) ──
	// The netauth middleware blocks this on TCP.
	// Tailscale also blocks it (peer identity, not localhost).

	var sockSrv *http.Server
	var tcpSrv *http.Server

	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		log.Printf("shutdown requested — exiting")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if tcpSrv != nil {
				tcpSrv.Shutdown(ctx)
			}
			if sockSrv != nil {
				sockSrv.Shutdown(ctx)
			}
			unixipc.Cleanup(sock)
		}()
	})

	// ── Unix socket listener (local IPC, no auth) ──

	sockLn, err := unixipc.Listen(sock)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	sockSrv = &http.Server{Handler: mux}
	go func() {
		if err := sockSrv.Serve(sockLn); err != http.ErrServerClosed {
			log.Printf("unix socket listener: %v", err)
		}
	}()
	log.Printf("unix socket: %s", sock)

	// ── TCP listener (always, token-authenticated) ──

	authedHandler := netauth.Middleware(authToken, mux)
	tcpSrv = &http.Server{Addr: tcpAddr, Handler: authedHandler}

	tcpLn, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		log.Fatalf("FATAL: tcp listener on %s: %v", tcpAddr, err)
	}

	log.Printf("tcp listener on %s (token-authenticated)", tcpAddr)
	go func() {
		if err := tcpSrv.Serve(tcpLn); err != http.ErrServerClosed {
			log.Printf("tcp listener: %v", err)
		}
	}()

	// ── Optional tailscale listener ──

	if cfg.Tailscale.Enabled {
		tsListener = tsauth.Start(tsauth.Config{
			Hostname: cfg.Tailscale.Hostname,
			Allow:    cfg.Tailscale.Allow,
		}, stateDir, mux)
		defer tsListener.Shutdown()
	}

	// ── Signal handling for graceful shutdown ──

	log.Printf("gmuxd %s ready", version)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received %v — shutting down", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tcpSrv.Shutdown(ctx)
	sockSrv.Shutdown(ctx)
	unixipc.Cleanup(sock)

	log.Printf("gmuxd stopped")
	return 0
}

// runStatus queries the running daemon via Unix socket and prints health info.
func runStatus(stdout, stderr io.Writer) int {
	sock := paths.SocketPath()
	client := unixipc.Client(sock)

	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: not running (socket: %s)\n", sock)
		return 1
	}
	defer resp.Body.Close()

	var health struct {
		OK   bool `json:"ok"`
		Data struct {
			Version         string `json:"version"`
			Status          string `json:"status"`
			Listen          string `json:"listen"`
			TailscaleURL    string `json:"tailscale_url,omitempty"`
			UpdateAvailable string `json:"update_available,omitempty"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		_, _ = fmt.Fprintf(stderr, "gmuxd: unexpected health response\n")
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "gmuxd %s (%s)\n", health.Data.Version, health.Data.Status)
	_, _ = fmt.Fprintf(stdout, "  tcp:    %s\n", health.Data.Listen)
	_, _ = fmt.Fprintf(stdout, "  socket: %s\n", sock)
	if health.Data.TailscaleURL != "" {
		_, _ = fmt.Fprintf(stdout, "  remote: %s\n", health.Data.TailscaleURL)
	}
	if health.Data.UpdateAvailable != "" {
		_, _ = fmt.Fprintf(stdout, "  update: %s available\n", health.Data.UpdateAvailable)
	}
	return 0
}

// runAuth queries the running daemon for the TCP address and auth token.
func runAuth(stdout, stderr io.Writer) int {
	sock := paths.SocketPath()
	client := unixipc.Client(sock)

	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: not running (socket: %s)\n", sock)
		return 1
	}
	defer resp.Body.Close()

	var health struct {
		OK   bool `json:"ok"`
		Data struct {
			Listen    string `json:"listen"`
			AuthToken string `json:"auth_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		_, _ = fmt.Fprintf(stderr, "gmuxd: unexpected health response\n")
		return 1
	}

	if health.Data.AuthToken == "" {
		_, _ = fmt.Fprintf(stderr, "gmuxd: could not retrieve auth token\n")
		return 1
	}

	url := fmt.Sprintf("http://%s/auth/login?token=%s", health.Data.Listen, health.Data.AuthToken)

	_, _ = fmt.Fprintf(stdout, "Listen:     %s\n", health.Data.Listen)
	_, _ = fmt.Fprintf(stdout, "Auth token: %s\n", health.Data.AuthToken)
	_, _ = fmt.Fprintf(stdout, "\nOpen this URL to authenticate:\n  %s\n", url)

	return 0
}

// buildSessionInfos converts store sessions to project SessionInfo structs.
func buildSessionInfos(sessions *store.Store) []projects.SessionInfo {
	list := sessions.List()
	infos := make([]projects.SessionInfo, len(list))
	for i, s := range list {
		infos[i] = projects.SessionInfo{
			ID:            s.ID,
			Cwd:           s.Cwd,
			WorkspaceRoot: s.WorkspaceRoot,
			Remotes:       s.Remotes,
			Alive:         s.Alive,
			ResumeKey:     s.ResumeKey,
		}
	}
	return infos
}

func sendSSE(w http.ResponseWriter, event string, payload any) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, bytes)
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"error": map[string]any{"code": code, "message": message},
	})
}


