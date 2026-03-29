// Package unixipc manages the gmuxd Unix socket for local IPC.
//
// The socket is the primary communication channel between the gmux CLI
// and the daemon. It replaces the old unauthenticated localhost TCP
// listener. Unlike TCP, Unix sockets cannot be forwarded by VS Code,
// Docker port mapping, or SSH tunnels. Access is enforced by filesystem
// permissions (0600 socket, 0700 directory).
package unixipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Listen creates and binds a Unix socket at the given path.
// The socket file is created with 0600 permissions in a 0700 directory.
// Any existing socket file is removed first.
func Listen(sockPath string) (net.Listener, error) {
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("unixipc: creating directory %s: %w", dir, err)
	}

	// Remove stale socket file if present.
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("unixipc: removing stale socket %s: %w", sockPath, err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("unixipc: listen %s: %w", sockPath, err)
	}

	// Restrict socket permissions to owner only.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("unixipc: chmod %s: %w", sockPath, err)
	}

	return ln, nil
}

// Cleanup removes the socket file. Call on graceful shutdown.
func Cleanup(sockPath string) {
	os.Remove(sockPath)
}

// Client returns an http.Client that connects to a gmuxd Unix socket.
// All HTTP requests use "http://localhost/..." as the URL; the host
// is ignored because the transport dials the socket directly.
func Client(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 2*time.Second)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// Healthy checks if a gmuxd is reachable and healthy at the given socket.
func Healthy(sockPath string) bool {
	client := Client(sockPath)
	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// HealthVersion checks health and returns the running daemon's version.
// Returns ("", false) if the daemon is unreachable.
func HealthVersion(sockPath string) (string, bool) {
	client := Client(sockPath)
	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	var health struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &health) != nil {
		return "", true // healthy but can't parse version
	}
	return health.Data.Version, true
}

// Shutdown asks a running gmuxd to shut down via its Unix socket,
// then waits for the socket to become unavailable.
func Shutdown(sockPath string) bool {
	client := Client(sockPath)
	resp, err := client.Post("http://localhost/v1/shutdown", "", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Wait for the socket to go away.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return true
		case <-tick.C:
			if !Healthy(sockPath) {
				return true
			}
		}
	}
}

// Replace shuts down any existing daemon on the socket and prepares
// for a new one to bind. Returns nil on success.
func Replace(sockPath string) error {
	if Healthy(sockPath) {
		if !Shutdown(sockPath) {
			return fmt.Errorf("existing daemon at %s did not shut down", sockPath)
		}
	}
	// Remove stale socket file (may exist even if daemon is gone).
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket %s: %w", sockPath, err)
	}
	return nil
}
