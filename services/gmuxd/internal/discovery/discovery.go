// Package discovery scans /tmp/gmux-sessions/*.sock for live gmux-run
// instances and queries their GET /meta endpoint to populate the store.
// Replaces the old file-polling approach from /tmp/gmux-meta/.
package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// ExpectedRunnerHash is the sha256 hash of the gmux binary that gmuxd
// would launch for new sessions. Set by main at startup. Exposed via
// /v1/health as runner_hash so the frontend can detect dev-mode hash drift.
var ExpectedRunnerHash string

func socketDir() string {
	if d := os.Getenv("GMUX_SOCKET_DIR"); d != "" {
		return d
	}
	return "/tmp/gmux-sessions"
}

// OnDeadFunc is invoked after a session has just landed as Alive=false
// in the store, with the post-Upsert snapshot. nil is allowed.
//
// Three call sites fire it:
//
//   - Scan's socket-gone phase, when a previously-alive session's
//     runner is no longer reachable.
//   - Register's fresh-upsert path, when the runner's /meta already
//     reports alive=false (fast-exit commands like `echo` whose
//     runner finishes before queryMeta arrives).
//   - Subscriptions.OnDead, after the SSE exit handler upserts.
type OnDeadFunc func(sess store.Session)

// Watch periodically scans for Unix sockets and queries their /meta.
// When a new session is found, it subscribes to the runner's /events SSE
// for real-time status/meta/exit updates.
//
// onFirstScan, if non-nil, runs once after the initial Scan completes.
// This is the right point to invoke work that depends on live sessions
// being registered with the FileMonitor (e.g. applying persisted
// attributions to freshly-rehydrated runners).
func Watch(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, onDead OnDeadFunc, onFirstScan func(), interval time.Duration, stop <-chan struct{}) {
	// Initial scan immediately
	Scan(sessions, subs, fileMon, onDead)
	if onFirstScan != nil {
		onFirstScan()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			subs.UnsubscribeAll()
			return
		case <-ticker.C:
			Scan(sessions, subs, fileMon, onDead)
		}
	}
}

// Scan finds all .sock files and queries each runner's /meta endpoint.
// Reachable sockets → upsert session + subscribe to /events.
// Unreachable → remove + cleanup + unsubscribe.
func Scan(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, onDead OnDeadFunc) {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("discovery: read dir: %v", err)
		}
		return
	}

	// Build set of sockets already tracked by a store session.
	trackedSockets := make(map[string]bool)
	for _, s := range sessions.List() {
		if s.SocketPath != "" {
			trackedSockets[s.SocketPath] = true
		}
	}

	// Phase 1: discover new sockets → Register is the single entry
	// point for creating/merging sessions.
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}
		sockPath := filepath.Join(dir, entry.Name())
		if trackedSockets[sockPath] {
			continue // already tracked
		}
		if err := Register(sessions, subs, fileMon, sockPath, onDead); err != nil {
			// Only remove sockets old enough to be genuinely stale.
			// A brand-new socket may not be listening yet (runner still starting).
			if info, serr := entry.Info(); serr == nil && time.Since(info.ModTime()) > 10*time.Second {
				os.Remove(sockPath)
			}
		}
	}

	// Phase 2: detect dead sessions whose runner is no longer reachable.
	//
	// The active /events subscription is the primary liveness signal:
	// while we hold an SSE stream from the runner, the runner is by
	// definition still talking to us, regardless of what the socket
	// path looks like in the filesystem. Notably, ptyserver.handleKill
	// unlinks the socket path before the runner has finished its
	// shutdown (so a replacement runner can BindSocket without racing
	// the dying listener; see ADR 0003); during that window the path
	// is gone but the SSE subscription is still streaming the runner's
	// final exit event. Treating the missing path as a death signal
	// would race ahead of the exit event and call NotifySessionDied,
	// dropping the file→session attribution that resume / restart
	// expects to keep across the seam.
	//
	// Only when the subscription itself has dropped do we fall back to
	// stat / probe to distinguish a stale path from a live runner whose
	// SSE blip we'll reconnect to.
	for _, s := range sessions.List() {
		if !s.Alive || s.SocketPath == "" {
			continue
		}
		if subs.IsActive(s.ID) {
			continue // subscription live — trust the SSE for the eventual exit
		}
		if _, err := os.Stat(s.SocketPath); err == nil && probeSocket(s.SocketPath) {
			continue // path exists and responds — subscription will reconnect
		}
		// Socket gone or unresponsive — mark dead.
		s.Alive = false
		s.Status = nil
		if fileMon != nil {
			if cmd := fileMon.ResolveResumeCommand(&s); cmd != nil {
				s.Command = cmd
			}
		}
		sessions.Upsert(s)
		if onDead != nil {
			onDead(s)
		}
		if subs != nil {
			subs.Unsubscribe(s.ID)
		}
		if fileMon != nil {
			fileMon.NotifySessionDied(s.ID)
		}
	}
}

