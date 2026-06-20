package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/gmuxapp/gmux/packages/sessionenv"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/authtoken"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/binhash"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/clipfile"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/coalesce"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/devcontainers"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/identity"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/netauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/nodeid"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/notify"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peerstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionfiles"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionmeta"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sleep"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/update"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/wsproxy"
	qrterminal "github.com/mdp/qrterminal/v3"
	"nhooyr.io/websocket"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

// maxInputBytes mirrors the cap the runner's POST /input handler
// enforces. We re-check it here so the error surfaces at gmuxd's
// edge (with a 413) instead of silently truncating inside the
// runner.
const maxInputBytes = 1 << 20 // 1 MiB

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

// launchGmux forks a gmux runner with the given command and cwd.
// Returns the PID on success.
//
// resumeID, when non-empty, is passed via --resume-id so the
// runner uses the daemon-supplied id instead of generating a fresh
// one. /v1/launch leaves it empty (fresh sessions get a runner-
// generated id); /v1/resume and /v1/restart pass the existing
// session's id so identity (and the scrollback directory on disk)
// carry across the seam. See ADR 0003.
//
// initialCols / initialRows, when non-zero, are passed via
// --initial-cols / --initial-rows so the PTY starts at the right
// size instead of the 80x24 default. Without this, /resume and
// /restart momentarily expose the child process to a
// default-sized terminal between exec and the browser's first
// resize WS message; programs that read $COLUMNS or query the
// TTY once at startup (claude, vim, less, prompt frameworks)
// stay stuck at 80 columns.
//
// Directives are delivered as CLI flags so the daemon↔runner
// contract is greppable and shows up in `ps`. The runner still
// honours the legacy GMUX_RESUME_ID env var as a fallback for
// rolling upgrades, but this code path no longer sets it.
func launchGmux(gmuxBin string, command []string, cwd, resumeID string, initialCols, initialRows uint16) (int, error) {
	cmd := exec.Command(gmuxBin, buildLaunchArgs(resumeID, initialCols, initialRows, command)...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	// Source a fresh interactive-login environment for the session
	// (see ADR 0006), so dotfile changes and the Restart button take
	// effect without a daemon restart. Falls back to the daemon's own
	// env when no login shell is available (headless daemons) or the
	// probe fails. Strip gmux session-identity vars either way so child
	// processes don't inherit a parent session's identity (a leaked
	// GMUX_SESSION_ID/GMUX_SOCKET/GMUX_ADAPTER would otherwise be
	// stamped onto every launched session). See packages/sessionenv.
	cmd.Env = sessionenv.Strip(captureLoginEnv(gmuxBin, cwd))

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

// buildLaunchArgs assembles the gmux runner argv for the internal
// `gmux __run [directives] -- <command>` form (ADR 0009): the hidden
// __run verb, any non-empty daemon→runner directive flags, then a `--`
// terminator and the user command verbatim. The `--` delivers the
// command intact even when its own arguments look like flags.
func buildLaunchArgs(resumeID string, initialCols, initialRows uint16, command []string) []string {
	args := make([]string, 0, len(command)+5)
	args = append(args, "__run")
	if resumeID != "" {
		args = append(args, "--resume-id="+resumeID)
	}
	if initialCols > 0 {
		args = append(args, fmt.Sprintf("--initial-cols=%d", initialCols))
	}
	if initialRows > 0 {
		args = append(args, fmt.Sprintf("--initial-rows=%d", initialRows))
	}
	args = append(args, "--")
	return append(args, command...)
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `gmuxd %s

Usage: gmuxd <command>

Commands:
  start              Start the daemon in the background
  run                Run the daemon in the foreground (for systemd/Docker)
  stop               Stop the running daemon
  restart            Restart the daemon (stops then starts)
  status             Show daemon health, listeners, and sessions
  auth               Show the auth URL and token
  remote             Set up or check remote access via Tailscale
  log-path           Print the daemon log file path
  version            Show gmuxd version
  help               Show this help

Tip:
  gmux daemon <cmd>  Canonical front for these commands (start/stop/status/...)
  gmux -- <command>  Run a command; gmux auto-starts gmuxd if needed
  More help: https://gmux.app
`, version)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	cmd := args[0]
	args = args[1:]

	switch cmd {
	case "start", "restart":
		for _, arg := range args {
			switch arg {
			case "-h", "--help":
				_, _ = fmt.Fprintf(stdout, "Usage: gmuxd %s\n\nStarts the daemon in the background, replacing any existing instance.\n", cmd)
				return 0
			default:
				_, _ = fmt.Fprintf(stderr, "gmuxd %s: unknown option %q\n", cmd, arg)
				return 2
			}
		}
		return startBackground(stdout, stderr)
	case "run":
		for _, arg := range args {
			switch arg {
			case "-h", "--help":
				_, _ = fmt.Fprintf(stdout, "Usage: gmuxd run\n\nRuns the daemon in the foreground (for systemd, Docker, or debugging).\n")
				return 0
			default:
				_, _ = fmt.Fprintf(stderr, "gmuxd run: unknown option %q\n", arg)
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
		return runRemote(os.Stdin, stdout, stderr)
	case "version":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd version: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", version)
		return 0
	case "log-path":
		if len(args) > 0 {
			_, _ = fmt.Fprintf(stderr, "gmuxd log-path: unexpected arguments: %s\n", strings.Join(args, " "))
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", filepath.Join(paths.StateDir(), "gmuxd.log"))
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

// startBackground re-execs gmuxd with "run" in a detached process,
// replacing any existing daemon. Output goes to a log file in the
// state directory. Waits briefly to confirm startup succeeded.
func startBackground(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: cannot determine own path: %v\n", err)
		return 1
	}

	// Check for an existing daemon and stop it first.
	sock := paths.SocketPath()
	replaced := false
	if oldVer, ok := unixipc.HealthVersion(sock); ok {
		replaced = true
		if oldVer != "" {
			_, _ = fmt.Fprintf(stdout, "gmuxd: stopping existing daemon (%s)...\n", oldVer)
		} else {
			_, _ = fmt.Fprintf(stdout, "gmuxd: stopping existing daemon...\n")
		}
		if !unixipc.Shutdown(sock) {
			_, _ = fmt.Fprintf(stderr, "gmuxd: existing daemon did not shut down\n")
			return 1
		}
	}

	stateDir := paths.StateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: cannot create state dir %s: %v\n", stateDir, err)
		return 1
	}

	logPath := filepath.Join(stateDir, "gmuxd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: cannot open log %s: %v\n", logPath, err)
		return 1
	}

	cmd := exec.Command(exe, "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Strip gmux session-identity vars so the daemon doesn't inherit
	// session identity. See packages/sessionenv.
	cmd.Env = sessionenv.Strip(os.Environ())

	if err := cmd.Start(); err != nil {
		logFile.Close()
		_, _ = fmt.Fprintf(stderr, "gmuxd: failed to start: %v\n", err)
		return 1
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		logFile.Close()
		close(done)
	}()

	// Wait for the daemon to become healthy. The new process does heavy
	// initialization (launcher discovery, session sweep, conversation
	// index scan, project loading) before it creates the Unix socket, so
	// the probe can take several seconds on a slow machine or a large
	// history. Poll with exponential backoff until a generous deadline
	// rather than a tight fixed budget. Break immediately if the process
	// exits before becoming healthy (crash, bad config, port conflict).
	healthy := false
	backoff := 200 * time.Millisecond
	deadline := time.Now().Add(20 * time.Second)
	for !healthy && time.Now().Before(deadline) {
		select {
		case <-done:
			// Daemon process exited before becoming healthy.
			_, _ = fmt.Fprintf(stderr, "gmuxd: daemon exited before becoming healthy\n  Logs: %s\n", logPath)
			return 1
		case <-time.After(backoff):
		}
		healthy = unixipc.Healthy(sock)
		backoff = min(backoff*2, 2*time.Second)
	}

	if !healthy {
		_, _ = fmt.Fprintf(stderr, "gmuxd: started (pid %d) but not yet healthy\n  Logs: %s\n", cmd.Process.Pid, logPath)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "gmuxd: running %s (pid %d)\n  Logs: %s\n", version, cmd.Process.Pid, logPath)
	if replaced {
		_, _ = fmt.Fprintf(stdout, "  Note: active sessions will use the new version when restarted.\n")
	}
	return 0
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

	// sessionmeta persists per-session records so dead sessions
	// survive a gmuxd restart. Sweep on startup repopulates the
	// store with everything we knew about previously; the OnDead
	// hook below persists every Alive=false landing; Dismiss /
	// Resume merge / slug takeover drop the corresponding directory.
	// See sessionmeta package doc for the full lifecycle.
	metaStore := sessionmeta.New(sessionmeta.DefaultDir())
	// persistedKeys records the id and slug of every session sessionmeta
	// holds on disk at startup (the post-retention sweep survivors). The
	// startup CleanupSessions unions this with the live store so a project
	// membership key is pruned only when it is neither live nor persisted.
	// Capturing it here rather than re-deriving it from the store at
	// cleanup time keeps the guard correct regardless of whether the swept
	// dead sessions are still populated in the store by then.
	persistedKeys := make(map[string]bool)
	if loaded, err := metaStore.Sweep(); err != nil {
		log.Printf("sessionmeta: sweep failed: %v", err)
	} else {
		for _, sess := range loaded {
			sessions.Upsert(sess)
			persistedKeys[sess.ID] = true
			if sess.Slug != "" {
				persistedKeys[sess.Slug] = true
			}
		}
		if n := len(loaded); n > 0 {
			log.Printf("sessionmeta: restored %d session(s) from %s", n, metaStore.Dir())
		}
	}
	persistDead := func(sess store.Session) {
		if err := metaStore.Write(sess); err != nil {
			log.Printf("sessionmeta: write %s: %v", sess.ID, err)
		}
	}
	forgetMeta := func(id string) {
		if err := metaStore.Remove(id); err != nil {
			log.Printf("sessionmeta: remove %s: %v", id, err)
		}
	}

	// Drive the persister's removal loop off store events so every
	// session-remove (dismiss, slug takeover, peer disconnect, etc.)
	// drops the matching meta dir. The explicit forgetMeta call in
	// the dismiss handler is redundant but cheap. Resume is an
	// alive=false→true Upsert under the same id (ADR 0003) and
	// leaves meta.json in place; it gets overwritten by persistDead
	// the next time the session dies, or harmlessly rediscovered as
	// alive=true on the next daemon restart. No explicit cleanup
	// needed.
	metaEvents, cancelMetaEvents := sessions.Subscribe()
	defer cancelMetaEvents()
	go metaStore.WatchRemovals(metaEvents)

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
	subs.OnDead = persistDead
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
	// The agent extension reports the JSONL file its agent holds; this is
	// authoritative attribution and also suppresses daemon-side parsing for
	// the session (its runner owns derived state).
	subs.OnSessionFile = func(sessionID, filePath string) {
		fileMon.AttributeFromHook(sessionID, filePath)
	}
	stopFileMon := make(chan struct{})
	go fileMon.Run(stopFileMon)
	defer close(stopFileMon)

	// Start socket-based discovery (scans paths.SessionSocketDir() for *.sock)
	// Discovery also subscribes to each runner's /events SSE for live updates.
	stopDiscovery := make(chan struct{})
	go discovery.Watch(sessions, subs, fileMon, persistDead, nil, 3*time.Second, stopDiscovery)
	defer close(stopDiscovery)

	// Session file scanner — discovers resumable sessions from adapter
	// session files (e.g. pi's JSONL conversations). Also purges stale
	// dead sessions that were never attributed to a file. Started below
	// after the project manager is set up so the first-scan callback
	// can clean up orphaned project session refs.
	scanner := sessionfiles.New(sessions)
	stopScanner := make(chan struct{})
	defer close(stopScanner)

	// Conversations index — maps (kind, slug) to file metadata for URL
	// resolution of dead conversations and future fulltext search.
	// One bootstrap scan at startup; from then on the index is kept
	// fresh by filemon's fsnotify event handler (SetConvIndex below).
	convIndex := conversations.New()
	convIndex.Scan()
	log.Printf("conversations: indexed %d files", convIndex.Count())

	// Wire filemon to the conversations index and install always-on
	// watches on every adapter session root. After this, every .jsonl
	// Create/Write/Remove under any adapter root updates the index
	// automatically, with no periodic scan involved.
	fileMon.SetConvIndex(convIndex)
	fileMon.WatchRoots()

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

	// Stable, opaque per-node identity (ADR 0007). Generated once and
	// persisted alongside the auth token; used for peer dedup, never
	// shown or routed.
	nodeID, err := nodeid.LoadOrCreate(stateDir)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}

	// Manually-added peers are runtime state (ADR 0007 §5), not config.
	// Opened early so the /v1/peers add/remove handlers can capture it.
	peerStore, err := peerstore.Open(stateDir)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}

	// peerManager is initialized later after config is loaded. Closures
	// (reconcileProjectStamps, buildSessionInfos call sites) capture this
	// pointer so handlers work once it's set.
	var peerManager *peering.Manager

	// Project manager handles concurrent access to projects.json and
	// auto-assignment of sessions to projects.
	projectMgr := projects.NewManager(stateDir)

	// One-time upgrade: ADR 0008 removed tailscale autodiscovery, so the
	// hosts it surfaced automatically would otherwise vanish (orphaning
	// their project references). Migrate the legacy discovery cache into
	// the peer store as token-less manual peers — but only the hosts a
	// project actually references — then delete the cache. The user
	// supplies each token via "Add token". Best-effort: never blocks
	// startup.
	if state, err := projectMgr.Load(); err != nil {
		log.Printf("peerstore: legacy discovery migration skipped (projects load: %v)", err)
	} else {
		referenced := make(map[string]bool)
		for _, it := range state.Items {
			if it.Peer != "" {
				referenced[it.Peer] = true
			}
		}
		if n, err := peerStore.ImportLegacyDiscovery(stateDir, referenced); err != nil {
			log.Printf("peerstore: legacy discovery migration: %v", err)
		} else if n > 0 {
			log.Printf("peerstore: migrated %d referenced host(s) from the legacy autodiscovery cache (supply a token to reconnect)", n)
		}
	}

	// reconcileProjectStamps is the single point that updates each
	// owned session's ProjectSlug / ProjectIndex from the current
	// projects.json state. Called after every projectMgr.Update (via
	// the Broadcast hook below) and once explicitly after the startup
	// session-seeding loop. See ADR 0002.
	reconcileProjectStamps := func(state *projects.State) {
		assignments := state.AssignmentsByKey()
		sessions.Reconcile(func(s store.Session) (string, int) {
			// Network peers own their own project assignments;
			// preserve whatever stamp arrived on the wire.
			if s.Peer != "" && (peerManager == nil || !peerManager.IsLocalPeer(s.Peer)) {
				return s.ProjectSlug, s.ProjectIndex
			}
			// Local sessions and Local-peer (devcontainer) sessions:
			// parent owns assignment, stamp from its projects.json.
			key := projects.SessionKey(s.ID, s.Slug)
			a := assignments[key]
			return a.Slug, a.Index
		})
	}

	projectMgr.Broadcast = func(state *projects.State) {
		reconcileProjectStamps(state)
		sessions.Broadcast(store.Event{Type: "projects-update"})
	}
	projectMgr.SeedIfEmpty()

	// Populate the store with project-tracked sessions that don't have
	// a sessionmeta record. The sessionmeta sweep above is the SOT for
	// runtime fields; this only fills in the pre-S2 fallback path. See
	// rehydrateProjects for the identity-model rationale.
	if state, err := projectMgr.Load(); err == nil {
		rehydrateProjects(sessions, convIndex, state)
		// Sweep live sessions into project.sessions[] arrays. Dead/resumable
		// sessions loaded from sessionmeta are stamped only when projects.json
		// already contains their key; dismiss removes that membership, and a
		// late exit/resume-command upsert must not recreate it.
		projectMgr.AutoAssignAll(buildSessionInfos(sessions, func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) }))
		// Stamp ProjectSlug / ProjectIndex on the just-rehydrated sessions
		// (and on any sessions previously loaded via sessionmeta.Sweep)
		// before SSE subscribers can observe. Reload to pick up any
		// AutoAssignAll mutations; Broadcast also runs reconcile but only
		// fires when state actually changed.
		if fresh, err := projectMgr.Load(); err == nil {
			state = fresh
		}
		reconcileProjectStamps(state)
	}

	// After the store is populated, clean up orphaned project entries
	// (keys that resolve to neither a live session nor a persisted one).
	// known = persisted-on-disk ∪ live store; see persistedKeys above.
	scanner.OnFirstScan = func() {
		known := make(map[string]bool, len(persistedKeys))
		for k := range persistedKeys {
			known[k] = true
		}
		for _, s := range sessions.List() {
			known[s.ID] = true
			if s.Slug != "" {
				known[s.Slug] = true
			}
		}
		projectMgr.CleanupSessions(known)
	}
	go scanner.Run(30*time.Second, stopScanner)

	// Conversations index updates are watcher-driven via filemon
	// (see SetConvIndex + WatchRoots above). No periodic rescan: a
	// healthy fsnotify watch tree plus the startup bootstrap scan
	// covers steady state. If reports of staleness emerge after
	// suspend or inotify queue overflow, add an explicit reconcile
	// hook — don't reintroduce the periodic ticker.

	// Auto-assign sessions to projects when they appear or get a Slug.
	sessionEvents, unsubSessionEvents := sessions.Subscribe()
	defer unsubSessionEvents()
	go func() {
		for ev := range sessionEvents {
			if ev.Type != "session-upsert" || ev.Session == nil {
				continue
			}
			s := ev.Session
			// Auto-assign live sessions only. Dead/resumable runtime state
			// belongs to sessionmeta; sidebar membership belongs to projects.json.
			// If dismiss removed the key, a late exit upsert must not add it back.
			if !s.Alive {
				continue
			}
			projectMgr.AutoAssignSession(projects.SessionInfo{
				ID:            s.ID,
				Cwd:           s.Cwd,
				WorkspaceRoot: s.WorkspaceRoot,
				Remotes:       s.Remotes,
				Host:          s.Peer,
				LocalHost:     s.Peer != "" && peerManager != nil && peerManager.IsLocalPeer(s.Peer),
				Alive:         s.Alive,
				Resumable:     s.Resumable,
				Slug:          s.Slug,
			})
		}
	}()

	// ── Health + Capabilities ──

	// composeHealth assembles the diagnostic blob shared between
	// GET /v1/health and the snapshot.world SSE event. It contains
	// everything the frontend needs to render headers, peer status,
	// session counts and update banners.
	//
	// The auth_token field is intentionally excluded here; the
	// /v1/health handler injects it only on local Unix-socket
	// connections. SSE always streams over the authenticated HTTP
	// path, so leaking the token through snapshot.world would be
	// a regression.
	composeHealth := func() map[string]any {
		data := map[string]any{
			"service": "gmuxd",
			"version": version,
			"node_id": nodeID,
			"status":  "ready",
		}
		// Node identity (ADR 0007): the live tailscale name when
		// connected, else the OS hostname. Evaluated per request so it
		// converges once the tailscale listener becomes ready.
		osHost, _ := os.Hostname()
		tsFQDN := ""
		if tsListener != nil {
			tsFQDN = tsListener.FQDN()
		}
		data["hostname"] = identity.Resolve(tsFQDN, osHost)
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
		if peers := currentPeers(peerManager); len(peers) > 0 {
			data["peers"] = peers
		}

		// Session summary.
		all := sessions.List()
		var localAlive, remoteAlive, dead int
		for _, s := range all {
			switch {
			case !s.Alive:
				dead++
			case s.Peer == "":
				localAlive++
			default:
				remoteAlive++
			}
		}
		data["sessions"] = map[string]int{
			"local_alive":  localAlive,
			"remote_alive": remoteAlive,
			"dead":         dead,
		}

		// runner_hash is the sha256 of the gmux runner binary on disk.
		// The frontend uses this (alongside runner_version on sessions)
		// to detect dev-mode builds where both sides report "dev" but
		// were compiled from different commits.
		if discovery.ExpectedRunnerHash != "" {
			data["runner_hash"] = discovery.ExpectedRunnerHash
		}

		// Launchers: what adapters can be launched on this host.
		data["default_launcher"] = launchConfig.DefaultLauncher
		data["launchers"] = launchConfig.Launchers

		return data
	}

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		data := composeHealth()
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
	//
	// In protocol 2 every reader is also an SSE subscriber and gets the
	// authoritative project view pushed via snapshot.world (ADR 0001).
	// GET /v1/projects is the one-shot project list. It is NOT legacy:
	// the v2 peering layer calls it (apiclient.GetProjects -> peer
	// fetchProjects) on connect and on every projects-update to refresh
	// the peer_projects projection a hub surfaces for this node. The
	// browser does not use it (it reads projects from snapshot.world).

	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		state, err := projectMgr.Load()
		if err != nil {
			log.Printf("projects: load error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to load projects")
			return
		}
		sessionInfos := buildSessionInfos(sessions, func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) })
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

		// Build match rules from the request.
		var rules []projects.MatchRule
		if req.Remote != "" {
			rules = append(rules, projects.MatchRule{
				Remote: projects.NormalizeRemote(req.Remote),
			})
		}
		for _, p := range req.Paths {
			rules = append(rules, projects.MatchRule{
				Path: paths.CanonicalizePath(p),
			})
		}

		// Derive slug: prefer remote repo name, fall back to first path basename.
		var slug string
		if req.Remote != "" {
			slug = projects.SlugFromRemote(req.Remote)
		} else {
			slug = projects.SlugFromPath(req.Paths[0])
		}

		// AddProject persists only on a valid result. A *ValidationError
		// means the add conflicts with existing state (e.g. a duplicate
		// path) and nothing was saved: report it as a 409 so the client
		// does not pin a reference to a project that was never created.
		item, err := projectMgr.AddProject(slug, rules)
		if err != nil {
			var verr *projects.ValidationError
			if errors.As(err, &verr) {
				log.Printf("projects: add rejected: %v", err)
				writeError(w, http.StatusConflict, "validation_error", verr.Error())
				return
			}
			log.Printf("projects: add error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
			return
		}
		// Populate the new project's sessions array with matching live
		// and resumable sessions immediately, so the frontend sees them
		// on the first fetch.
		projectMgr.AutoAssignAll(buildSessionInfos(sessions, func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) }))
		writeJSON(w, map[string]any{"ok": true, "data": item})
	})

	mux.HandleFunc("PATCH /v1/projects/{slug}/sessions", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}
		var req struct {
			Sessions []string `json:"sessions"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}
		found := false
		err = projectMgr.Update(func(state *projects.State) bool {
			found = state.ReorderSessions(slug, req.Sessions)
			return found
		})
		if err != nil {
			log.Printf("projects: reorder sessions error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// ── Peer-write proxy ──
	//
	// Generic forwarder for state that lives on a peer. The frontend
	// reaches it via `/v1/peers/{peer}/<rest>`; we forward to the peer
	// at `/<rest>` (which must already include the leading `/v1/...`).
	// Per ADR 0002, project membership and ordering are owned by the
	// session's origin host; the only correct way for the viewer to
	// reorder a peer's project is to ask the peer to do it.
	//
	// The proxy is intentionally narrow: it allowlists writes the
	// frontend actually issues today (project reorder), so a buggy or
	// hostile client can't drive arbitrary peer endpoints through us.
	mux.HandleFunc("/v1/peers/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/peers/")
		name, sub, ok := strings.Cut(rest, "/")
		if !ok || name == "" || sub == "" {
			writeError(w, http.StatusNotFound, "not_found", "peer path required")
			return
		}
		if !isAllowedPeerProxyPath(r.Method, sub) {
			writeError(w, http.StatusForbidden, "forbidden", "peer proxy: method+path not allowed")
			return
		}
		if peerManager == nil {
			writeError(w, http.StatusBadGateway, "unknown_peer", "no peers configured")
			return
		}
		peer := peerManager.GetPeer(name)
		if peer == nil {
			writeError(w, http.StatusBadGateway, "unknown_peer", fmt.Sprintf("peer %q not configured", name))
			return
		}
		peer.ForwardPath(w, r, "/"+sub)
	})

	// POST /v1/peers — connect to a host (ADR 0007 §5,§7). Probes the
	// target's /v1/health for its node_id + name, dedups by node_id
	// (already-known host reached again is a no-op, not a duplicate),
	// resolves any name collision viewer-side, then connects + persists.
	mux.HandleFunc("POST /v1/peers", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read error")
			return
		}
		var req struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}
		if peerManager == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "peering is not available")
			return
		}
		req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
		if err := peerstore.ValidateURL(req.URL); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		peerNodeID, name, err := probePeerHealth(r.Context(), req.URL, req.Token)
		if err != nil {
			writeError(w, http.StatusBadGateway, "unreachable", fmt.Sprintf("could not reach host: %v", err))
			return
		}

		// Atomic upsert: a known host (by node_id, else URL) refreshes its
		// URL/token in place; a new host is slugified, de-collided, and
		// persisted — all under one lock (no check-then-act race).
		rec, outcome, err := peerStore.AddOrGet(peerstore.Record{Name: name, URL: req.URL, Token: req.Token, NodeID: peerNodeID})
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if outcome == peerstore.Unchanged {
			writeJSON(w, map[string]any{"peer": rec, "already_connected": true})
			return
		}
		if outcome == peerstore.Updated {
			// Credentials changed (e.g. a token supplied for a host added
			// without one). AddPeer is a no-op when the name already exists,
			// so drop the live peer first to force a reconnect with them.
			peerManager.RemovePeer(rec.Name)
		}
		peerManager.AddPeer(config.PeerConfig{Name: rec.Name, URL: rec.URL, Token: rec.Token, Source: config.SourceManual})
		log.Printf("peering: connected to %s (%s)", rec.Name, rec.URL)
		writeJSON(w, map[string]any{"peer": rec, "updated": outcome == peerstore.Updated})
	})

	// DELETE /v1/peers/{name} — disconnect a manually-added peer.
	mux.HandleFunc("DELETE /v1/peers/{name}", func(w http.ResponseWriter, r *http.Request) {
		if peerManager == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "peering is not available")
			return
		}
		name := r.PathValue("name")
		rec, ok, err := peerStore.Remove(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to persist peers")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("peer %q is not a manually-added host", name))
			return
		}
		peerManager.RemovePeer(rec.Name)
		log.Printf("peering: disconnected from %s", rec.Name)
		writeJSON(w, map[string]any{"ok": true})
	})

	// ── Sessions ──

	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "data": sessions.List()})
	})

	// Conversation lookup — resolve dead conversations by (kind, slug)
	// for URL resolution. Returns file metadata + resume command.
	mux.HandleFunc("GET /v1/conversations/{kind}/{slug}", func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		slug := r.PathValue("slug")
		info, ok := convIndex.Lookup(kind, slug)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "conversation not found")
			return
		}
		writeJSON(w, map[string]any{
			"ok": true,
			"data": map[string]any{
				"slug":           info.Slug,
				"kind":           info.Kind,
				"title":          info.Title,
				"cwd":            info.Cwd,
				"resume_command": info.ResumeCommand,
				"created":        info.Created,
			},
		})
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
		if err := discovery.Register(sessions, subs, fileMon, req.SocketPath, persistDead); err != nil {
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
			Peer       string   `json:"peer"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
			return
		}

		// Forward to peer if requested. ForwardLaunch strips the peer
		// field from the body so the spoke treats it as a local launch.
		if req.Peer != "" {
			if peerManager == nil {
				writeError(w, http.StatusBadRequest, "unknown_peer", "no peers configured")
				return
			}
			if peer := peerManager.GetPeer(req.Peer); peer != nil {
				r.Body = io.NopCloser(bytes.NewReader(body))
				peer.ForwardLaunch(w, r)
				return
			}
			writeError(w, http.StatusBadRequest, "unknown_peer", fmt.Sprintf("peer %q not configured", req.Peer))
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
		// Expand ~ to absolute path for exec.Command.Dir.
		cwd = projects.NormalizePath(cwd)

		if gmuxBin == "" {
			writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found (install gmux alongside gmuxd)")
			return
		}

		// Fresh launch: no resume id, no size hint. CLI invocations
		// (`gmux <cmd>` in a terminal) detect their own TTY size
		// via localterm.TerminalSize(). Browser-initiated launches
		// have no TTY and currently no protocol slot for client
		// dimensions, so the PTY starts at 80x24 until the
		// browser's first resize WS message arrives. Same race as
		// /resume and /restart had before the size hints were
		// added; fixing /launch requires a protocol change to
		// carry cols/rows in the launch request and is left as a
		// follow-up.
		pid, err := launchGmux(gmuxBin, req.Command, cwd, "", 0, 0)
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

		// Route to peer if this is a remote session.
		if peerManager != nil && action != "" {
			if peer, originalID := peerManager.FindPeer(sessionID); peer != nil {
				if action == "attach" {
					// Attach returns the hub's own WS path (the hub proxies to the spoke).
					writeJSON(w, map[string]any{
						"ok": true,
						"data": map[string]any{
							"transport": "websocket",
							"ws_path":   "/ws/" + sessionID,
						},
					})
					return
				}
				peer.Forward(w, r, originalID, action)
				return
			}
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

			// The runner receives --resume-id and registers under the
			// same id, so Register() lands in its re-registration
			// branch and the session keeps its identity (and its
			// scrollback directory). See ADR 0003. The size hints
			// preserve the last-known PTY dimensions through the
			// fork; without them claude / vim / prompt frameworks
			// reading $COLUMNS at startup would clamp to 80.
			resumeCwd := projects.NormalizePath(sess.Cwd)
			pid, err := launchGmux(gmuxBin, sess.Command, resumeCwd, sessionID, sess.TerminalCols, sess.TerminalRows)
			if err != nil {
				log.Printf("resume: failed to start gmux: %v", err)
				writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
				return
			}

			// Don't modify the session here. It stays dead/resumable
			// until the runner calls POST /register and the
			// re-registration upsert flips alive=true.
			// The frontend shows a local "resuming" indicator.
			log.Printf("resume: started gmux pid=%d for %s cwd=%s", pid, sessionID, resumeCwd)
			writeJSON(w, map[string]any{
				"ok":   true,
				"data": map[string]any{"pid": pid, "session_id": sessionID},
			})

		case "restart":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			// Serialize with /resume to prevent double-click races.
			resumeMu.Lock()
			defer resumeMu.Unlock()

			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			if gmuxBin == "" {
				writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found")
				return
			}

			// If the runner is alive, kill it and wait for the exit lifecycle
			// to transition the session to resumable (Alive=false + resume Command).
			if sess.Alive {
				if sess.SocketPath == "" {
					writeError(w, http.StatusBadRequest, "no_socket", "alive session missing socket")
					return
				}
				// Subscribe BEFORE killing so we don't miss the exit upsert.
				evCh, unsub := sessions.Subscribe()
				defer unsub()
				if err := discovery.KillSession(sess.SocketPath); err != nil {
					log.Printf("restart: %s: kill failed: %v", sessionID, err)
					writeError(w, http.StatusInternalServerError, "kill_failed", err.Error())
					return
				}
				exited, ok := waitForSessionExit(sessions, evCh, sessionID, 5*time.Second, 500*time.Millisecond)
				if !ok {
					writeError(w, http.StatusGatewayTimeout, "kill_timeout", "session did not exit in time")
					return
				}
				sess = exited
				// /kill releases the canonical socket path before
				// responding 204, so by the time KillSession returned
				// (above) the path was already free. The replacement
				// runner's BindSocket below cannot race against the old
				// runner's lingering listener for path ownership.
			}

			if sess.Alive || len(sess.Command) == 0 {
				writeError(w, http.StatusBadRequest, "not_resumable", "session is not resumable")
				return
			}

			// Same as /resume: launch a new runner under the existing
			// session id; Register's re-registration branch handles
			// the rest.
			restartCwd := projects.NormalizePath(sess.Cwd)
			pid, err := launchGmux(gmuxBin, sess.Command, restartCwd, sessionID, sess.TerminalCols, sess.TerminalRows)
			if err != nil {
				log.Printf("restart: failed to start gmux: %v", err)
				writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
				return
			}
			log.Printf("restart: started gmux pid=%d for %s cwd=%s", pid, sessionID, restartCwd)
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
					persistDead(sess)
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

		case "input":
			// Cross-peer `gmux --send`. The peer-routing branch above
			// already forwarded any non-local session, so by here the
			// session is owned by this gmuxd. Reading the body up to
			// the runner's cap before forwarding means we don't tie up
			// a runner connection on a slow CLI stdin: gmuxd absorbs
			// the backpressure.
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
				return
			}
			sess, ok := sessions.Get(sessionID)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			if !sess.Alive || sess.SocketPath == "" {
				writeError(w, http.StatusConflict, "not_running", "session is not running")
				return
			}
			// Mirror the runner's 1 MiB cap so the error appears at the
			// edge (here) rather than as silent truncation inside the
			// runner.
			body, err := io.ReadAll(io.LimitReader(r.Body, maxInputBytes+1))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
				return
			}
			if int64(len(body)) > maxInputBytes {
				writeError(w, http.StatusRequestEntityTooLarge, "too_large",
					fmt.Sprintf("input exceeds %d bytes", maxInputBytes))
				return
			}
			if err := discovery.SendInput(r.Context(), sess.SocketPath, bytes.NewReader(body)); err != nil {
				log.Printf("input: %s: %v", sessionID, err)
				writeError(w, http.StatusBadGateway, "runner_unreachable", err.Error())
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case "scrollback":
			scrollbackBrokerHandler(w, r, sessionID, sessions, metaStore.SessionDir)

		case "clipboard":
			// Materialize a clipboard binary payload as a file in this
			// gmuxd's os.TempDir() and return the absolute path. For
			// devcontainer/peer sessions, the request was already
			// forwarded above to the gmuxd that owns the session, so
			// reaching this branch always means "write locally". The
			// session must exist; we don't otherwise need fields from it.
			if _, ok := sessions.Get(sessionID); !ok {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			clipboardHandler(clipfile.NewLocalWriter(os.TempDir())).ServeHTTP(w, r)

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
			// Let the adapter perform any cleanup (e.g. removing a state file).
			if a := adapters.FindByKind(sess.Kind); a != nil {
				if fin, ok := a.(adapter.SessionFinalizer); ok {
					fin.OnDismiss(sessionID, projects.NormalizePath(sess.Cwd))
				}
			}
			// Remove session from its project's sessions array.
			projectMgr.DismissSession(sessionID, sess.Slug)
			// Remove from store — broadcasts session-remove to all clients
			// (which the cleanup goroutine catches to drop meta), then
			// also drop meta synchronously to defeat any subscriber lag.
			sessions.Remove(sessionID)
			forgetMeta(sessionID)
			if subs != nil {
				subs.Unsubscribe(sessionID)
			}
			if fileMon != nil {
				fileMon.NotifySessionDied(sessionID)
			}
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})

		case "wait":
			handleWait(w, r, sessions, sessionID)

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
		if sess.Peer != "" {
			// Remote session: return empty socket path. The WS handler
			// checks for this and uses the peer proxy path instead.
			return "", fmt.Errorf("session %s is remote (peer: %s)", sessionID, sess.Peer)
		}
		if sess.SocketPath == "" {
			return "", fmt.Errorf("session %s has no socket", sessionID)
		}
		return sess.SocketPath, nil
	}, sessions)

	// WS handler: local sessions use the Unix proxy, remote sessions
	// are proxied to the spoke's WS endpoint over TCP.
	mux.HandleFunc("/ws/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("sessionID")

		// Check if this is a remote session.
		if peerManager != nil {
			if peer, originalID := peerManager.FindPeer(sessionID); peer != nil {
				peer.ProxyWS(w, r, originalID)
				return
			}
		}

		// Local session: use the existing Unix socket proxy.
		wsProxy.Handler()(w, r)
	})

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

	// ── Snapshot push protocol (ADR 0001) ──
	//
	// Two coalesced kinds plus one bare event:
	//
	//   snapshot.sessions  trigger: any session state change
	//   snapshot.world     trigger: projects-update | peer-status
	//   session-activity   bare: forwarded as-is, lossy, not coalesced
	//
	// The coalescers emit `struct{}` triggers; the SSE handler
	// composes the actual payload at emit time by reading current
	// state. This avoids snapshotting on every Push (most of which
	// are coalesced away) and keeps memory bounded under bursts.
	const snapshotWindow = 50 * time.Millisecond
	sessionsCoalescer := coalesce.New[struct{}](snapshotWindow)
	worldCoalescer := coalesce.New[struct{}](snapshotWindow)

	// Pump: a single goroutine watches the broadcast bus and routes
	// each event type to the right coalescer. session-activity is
	// not coalesced; it stays on the broadcast bus and SSE handlers
	// subscribe directly.
	//
	// Per-event broadcast types are kept (session-upsert,
	// session-remove, projects-update, peer-status) because they
	// remain the canonical signal that *something* changed; protocol
	// 2 just stops shipping them on the wire.
	pumpCh, cancelPump := sessions.Subscribe()
	defer cancelPump()
	go func() {
		for ev := range pumpCh {
			pushSessions, pushWorld := snapshotPumpRoute(ev.Type)
			if pushSessions {
				sessionsCoalescer.Push(struct{}{})
			}
			if pushWorld {
				worldCoalescer.Push(struct{}{})
			}
		}
	}()

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

		// asPeer consumers (?as=peer) get owned sessions only and no
		// world snapshots. Browser consumers get everything.
		asPeer := r.URL.Query().Get("as") == "peer"

		// isLocalPeer wraps the manager's check so it tolerates a
		// nil manager (test harnesses, single-node mode).
		isLocalPeer := func(name string) bool {
			return peerManager != nil && peerManager.IsLocalPeer(name)
		}
		// isOwned: own sessions (Peer=="") + Local-peer (devcontainer)
		// sessions belong to this node and ride peer feeds; network
		// peer sessions don't forward through this node to others.
		isOwned := func(s *store.Session) bool {
			if s.Peer == "" {
				return true
			}
			return isLocalPeer(s.Peer)
		}

		// snapshot.sessions filter differs by subscriber kind:
		//
		//  - asPeer (?as=peer, another hub): owned only. The peer
		//    reconciles its view of network-peer sessions directly
		//    from the originating peers; forwarding them via this
		//    node would create double delivery and false-origin
		//    attribution (ADR 0002).
		//  - browser: every session in the local store, including
		//    namespaced mirrors of network-peer sessions. ADR 0002
		//    needs these so peer-owned folders render in the sidebar
		//    with the origin's chosen order.
		composeSessions := func() snapshot.SessionsPayload {
			if asPeer {
				return snapshot.ComposeSessions(sessions.List(), isOwned)
			}
			return snapshot.ComposeSessions(sessions.List(), nil)
		}

		composeWorld := func() snapshot.WorldPayload {
			state, err := projectMgr.Load()
			var items []projects.Item
			if err == nil {
				items = state.Items
			} else {
				log.Printf("snapshot.world: projects load: %v", err)
			}
			health := composeHealth()
			return snapshot.WorldPayload{
				Projects:        items,
				Peers:           currentPeers(peerManager),
				Health:          health,
				Launchers:       launchConfig.Launchers,
				DefaultLauncher: launchConfig.DefaultLauncher,
				PeerProjects:    composePeerProjects(peerManager),
				PeerDiscovered:  composePeerDiscovered(peerManager),
			}
		}

		sessionsSub, cancelSessions := sessionsCoalescer.Subscribe()
		defer cancelSessions()
		var worldSub <-chan struct{}
		var cancelWorld func()
		if !asPeer {
			worldSub, cancelWorld = worldCoalescer.Subscribe()
			defer cancelWorld()
		}

		// Activity events stay on the bare broadcast bus: lossy,
		// per-session, no batching. Filter for owned sessions so
		// peer consumers don't see network-peer activity.
		activityCh, cancelActivity := sessions.Subscribe()
		defer cancelActivity()

		// Initial snapshots (leading edge: subscriber is idle).
		sendSSE(w, "snapshot.sessions", composeSessions())
		if !asPeer {
			sendSSE(w, "snapshot.world", composeWorld())
		}

		flusher.Flush()

		// Heartbeat: send an SSE comment every 30s to keep the connection
		// alive through idle periods. Without this, the hub's sseclient
		// idle timeout (60s) would fire on legitimately idle spokes, and
		// the browser's EventSource would have no way to detect a dead
		// hub connection.
		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()

		notify := r.Context().Done()
		for {
			select {
			case <-notify:
				return
			case <-heartbeat.C:
				fmt.Fprint(w, ":\n\n")
				flusher.Flush()
			case _, open := <-sessionsSub:
				if !open {
					return
				}
				sendSSE(w, "snapshot.sessions", composeSessions())
				flusher.Flush()
			case _, open := <-worldSub:
				if !open {
					return
				}
				sendSSE(w, "snapshot.world", composeWorld())
				flusher.Flush()
			case ev, open := <-activityCh:
				if !open {
					return
				}
				switch ev.Type {
				case "session-activity":
					// Peer subscribers (?as=peer) get activity for owned
					// sessions only; browser subscribers see everything so
					// peer-owned session indicators update.
					if !shouldForwardActivity(asPeer, ev.ID, isLocalPeer) {
						continue
					}
					sendSSE(w, "session-activity", ev)
					flusher.Flush()
				case "projects-update":
					// Peer-hub trigger only. A `?as=peer` subscriber has
					// no snapshot.world, so it relies on this event to
					// re-fetch our project list (GET /v1/projects) and
					// refresh the peer_projects it surfaces upstream.
					// Browser subscribers ignore it — they already get
					// projects via snapshot.world (projects-update fires
					// the world coalescer, see snapshotPumpRoute) — so we
					// don't waste the frame on them.
					if !asPeer {
						continue
					}
					sendSSE(w, ev.Type, ev)
					flusher.Flush()
				}
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

	// shutdownCh is closed by the /v1/shutdown handler to trigger the
	// same graceful exit path as SIGINT/SIGTERM. Without this, the
	// handler only shut down HTTP listeners, leaving background
	// goroutines (peering, discovery, file monitors) running
	// indefinitely as a zombie process.
	shutdownCh := make(chan struct{})
	var shutdownOnce sync.Once

	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		shutdownOnce.Do(func() {
			log.Printf("shutdown requested — exiting")
			close(shutdownCh)
		})
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

	// ── Sleep detection ──

	sleepWatcher := sleep.NewWatcher()
	defer sleepWatcher.Stop()

	// ── Peer connections (hub protocol) ──

	hostname, _ := os.Hostname()
	manualPeers := peerStore.PeerConfigs()
	// Always create the manager: peers can be added at runtime via
	// /v1/peers (ADR 0007 §5), so it must exist even when nothing is
	// configured or discovered at startup.
	peerManager = peering.NewManager(manualPeers, sessions, hostname)
	// Prune namespaced projects.json keys when a Local peer
	// (devcontainer) is removed: its `id@<peer>` session keys can
	// never resolve again and would accumulate dead weight in the
	// parent's projects.json.
	peerManager.OnPeerRemoved = func(name string, wasLocal bool) {
		if wasLocal {
			projectMgr.PruneNamespacedKeys(name)
		}
	}
	peerManager.Start()
	if len(manualPeers) > 0 {
		log.Printf("peering: %d manual peer(s) loaded", len(manualPeers))
	}

	// Reconnect all peers after system sleep.
	go func() {
		for range sleepWatcher.C() {
			peerManager.OnSleep()
		}
	}()

	// ── Devcontainer discovery ──

	var dcWatcher *devcontainers.Watcher
	if cfg.Discovery.Devcontainers {
		dcWatcher = devcontainers.NewWatcher(peerManager)
		if dcWatcher != nil {
			dcWatcher.Start()
			log.Printf("devcontainers: discovery enabled")
		} else {
			log.Printf("devcontainers: docker CLI not found, skipping discovery")
		}
	}

	// Signal channel: declared here so the tailscale discovery goroutine
	// can select on it to avoid blocking shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// ── Optional tailscale listener ──

	if cfg.Tailscale.Enabled {
		// Requested tailscale name for *first* registration (ADR 0007):
		// GMUXD_TS_HOSTNAME wins verbatim (used by dev-server and any
		// multi-instance/container setup that needs a distinct name on a
		// shared OS hostname); otherwise derive "gmux-<os-hostname>".
		// Once registered, tailscale owns the name and this is ignored.
		tsSeed := strings.TrimSpace(os.Getenv("GMUXD_TS_HOSTNAME"))
		if tsSeed == "" {
			tsSeed = tsauth.SeedFromHostname(hostname)
		}
		// The tailnet handler is the *token-authenticated* handler, not the
		// raw mux: tsauth's identity check is the outer gate (only the
		// owner's tailnet login may reach the prompt) and netauth's bearer
		// token is the inner gate (ADR 0008). A passing tailnet identity no
		// longer grants the full API on its own.
		tsListener = tsauth.Start(tsauth.Config{
			Hostname: tsSeed,
			Allow:    cfg.Tailscale.Allow,
		}, stateDir, authedHandler)
		defer tsListener.Shutdown()
	}

	// ── Signal handling for graceful shutdown ──

	log.Printf("gmuxd %s ready", version)

	select {
	case sig := <-sigCh:
		log.Printf("received %v — shutting down", sig)
	case <-shutdownCh:
		log.Printf("shutdown requested — shutting down")
	}

	if dcWatcher != nil {
		dcWatcher.Stop()
	}
	if peerManager != nil {
		peerManager.Stop()
	}

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
			Sessions        *struct {
				LocalAlive  int `json:"local_alive"`
				RemoteAlive int `json:"remote_alive"`
				Dead        int `json:"dead"`
			} `json:"sessions,omitempty"`
			Peers []struct {
				Name         string `json:"name"`
				URL          string `json:"url"`
				Status       string `json:"status"`
				SessionCount int    `json:"session_count"`
				LastError    string `json:"last_error,omitempty"`
			} `json:"peers,omitempty"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		_, _ = fmt.Fprintf(stderr, "gmuxd: unexpected health response\n")
		return 1
	}

	d := health.Data
	_, _ = fmt.Fprintf(stdout, "gmuxd %s (%s)\n", d.Version, d.Status)
	_, _ = fmt.Fprintf(stdout, "  tcp:    %s\n", d.Listen)
	_, _ = fmt.Fprintf(stdout, "  socket: %s\n", sock)
	if d.TailscaleURL != "" {
		_, _ = fmt.Fprintf(stdout, "  remote: %s\n", d.TailscaleURL)
	}
	if d.UpdateAvailable != "" {
		_, _ = fmt.Fprintf(stdout, "  update: %s available\n", d.UpdateAvailable)
	}

	// Sessions.
	if s := d.Sessions; s != nil {
		total := s.LocalAlive + s.RemoteAlive + s.Dead
		_, _ = fmt.Fprintf(stdout, "\nSessions: %d alive", s.LocalAlive+s.RemoteAlive)
		if s.RemoteAlive > 0 {
			_, _ = fmt.Fprintf(stdout, " (%d local, %d remote)", s.LocalAlive, s.RemoteAlive)
		}
		_, _ = fmt.Fprintf(stdout, ", %d dead (%d total)\n", s.Dead, total)
	}

	// Peers.
	if len(d.Peers) > 0 {
		_, _ = fmt.Fprintf(stdout, "\nPeers:\n")
		for _, p := range d.Peers {
			var detail string
			switch p.Status {
			case "connected":
				detail = fmt.Sprintf("%d session%s", p.SessionCount, plural(p.SessionCount))
			case "connecting":
				detail = "connecting..."
			case "offline":
				detail = "offline"
			default:
				if p.LastError != "" {
					detail = p.LastError
				} else {
					detail = "disconnected"
				}
			}
			_, _ = fmt.Fprintf(stdout, "  %s %s (%s)\n", statusDot(p.Status), p.Name, detail)
			_, _ = fmt.Fprintf(stdout, "    %s\n", p.URL)
		}
	}

	return 0
}

// composePeerProjects gathers each connected peer's cached project
// projection into a map keyed by peer name. Returned as map[string][]
// SpokeProject; the snapshot.world JSON tag handles the rest.
//
// Peers that haven't been fetched yet (or whose fetch failed) appear
// with an empty list. The viewer still gets the key so it knows the
// peer is enumerable; the list fills in once the first fetch lands.
func composePeerProjects(mgr *peering.Manager) map[string][]peering.SpokeProject {
	if mgr == nil {
		return nil
	}
	infos := mgr.PeerStatus()
	if len(infos) == 0 {
		return nil
	}
	out := make(map[string][]peering.SpokeProject, len(infos))
	for _, info := range infos {
		if info.Local {
			continue
		}
		p := mgr.GetPeer(info.Name)
		if p == nil {
			continue
		}
		projects, _ := p.CachedProjects()
		if projects == nil {
			projects = []peering.SpokeProject{}
		}
		out[info.Name] = projects
	}
	return out
}

// composePeerDiscovered gathers each connected peer's self-advertised
// discovered list into a map keyed by peer name. Discovery is
// host-authoritative (ADR 0002/0005): the viewer renders each peer's
// own discovered rows verbatim instead of recomputing them blind. Local
// peers are skipped (their sessions flow through the parent's local
// discovery). Peers not yet fetched appear with an empty list.
func composePeerDiscovered(mgr *peering.Manager) map[string][]peering.SpokeDiscovered {
	if mgr == nil {
		return nil
	}
	infos := mgr.PeerStatus()
	if len(infos) == 0 {
		return nil
	}
	out := make(map[string][]peering.SpokeDiscovered, len(infos))
	for _, info := range infos {
		if info.Local {
			continue
		}
		p := mgr.GetPeer(info.Name)
		if p == nil {
			continue
		}
		discovered, _ := p.CachedDiscovered()
		if discovered == nil {
			discovered = []peering.SpokeDiscovered{}
		}
		out[info.Name] = discovered
	}
	return out
}

// currentPeers returns the manager's active peer status list, or nil if
// there are none. (Offline tailnet-discovery hints were removed with
// tsdiscovery in ADR 0008.)
func currentPeers(mgr *peering.Manager) []peering.PeerInfo {
	if mgr != nil && mgr.HasPeers() {
		return mgr.PeerStatus()
	}
	return nil
}

func statusDot(status string) string {
	switch status {
	case "connected":
		return "\u2022" // bullet
	case "connecting":
		return "\u25cb" // open circle
	case "offline":
		return "\u25cb" // open circle
	default:
		return "\u2717" // X mark
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
			Listen       string `json:"listen"`
			AuthToken    string `json:"auth_token"`
			TailscaleURL string `json:"tailscale_url"`
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
	_, _ = fmt.Fprintf(stdout, "\nOpen this URL to authenticate in a browser on this machine:\n  %s\n", url)

	// When tailscale is enabled, the same token-in-URL form over the
	// tailnet FQDN is the connect string for pairing from another gmux
	// machine: paste it into "Connect to host", which splits it back into
	// {url, token} (ADR 0008 — tailnet peers are added manually).
	if health.Data.TailscaleURL != "" {
		connectURL := fmt.Sprintf("%s/auth/login?token=%s", strings.TrimRight(health.Data.TailscaleURL, "/"), health.Data.AuthToken)
		_, _ = fmt.Fprintf(stdout, "\nTo add this host from another gmux machine, paste this into \"Connect to host\":\n  %s\n", connectURL)
		// Inline QR for the same connect URL: scan from a phone on the
		// tailnet to open gmux authenticated, no typing the token.
		_, _ = fmt.Fprintf(stdout, "\nOr scan to open gmux on a device on your tailnet:\n")
		qrterminal.GenerateHalfBlock(connectURL, qrterminal.L, stdout)
	}

	return 0
}

// snapshotPumpRoute decides which protocol-2 coalescers a given
// internal broadcast event triggers. Returned in (pushSessions,
// pushWorld) order. Unknown event types fire neither.
//
//   - session-upsert / session-remove fire snapshot.sessions only.
//   - peer-status fires snapshot.world only (peer state lives in
//     the world bundle, not in the per-session payload).
//   - projects-update fires both: projects.Manager.Broadcast runs
//     Reconcile beforehand, which silently re-stamps every owned
//     session's ProjectSlug / ProjectIndex. Without re-emitting
//     snapshot.sessions, the UI would keep rendering each session
//     under its previous project until the next unrelated session
//     change.
func snapshotPumpRoute(eventType string) (pushSessions, pushWorld bool) {
	switch eventType {
	case "session-upsert", "session-remove":
		return true, false
	case "peer-status":
		return false, true
	case "projects-update":
		return true, true
	}
	return false, false
}

// shouldForwardActivity decides whether a session-activity event
// should be sent to a given SSE subscriber.
//
//   - asPeer subscribers (?as=peer, i.e. another node's hub) only
//     receive activity for sessions this node owns (own sessions or
//     sessions on a Local peer such as a devcontainer). Activity for
//     network-peer sessions reaches the requesting hub directly from
//     the origin; forwarding it via this node would create double-
//     delivery and false-origin attribution.
//   - Browser subscribers receive every activity event the local
//     daemon sees, including namespaced activity that hubs have
//     re-broadcast from network peers — the UI renders all of those
//     sessions and needs the indicator updates.
func shouldForwardActivity(asPeer bool, sessionID string, isLocalPeer func(string) bool) bool {
	if !asPeer {
		return true
	}
	_, peerName := peering.ParseID(sessionID)
	if peerName == "" {
		return true
	}
	return isLocalPeer != nil && isLocalPeer(peerName)
}

// isAllowedPeerProxyPath gates the generic /v1/peers/{peer}/...
// proxy. We deliberately allowlist a small surface, scoped to writes
// the frontend actually issues for peer-owned state today, rather
// than blanket-forwarding every method+path. New peer-write features
// extend this function explicitly.
//
// `sub` is the path relative to the peer's API root, with no leading
// slash (e.g. "v1/projects/gmux/sessions").
func isAllowedPeerProxyPath(method, sub string) bool {
	parts := strings.Split(sub, "/")
	// Project session reorder: PATCH v1/projects/<slug>/sessions.
	// The frontend uses this to push a new session order into a
	// peer's projects.json. The peer applies the change atomically
	// and re-broadcasts the resulting stamps, which we observe over
	// SSE; no local optimistic mirror is needed because the viewer
	// doesn't own the projects.json being reordered.
	if method == http.MethodPatch &&
		len(parts) == 4 &&
		parts[0] == "v1" && parts[1] == "projects" &&
		parts[2] != "" && parts[3] == "sessions" {
		return true
	}
	// Create a project on a peer: POST v1/projects/add.
	// The frontend uses this to add a project on the peer's own
	// projects.json from the Manage Projects modal (Discovered
	// section, remote items). The peer applies the add atomically;
	// the resulting items[] flows back via projects-update.
	if method == http.MethodPost &&
		len(parts) == 3 &&
		parts[0] == "v1" && parts[1] == "projects" && parts[2] == "add" {
		return true
	}
	return false
}

// sessionLastActive returns the timestamp used to sort discovered
// suggestions by recency: the session's last_activity_at, falling back
// to created_at when no activity has been recorded yet.
func sessionLastActive(s store.Session) string {
	if s.LastActivityAt != "" {
		return s.LastActivityAt
	}
	return s.CreatedAt
}

// buildSessionInfos converts store sessions to project SessionInfo structs.
func buildSessionInfos(sessions *store.Store, isLocalPeer func(string) bool) []projects.SessionInfo {
	list := sessions.List()
	infos := make([]projects.SessionInfo, len(list))
	for i, s := range list {
		infos[i] = projects.SessionInfo{
			ID:            s.ID,
			Cwd:           s.Cwd,
			WorkspaceRoot: s.WorkspaceRoot,
			Remotes:       s.Remotes,
			Host:          s.Peer,
			LocalHost:     s.Peer != "" && isLocalPeer != nil && isLocalPeer(s.Peer),
			Alive:         s.Alive,
			Resumable:     s.Resumable,
			Slug:          s.Slug,
			LastActive:    sessionLastActive(s),
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

// probePeerHealth fetches the target's /v1/health and returns its opaque
// node_id and self-reported name (ADR 0007). The add-peer flow uses the
// node_id to dedup and adopts the name as the peer's routing identity.
// Manual peers use the default transport (as [[peers]] did before).
func probePeerHealth(ctx context.Context, baseURL, token string) (nodeID, name string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/health", nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("health returned %s", resp.Status)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Service  string `json:"service"`
			NodeID   string `json:"node_id"`
			Hostname string `json:"hostname"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&env); err != nil {
		return "", "", fmt.Errorf("parsing health: %w", err)
	}
	// Confirm it's actually gmuxd, so a stray HTTP endpoint can't be
	// registered as a peer.
	if !env.OK || env.Data.Service != "gmuxd" {
		return "", "", fmt.Errorf("host is not running gmux")
	}
	if env.Data.Hostname == "" {
		return "", "", fmt.Errorf("host did not report a name")
	}
	return env.Data.NodeID, env.Data.Hostname, nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"error": map[string]any{"code": code, "message": message},
	})
}
