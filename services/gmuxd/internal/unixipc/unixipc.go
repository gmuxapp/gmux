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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/socklease"
)

// Listen creates and binds a Unix socket at the given path.
// The socket file is created with 0600 permissions in a 0700 directory.
// An existing pathname is removed only when it is a non-socket artifact or a
// pinned socket whose connect was actively refused. A live or ambiguous owner
// is never unlinked. Daemon callers additionally hold gmuxd.lock across this
// operation, which excludes another lease-aware daemon from the probe/remove
// interval.
type Listener struct {
	*net.UnixListener
	path string
	pin  *socklease.Pin
	once sync.Once
	err  error
}

// Close closes the listener and removes its pathname only if the pathname
// still names the inode this listener bound. Go's UnixListener auto-unlink is
// disabled in Listen so all cleanup flows through this identity check.
func (l *Listener) Close() error {
	l.once.Do(func() {
		closeErr := l.UnixListener.Close()
		if l.pin.StillNamesIt() {
			if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
				l.err = fmt.Errorf("unixipc: cleanup %s: %w", l.path, err)
			}
		}
		if err := l.pin.Close(); l.err == nil && err != nil {
			l.err = err
		}
		if l.err == nil {
			l.err = closeErr
		}
	})
	return l.err
}

func Listen(sockPath string) (*Listener, error) {
	dir := filepath.Dir(sockPath)
	if err := socklease.RequireOwnedDir(dir); err != nil {
		return nil, fmt.Errorf("unixipc: preparing directory %s: %w", dir, err)
	}

	if _, err := os.Lstat(sockPath); err == nil {
		_, isSocket := socklease.StatSocket(sockPath)
		if !isSocket {
			// A regular file cannot be a listening owner. The state directory is
			// owner-only and production holds the lifetime lock here.
			if err := os.Remove(sockPath); err != nil {
				return nil, fmt.Errorf("unixipc: removing stale artifact %s: %w", sockPath, err)
			}
		} else {
			pin, pinErr := socklease.PinSocket(sockPath)
			if pinErr != nil {
				return nil, fmt.Errorf("unixipc: pinning existing socket %s: %w", sockPath, pinErr)
			}
			defer pin.Close()
			if err := socklease.ProbeRefusedPinned(pin, 250*time.Millisecond); err != nil {
				return nil, fmt.Errorf("unixipc: socket %s is still owned: %w", sockPath, err)
			}
			if !pin.StillNamesIt() {
				return nil, fmt.Errorf("unixipc: socket %s changed during stale check", sockPath)
			}
			if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("unixipc: removing stale socket %s: %w", sockPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("unixipc: inspecting socket %s: %w", sockPath, err)
	}

	addr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("unixipc: resolve %s: %w", sockPath, err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("unixipc: listen %s: %w", sockPath, err)
	}
	// The standard listener otherwise unlinks by pathname on Close, which can
	// delete a successor that rebound after this listener lost its name.
	ln.SetUnlinkOnClose(false)

	// Restrict socket permissions to owner only.
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("unixipc: chmod %s: %w", sockPath, err)
	}
	pin, err := socklease.PinSocket(sockPath)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("unixipc: pin bound socket %s: %w", sockPath, err)
	}

	return &Listener{UnixListener: ln, path: sockPath, pin: pin}, nil
}

// Client returns an http.Client that connects to a gmuxd Unix socket.
// All HTTP requests use "http://localhost/..." as the URL; the host
// is ignored because the transport dials the socket directly.
func Client(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Callers commonly create a client for one probe and discard it.
			// Do not strand that connection in the abandoned transport's idle pool.
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 2*time.Second)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// SocketState is the ownership-relevant result of a Unix connect probe.
type SocketState int

const (
	SocketDead SocketState = iota
	SocketAlive
	SocketAmbiguous
)

// ProbeSocket distinguishes proved absence from a live or ambiguous owner.
// Only ENOENT and ECONNREFUSED prove death. In particular, a connect timeout
// may be a stopped owner with a full accept backlog and is never permission to
// unlink or replace it.
func ProbeSocket(sockPath string, timeout time.Duration) SocketState {
	conn, err := net.DialTimeout("unix", sockPath, timeout)
	if err == nil {
		_ = conn.Close()
		return SocketAlive
	}
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return SocketDead
	}
	return SocketAmbiguous
}

// SocketOwned reports whether the socket is live or its state is ambiguous.
// It is deliberately conservative: callers may act destructively only when
// ProbeSocket returned SocketDead.
func SocketOwned(sockPath string) bool {
	return ProbeSocket(sockPath, 500*time.Millisecond) != SocketDead
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

// DaemonIdentity distinguishes a newly spawned daemon from an incumbent,
// including same-version restarts.
type DaemonIdentity struct {
	Version string
	PID     int
}

// HealthIdentity checks health and returns the running daemon identity.
func HealthIdentity(sockPath string) (DaemonIdentity, bool) {
	client := Client(sockPath)
	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		return DaemonIdentity{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DaemonIdentity{}, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return DaemonIdentity{}, false
	}
	var health struct {
		Data struct {
			Version string `json:"version"`
			PID     int    `json:"pid"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &health) != nil || strings.TrimSpace(health.Data.Version) == "" || health.Data.PID <= 0 {
		return DaemonIdentity{}, false
	}
	return DaemonIdentity{Version: health.Data.Version, PID: health.Data.PID}, true
}

// HealthVersion is the compatibility projection used by the pre-cutover path.
func HealthVersion(sockPath string) (string, bool) {
	id, ok := HealthIdentity(sockPath)
	return id.Version, ok
}

// Shutdown asks a running gmuxd to shut down via its Unix socket,
// then waits until connect proves the socket is gone. A deadline is failure,
// never success: a stopped or overloaded incumbent still owns its pathname.
func Shutdown(sockPath string) bool { return shutdownWithin(sockPath, 5*time.Second) }

func shutdownWithin(sockPath string, wait time.Duration) bool {
	client := Client(sockPath)
	resp, err := client.Post("http://localhost/v1/shutdown", "", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			return false
		case <-tick.C:
			if ProbeSocket(sockPath, 250*time.Millisecond) == SocketDead {
				return true
			}
		}
	}
}

// Replace shuts down any existing daemon on the socket and prepares
// for a new one to bind. Returns nil on success.
func Replace(sockPath string) error {
	if ProbeSocket(sockPath, 500*time.Millisecond) != SocketDead {
		if !Shutdown(sockPath) {
			return fmt.Errorf("existing daemon at %s did not shut down", sockPath)
		}
	}
	// Listen performs the pinned stale-path check immediately before bind.
	return nil
}
