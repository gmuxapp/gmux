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
// would launch for new sessions. Set by main at startup. Sessions whose
// binary_hash differs are marked stale.
var ExpectedRunnerHash string

func socketDir() string {
	if d := os.Getenv("GMUX_SOCKET_DIR"); d != "" {
		return d
	}
	return "/tmp/gmux-sessions"
}

// markStale sets sess.Stale based on whether its BinaryHash matches the expected runner hash.
func markStale(sess *store.Session) {
	if ExpectedRunnerHash == "" || sess.BinaryHash == "" {
		// Can't determine — don't mark stale (graceful degradation for old runners)
		return
	}
	sess.Stale = sess.BinaryHash != ExpectedRunnerHash
}

// Watch periodically scans for Unix sockets and queries their /meta.
// When a new session is found, it subscribes to the runner's /events SSE
// for real-time status/meta/exit updates.
func Watch(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, resumes *PendingResumes, interval time.Duration, stop <-chan struct{}) {
	// Initial scan immediately
	Scan(sessions, subs, fileMon, resumes)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			subs.UnsubscribeAll()
			return
		case <-ticker.C:
			Scan(sessions, subs, fileMon, resumes)
		}
	}
}

// Scan finds all .sock files and queries each runner's /meta endpoint.
// Reachable sockets → upsert session + subscribe to /events.
// Unreachable → remove + cleanup + unsubscribe.
func Scan(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, resumes *PendingResumes) {
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
		if err := Register(sessions, subs, fileMon, sockPath, resumes); err != nil {
			// Only remove sockets old enough to be genuinely stale.
			// A brand-new socket may not be listening yet (runner still starting).
			if info, serr := entry.Info(); serr == nil && time.Since(info.ModTime()) > 10*time.Second {
				os.Remove(sockPath)
			}
		}
	}

	// Phase 2: detect dead sessions (socket file gone or unresponsive).
	for _, s := range sessions.List() {
		if !s.Alive || s.SocketPath == "" {
			continue
		}
		if _, err := os.Stat(s.SocketPath); err != nil {
			// Socket file is gone — definitely dead.
		} else if subs.IsActive(s.ID) {
			continue // socket exists and subscription is live — healthy
		} else if probeSocket(s.SocketPath) {
			continue // socket exists and responds — subscription will reconnect
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
// Immediately queries the runner's /meta, adds to store, and subscribes to /events.
// If there's a pending resume matching this session's cwd+kind, the existing
// store entry is updated in-place rather than creating a new one.
func Register(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, socketPath string, resumes *PendingResumes) error {
	newSess, err := queryMeta(socketPath)
	if err != nil {
		return err
	}
	markStale(newSess)

	// Check if this is a resumed session.
	if resumes != nil {
		if existingID, ok := resumes.Take(newSess.Command); ok {
			if existing, ok := sessions.Get(existingID); ok {
				// Merge: keep the existing entry's ID and resume_key,
				// update with live session data.
				existing.Alive = true
				existing.SocketPath = socketPath
				existing.Pid = newSess.Pid
				existing.StartedAt = newSess.StartedAt
				existing.Status = newSess.Status
				existing.BinaryHash = newSess.BinaryHash
				existing.Stale = newSess.Stale
				sessions.Upsert(existing)
				if subs != nil {
					subs.Subscribe(existingID, socketPath)
				}
				if fileMon != nil {
					fileMon.NotifyNewSession(existingID)
				}
				// Clean up any duplicate the discovery Watch loop may have
				// created between socket creation and this Register() call.
				if newSess.ID != existingID {
					sessions.Remove(newSess.ID)
					if subs != nil {
						subs.Unsubscribe(newSess.ID)
					}
				}
				log.Printf("register: merged resumed session %s ← %s", existingID, newSess.ID)
				return nil
			}
		}
	}

	// Write shell state file so the session scanner can rediscover
	// shell sessions after a gmuxd restart.
	if newSess.Kind == "shell" {
		path, err := adapters.WriteShellStateFile(newSess.ID, newSess.Cwd, newSess.Command)
		if err != nil {
			log.Printf("register: failed to write shell state file for %s: %v", newSess.ID, err)
		} else {
			newSess.ResumeKey = adapter.Slugify(filepath.Base(newSess.Cwd))
			if newSess.ResumeKey == "" {
				newSess.ResumeKey = "shell"
			}
			log.Printf("register: wrote shell state file %s", path)
		}
	}

	sessions.Upsert(*newSess)
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
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 2*time.Second)
			},
		},
		Timeout: 3 * time.Second,
	}

	resp, err := client.Get("http://localhost/meta")
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

// KillSession sends POST /kill to a runner's Unix socket, asking it to
// SIGTERM its child process. The runner's normal exit lifecycle handles the rest.
func KillSession(socketPath string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 2*time.Second)
			},
		},
		Timeout: 3 * time.Second,
	}

	resp, err := client.Post("http://localhost/kill", "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
