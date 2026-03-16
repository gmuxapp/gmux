package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/binhash"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionfiles"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/wsproxy"
)

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

// resolveGmuxr finds the gmuxr binary.
// Priority: sibling to this binary > PATH lookup.
// Both gmuxd and gmuxr are always installed to the same directory.
func resolveGmuxr() string {
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "gmuxr")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("gmuxr"); err == nil {
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

// launchGmuxr starts a detached gmuxr process with the given command and cwd.
// Returns the PID on success.
func launchGmuxr(gmuxrBin string, command []string, cwd string) (int, error) {
	args := []string{"--cwd", cwd, "--"}
	args = append(args, command...)

	cmd := exec.Command(gmuxrBin, args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

func main() {
	gmuxrBin := resolveGmuxr() // resolve once, use everywhere
	if gmuxrBin != "" {
		log.Printf("gmuxr: %s", gmuxrBin)
		h := binhash.File(gmuxrBin)
		if h != "" {
			discovery.ExpectedRunnerHash = h
			log.Printf("gmuxr hash: %s…", h[:12])
		}
	}
	launchConfig := discoverLaunchers()

	sessions := store.New()
	subs := discovery.NewSubscriptions(sessions)
	pendingResumes := discovery.NewPendingResumes()

	// Start file monitor — watches adapter session directories with inotify
	// to extract title and working status from JSONL files.
	fileMon := discovery.NewFileMonitor(sessions)
	stopFileMon := make(chan struct{})
	go fileMon.Run(stopFileMon)
	defer close(stopFileMon)

	// Start socket-based discovery (scans /tmp/gmux-sessions/*.sock)
	// Discovery also subscribes to each runner's /events SSE for live updates.
	stopDiscovery := make(chan struct{})
	go discovery.Watch(sessions, subs, fileMon, 3*time.Second, stopDiscovery)
	defer close(stopDiscovery)

	// Start session file scanner — discovers resumable sessions from
	// adapter session files (e.g. pi's JSONL conversations). Also purges
	// stale dead sessions that were never attributed to a file.
	scanner := sessionfiles.New(sessions)
	stopScanner := make(chan struct{})
	go scanner.Run(30*time.Second, stopScanner)
	defer close(stopScanner)

	mux := http.NewServeMux()

	// ── Health + Capabilities ──

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok": true,
			"data": map[string]any{
				"service": "gmuxd",
				"node_id": "node-local",
				"status":  "ready",
			},
		})
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

		if gmuxrBin == "" {
			writeError(w, http.StatusInternalServerError, "gmuxr_not_found", "gmuxr not found (install gmuxr alongside gmuxd)")
			return
		}

		pid, err := launchGmuxr(gmuxrBin, req.Command, cwd)
		if err != nil {
			log.Printf("launch: failed to start gmuxr: %v", err)
			writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
			return
		}

		log.Printf("launch: started gmuxr pid=%d cwd=%s cmd=%v", pid, cwd, req.Command)
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
			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			if !sess.Resumable || len(sess.Command) == 0 {
				writeError(w, http.StatusBadRequest, "not_resumable", "session is not resumable")
				return
			}
			if gmuxrBin == "" {
				writeError(w, http.StatusInternalServerError, "gmuxr_not_found", "gmuxr not found")
				return
			}

			// Record pending resume BEFORE launching so Register() can match.
			pendingResumes.Add(sess.Command, sessionID)

			pid, err := launchGmuxr(gmuxrBin, sess.Command, sess.Cwd)
			if err != nil {
				log.Printf("resume: failed to start gmuxr: %v", err)
				writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
				return
			}

			// Update in-place: session is now starting.
			// Register() will merge in the live session data (socket, pid)
			// when gmuxr calls POST /v1/register.
			sess.Alive = true
			sess.Resumable = false
			sess.Status = &store.Status{Label: "starting", Working: true}
			sessions.Upsert(sess)

			log.Printf("resume: started gmuxr pid=%d for %s cwd=%s", pid, sessionID, sess.Cwd)
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
			// normal exit lifecycle (exit event → subscription updates store)
			if sess.SocketPath != "" && sess.Alive {
				if err := discovery.KillSession(sess.SocketPath); err != nil {
					log.Printf("kill: %s: runner kill failed: %v", sessionID, err)
				}
			}
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

	// Env var overrides config file port.
	port := envOr("GMUXD_PORT", fmt.Sprintf("%d", cfg.Port))
	addr := "127.0.0.1:" + port

	// ── Shutdown endpoint (used by new instances to take over the port) ──

	srv := &http.Server{Addr: addr, Handler: mux}

	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		log.Printf("shutdown requested — exiting")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			srv.Shutdown(ctx)
		}()
	})

	// ── Take over from any existing gmuxd on this port ──

	requestShutdown(addr)

	// ── Optional tailscale listener ──

	if cfg.Tailscale.Enabled {
		tsListener, err := tsauth.Start(tsauth.Config{
			Hostname: cfg.Tailscale.Hostname,
			Allow:    cfg.Tailscale.Allow,
		}, stateDir(), mux)
		if err != nil {
			log.Printf("tailscale: %v (continuing without tailscale)", err)
		} else {
			defer tsListener.Shutdown()
		}
	}

	// ── Localhost listener (always, no auth) ──

	log.Printf("gmuxd listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// requestShutdown asks an existing gmuxd on the same address to shut down,
// then waits for the port to become available. This replaces PID files —
// the port itself is the lock.
func requestShutdown(addr string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post("http://"+addr+"/v1/shutdown", "", nil)
	if err != nil {
		return // Nothing listening — port is free.
	}
	resp.Body.Close()
	log.Printf("asked existing gmuxd at %s to shut down", addr)

	// Wait for the port to become available.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			log.Printf("warning: timed out waiting for %s to free up", addr)
			return
		case <-tick.C:
			resp, err := client.Get("http://" + addr + "/v1/health")
			if err != nil {
				return // Port is free.
			}
			resp.Body.Close()
		}
	}
}

// stateDir returns the gmux state directory (~/.local/state/gmux).
func stateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "gmux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "gmux")
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

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
