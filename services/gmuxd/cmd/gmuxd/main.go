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
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
	qrterminal "github.com/mdp/qrterminal/v3"
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
	result, err := launchRunnerProcess(context.Background(), runnerLaunchRequest{GmuxBin: gmuxBin, Command: command, CWD: cwd, ResumeID: resumeID, InitialCols: initialCols, Rows: initialRows})
	if err != nil {
		return 0, err
	}
	return result.PID, nil
}

// resolveResumeDir picks the directory to (re)launch a runner in for a
// dead session whose original cwd may have been deleted. Order:
//
//  1. the session's stored cwd, if it still exists;
//  2. the owning project's canonical dir (first match-rule path), if it
//     exists — so a resume lands where the project "+" button would;
//  3. $HOME, as a last resort, so resume still succeeds.
//
// Returns the chosen dir and whether a fallback (anything but the
// original cwd) was used. Returns ("", false) only when none of the
// candidates exist as directories — the caller then returns a clear
// cwd_missing error instead of letting exec fail with the misleading
// "fork/exec .../gmux: no such file or directory" (Go reports the
// child's chdir ENOENT against argv0).
//
// Every candidate is Stat'd via projects.IsDir, so a missing target can
// never reach exec.
func relaunchData(sessionID string, pid int, originalCwd, usedCwd string, fellBack bool) map[string]any {
	data := map[string]any{"pid": pid, "session_id": sessionID}
	if fellBack {
		data["original_cwd"] = originalCwd
		data["fallback_cwd"] = usedCwd
	}
	return data
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
  state              Check, back up, or export the daemon state database
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
	case "state":
		return runState(args, stdout, stderr)
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

// startBackground is a pure spawn-first exact-child supervisor. The child
// owns verification, incumbent shutdown, the lifetime lock, and database open.
func startBackground(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: cannot determine own path: %v\n", err)
		return 1
	}
	stateDir := paths.StateDir()
	logPath := filepath.Join(stateDir, "gmuxd.log")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: cannot create state dir %s: %v\n", stateDir, err)
		return 1
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: cannot open log %s: %v\n", logPath, err)
		return 1
	}
	defer logFile.Close()
	pid, incumbent, replaced, err := startBackgroundProduction(context.Background(), exe, paths.SocketPath(), stateDir, logFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: daemon failed to become healthy: %v\n  Logs: %s\n", err, logPath)
		return 1
	}
	if replaced {
		if incumbent.Version != "" {
			_, _ = fmt.Fprintf(stdout, "gmuxd: replaced existing daemon (%s, pid %d)\n", incumbent.Version, incumbent.PID)
		} else {
			_, _ = fmt.Fprintf(stdout, "gmuxd: replaced existing daemon (pid %d)\n", incumbent.PID)
		}
	}
	_, _ = fmt.Fprintf(stdout, "gmuxd: running %s (pid %d)\n  Logs: %s\n", version, pid, logPath)
	if replaced {
		_, _ = fmt.Fprintf(stdout, "  Note: active sessions will use the new version when restarted.\n")
	}
	return 0
}

func serve(stderr io.Writer) int {
	return serveCentral(stderr)
}

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
// host-authoritative (ADR 0002/0025): the viewer renders each peer's
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
	// peer-owned project catalog. The peer applies the change atomically
	// and re-broadcasts the resulting stamps, which we observe over
	// SSE; no local optimistic mirror is needed because the viewer
	// doesn't own the reordered state.
	if method == http.MethodPatch &&
		len(parts) == 4 &&
		parts[0] == "v1" && parts[1] == "projects" &&
		parts[2] != "" && parts[3] == "sessions" {
		return true
	}
	// Create a project on a peer: POST v1/projects/add.
	// The frontend uses this to add a project on the peer's own
	// durable catalog from the Manage Projects modal (Discovered
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
// suggestions by recency: the session's last_output_at, falling back
// to created_at when no activity has been recorded yet.
const sseWriteTimeout = 10 * time.Second

func sendSSEFrame(rc *http.ResponseController, w io.Writer, event string, payload any) error {
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if err := sendSSE(w, event, payload); err != nil {
		return err
	}
	return rc.Flush()
}

// sendSSEComment writes a heartbeat comment frame (":") with a fresh
// write deadline and flushes it. Same error contract as sendSSEFrame.
func sendSSEComment(rc *http.ResponseController, w io.Writer) error {
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprint(w, ":\n\n"); err != nil {
		return err
	}
	return rc.Flush()
}

// sendSSE writes one SSE frame. A marshal failure skips the frame
// (payload bug, not a connection problem); a write failure is
// returned so the caller can treat it as a client disconnect.
func sendSSE(w io.Writer, event string, payload any) error {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, bytes)
	return err
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// probePeerHealth fetches the target's /v1/health and returns its opaque
// node_id and self-reported name (ADR 0007). The add-peer flow uses the
// node_id to dedup and adopts the name as the peer's routing identity.
// The transport routes same-tailnet MagicDNS hosts through tsnet once
// it is ready; everything else uses the default transport (#281).
func probePeerHealth(ctx context.Context, transport http.RoundTripper, baseURL, token string) (nodeID, name string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/health", nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
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