// probeSocket checks if a Unix socket is still accepting connections.
// Used to distinguish stale socket files from live runners whose
// subscription dropped momentarily.
func probeSocket(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Register handles a registration request from gmux-run.
// Immediately queries the runner's /meta, adds to store, and
// subscribes to /events.
//
// Two paths, distinguished by whether the runner-reported id is
// already known to the store:
//
//   - **Re-registration** (id already in store). Resume — where the
//     daemon passed GMUX_RESUME_ID to the runner per ADR 0003 — and
//     daemon-restart-with-surviving-runner both land here. The
//     existing record is mutated in place: runtime fields (alive,
//     pid, socket, status, started/exit times, binary hash, runner
//     version, command, terminal size) take their values from the
//     fresh /meta payload, while everything else — slug,
//     created_at, attribution-derived adapter title and subtitle,
//     workspace root, remotes — carries across the seam. The
//     adapter's OnRegister hook is intentionally skipped: its
//     primary job is slug derivation, and the authoritative slug
//     for this session was decided at original registration.
//
//   - **Fresh** (id not in store). Normal new-session launch. The
//     adapter's OnRegister runs to write any per-session state file
//     and to derive the initial slug.
//
// Fast-exiting commands (echo, true) often die before queryMeta
// arrives, so the /meta payload reports alive=false. In that case
// Register is the session's only landing point in the store, and
// onDead fires after the Upsert so the record is persisted to disk.
func Register(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, socketPath string, onDead OnDeadFunc) error {
	newSess, err := queryMeta(socketPath)
	if err != nil {
		return err
	}

	if existing, ok := sessions.Get(newSess.ID); ok {
		// Re-registration. The runner reports fresh runtime state;
		// the store has the historical and attribution-derived
		// state from before the seam. Merge by overwriting only the
		// runtime-owned fields so anything the runner doesn't know
		// about (slug, created_at, FileMonitor-attributed title /
		// subtitle, workspace metadata) survives.
		existing.Alive = newSess.Alive
		existing.Pid = newSess.Pid
		existing.SocketPath = socketPath
		existing.StartedAt = newSess.StartedAt
		existing.ExitedAt = newSess.ExitedAt
		existing.ExitCode = newSess.ExitCode
		existing.Status = newSess.Status
		existing.BinaryHash = newSess.BinaryHash
		existing.RunnerVersion = newSess.RunnerVersion
		existing.Command = newSess.Command
		existing.TerminalCols = newSess.TerminalCols
		existing.TerminalRows = newSess.TerminalRows
		// Resumable is a derived attribute of dead sessions; a
		// re-registration means alive, so always clear.
		existing.Resumable = false
		*newSess = existing
		log.Printf("register: re-registered %s session %s (slug=%s)", newSess.Kind, newSess.ID, newSess.Slug)
	} else if a := adapters.FindByKind(newSess.Kind); a != nil {
		if reg, ok := a.(adapter.SessionRegistrar); ok {
			info, err := reg.OnRegister(newSess.ID, newSess.Cwd, newSess.Command)
			if err != nil {
				log.Printf("register: %s adapter OnRegister failed for %s: %v", newSess.Kind, newSess.ID, err)
			} else if info.Slug != "" {
				newSess.Slug = info.Slug
				log.Printf("register: %s registered session %s (slug=%s)", newSess.Kind, newSess.ID, info.Slug)
			}
		}
	}

	sessions.Upsert(*newSess)
	if !newSess.Alive && newSess.Peer == "" && onDead != nil {
		// /meta arrived after the runner already exited; the session
		// will never appear in any /events stream we subscribe to.
		onDead(*newSess)
	}
	if subs != nil {
		subs.Subscribe(newSess.ID, socketPath)
	}
	if fileMon != nil {
		fileMon.NotifyNewSession(newSess.ID)
	}
	return nil
}

// queryMeta connects to a runner's Unix socket and fetches GET /meta.
func queryMeta(socketPath string) (*store.Session, error) {
	resp, err := runnerRequest(context.Background(), socketPath, http.MethodGet, "/meta", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}

	var sess store.Session
	if err := json.Unmarshal(body, &sess); err != nil {
		return nil, err
	}

	// Ensure socket_path is set (runner might not include it)
	if sess.SocketPath == "" {
		sess.SocketPath = socketPath
	}

	return &sess, nil
}

// KillSession sends POST /kill to a runner's Unix socket, asking it
// to SIGTERM its child process. The runner's normal exit lifecycle
// handles the rest.
func KillSession(socketPath string) error {
	resp, err := runnerRequest(context.Background(), socketPath, http.MethodPost, "/kill", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
