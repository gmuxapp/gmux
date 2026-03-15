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

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func socketDir() string {
	if d := os.Getenv("GMUX_SOCKET_DIR"); d != "" {
		return d
	}
	return "/tmp/gmux-sessions"
}

// Watch periodically scans for Unix sockets and queries their /meta.
// When a new session is found, it subscribes to the runner's /events SSE
// for real-time status/meta/exit updates.
func Watch(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor, interval time.Duration, stop <-chan struct{}) {
	// Initial scan immediately
	Scan(sessions, subs, fileMon)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			subs.UnsubscribeAll()
			return
		case <-ticker.C:
			Scan(sessions, subs, fileMon)
		}
	}
}

// Scan finds all .sock files and queries each runner's /meta endpoint.
// Reachable sockets → upsert session + subscribe to /events.
// Unreachable → remove + cleanup + unsubscribe.
func Scan(sessions *store.Store, subs *Subscriptions, fileMon *FileMonitor) {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("discovery: read dir: %v", err)
		}
		return
	}

	// Build set of sockets already tracked (from merged resumes).
	trackedSockets := make(map[string]bool)
	for _, s := range sessions.List() {
		if s.SocketPath != "" {
			trackedSockets[s.SocketPath] = true
		}
	}

	seen := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}

		sockPath := filepath.Join(dir, entry.Name())
		sessionID := strings.TrimSuffix(entry.Name(), ".sock")

		// Skip if this socket is already tracked by an existing session
		// (e.g., a merged resume that kept a different session ID).
		if trackedSockets[sockPath] {
			seen[sessionID] = true
			continue
		}

		sess, err := queryMeta(sockPath)
		if err != nil {
			log.Printf("discovery: %s unreachable: %v", sessionID, err)
			os.Remove(sockPath)
			if sess, ok := sessions.Get(sessionID); ok && sess.Alive {
				sess.Alive = false
				sessions.Upsert(sess)
			}
			if subs != nil {
				subs.Unsubscribe(sessionID)
			}
			continue
		}

		_, existed := sessions.Get(sess.ID)
		seen[sess.ID] = true
		sessions.Upsert(*sess)
		if subs != nil {
			subs.Subscribe(sess.ID, sockPath)
		}
		if !existed && fileMon != nil {
			fileMon.NotifyNewSession(sess.ID)
		}
	}

	// Mark unseen live sessions as dead (socket gone)
	// Dead sessions are kept in the store for UI visibility.
	for _, s := range sessions.List() {
		if !seen[s.ID] && s.Alive && s.SocketPath != "" {
			if _, err := os.Stat(s.SocketPath); os.IsNotExist(err) {
				s.Alive = false
				sessions.Upsert(s)
				if subs != nil {
					subs.Unsubscribe(s.ID)
				}
				if fileMon != nil {
					fileMon.NotifySessionDied(s.ID)
				}
			}
		}
	}
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

	// Check if this is a resumed session.
	if resumes != nil {
		if existingID, ok := resumes.Take(newSess.Command); ok {
			if existing, ok := sessions.Get(existingID); ok {
				// Merge: keep the existing entry's ID and resume_key,
				// update with live session data.
				existing.Alive = true
				existing.Resumable = false
				existing.SocketPath = socketPath
				existing.Pid = newSess.Pid
				existing.StartedAt = newSess.StartedAt
				existing.Status = newSess.Status
				sessions.Upsert(existing)
				if subs != nil {
					subs.Subscribe(existingID, socketPath)
				}
				if fileMon != nil {
					fileMon.NotifyNewSession(existingID)
				}
				log.Printf("register: merged resumed session %s ← %s", existingID, newSess.ID)
				return nil
			}
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
