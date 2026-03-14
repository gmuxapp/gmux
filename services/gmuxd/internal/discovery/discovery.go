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
func Watch(sessions *store.Store, subs *Subscriptions, interval time.Duration, stop <-chan struct{}) {
	// Initial scan immediately
	Scan(sessions, subs)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			subs.UnsubscribeAll()
			return
		case <-ticker.C:
			Scan(sessions, subs)
		}
	}
}

// Scan finds all .sock files and queries each runner's /meta endpoint.
// Reachable sockets → upsert session + subscribe to /events.
// Unreachable → remove + cleanup + unsubscribe.
func Scan(sessions *store.Store, subs *Subscriptions) {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("discovery: read dir: %v", err)
		}
		return
	}

	seen := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}

		sockPath := filepath.Join(dir, entry.Name())
		sessionID := strings.TrimSuffix(entry.Name(), ".sock")

		sess, err := queryMeta(sockPath)
		if err != nil {
			// Socket unreachable — stale, clean up
			log.Printf("discovery: %s unreachable, removing: %v", sessionID, err)
			os.Remove(sockPath)
			sessions.Remove(sessionID)
			continue
		}

		seen[sess.ID] = true
		sessions.Upsert(*sess)
		// Subscribe to runner /events for real-time updates
		if subs != nil {
			subs.Subscribe(sess.ID, sockPath)
		}
	}

	// Remove sessions whose sockets no longer exist
	for _, s := range sessions.List() {
		if !seen[s.ID] && s.SocketPath != "" {
			// Check if socket file still exists
			if _, err := os.Stat(s.SocketPath); os.IsNotExist(err) {
				sessions.Remove(s.ID)
				if subs != nil {
					subs.Unsubscribe(s.ID)
				}
			}
		}
	}
}

// Register handles a registration request from gmux-run.
// Immediately queries the runner's /meta, adds to store, and subscribes to /events.
func Register(sessions *store.Store, subs *Subscriptions, socketPath string) error {
	sess, err := queryMeta(socketPath)
	if err != nil {
		return err
	}
	sessions.Upsert(*sess)
	if subs != nil {
		subs.Subscribe(sess.ID, socketPath)
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
