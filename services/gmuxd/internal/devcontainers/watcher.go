// Package devcontainers discovers gmuxd instances running in dev containers
// on the local Docker daemon and registers them as peers.
//
// Detection: containers with GMUXD_LISTEN in their environment (set by the
// gmux devcontainer feature) are candidates. The watcher reads each
// container's auth token via docker exec and registers a peer connection.
//
// The watcher subscribes to Docker's event stream and reconciles tracked
// containers on every container start/die event. It also runs a full
// reconcile on each (re)connection to the event stream so that no state
// is lost across disconnects.
package devcontainers

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
)

const (
	// eventsRetry is the delay before reconnecting to the events stream
	// after a failure. A scan runs on every (re)connection, so this only
	// affects how quickly we catch up after a disconnect.
	eventsRetry = 5 * time.Second
	gmuxdPort   = 8790
)

// Watcher subscribes to Docker events, reconciles dev containers
// running gmuxd, and manages their lifecycle as peers in the peering
// manager.
type Watcher struct {
	mgr         *peering.Manager
	docker      dockerRunner
	reconnectIn time.Duration
	socketPath  string // empty disables socket-wait

	mu      sync.Mutex
	tracked map[string]string // containerID → peerName

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWatcher creates a watcher if the docker CLI is installed. Returns
// nil otherwise. If the Docker socket isn't yet available, the watcher
// will wait for it to appear (via fsnotify) before scanning containers.
func NewWatcher(mgr *peering.Manager) *Watcher {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	return &Watcher{
		mgr:         mgr,
		docker:      &cliDocker{},
		reconnectIn: eventsRetry,
		tracked:     make(map[string]string),
		socketPath:  dockerSocketPath(),
	}
}

// newWatcher is the internal constructor for testing with a fake docker.
func newWatcher(mgr *peering.Manager, docker dockerRunner) *Watcher {
	return &Watcher{
		mgr:         mgr,
		docker:      docker,
		reconnectIn: eventsRetry,
		tracked:     make(map[string]string),
	}
}

// Start begins the background discovery loop.
func (w *Watcher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx)
	}()
}

// Stop cancels the discovery loop and waits for it to finish.
func (w *Watcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func (w *Watcher) run(ctx context.Context) {
	if !w.waitForSocket(ctx) {
		return
	}
	for ctx.Err() == nil {
		// Scan on every (re)connection to catch containers that started
		// or stopped while we were disconnected (initial state, daemon
		// restart, stream reconnection).
		w.scan(ctx)

		w.subscribe(ctx)

		// Stream ended or failed to start; back off before reconnecting.
		// Returns immediately if ctx is already cancelled.
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.reconnectIn):
		}
	}
}

// subscribe opens the events stream and scans on each event until the
// stream ends or the context is cancelled.
func (w *Watcher) subscribe(ctx context.Context) {
	events, err := w.docker.events(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("devcontainers: events: %v", err)
		}
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			w.scan(ctx)
		}
	}
}

// scan reconciles tracked containers against Docker's current state.
func (w *Watcher) scan(ctx context.Context) {
	containers, err := w.docker.list(ctx)
	if err != nil {
		log.Printf("devcontainers: scan: %v", err)
		return
	}

	// Index gmux containers by ID.
	gmux := make(map[string]container)
	for _, c := range containers {
		if isGmuxContainer(c) {
			gmux[c.ID] = c
		}
	}

	// Collect removals under the lock, then execute outside (RemovePeer
	// blocks until the peer goroutine finishes, which can take seconds).
	w.mu.Lock()
	var removals []struct{ id, name string }
	for id, name := range w.tracked {
		if _, ok := gmux[id]; !ok {
			removals = append(removals, struct{ id, name string }{id, name})
			delete(w.tracked, id)
		}
	}
	w.mu.Unlock()

	for _, r := range removals {
		log.Printf("devcontainers: container %s stopped, removing peer %s", short(r.id), r.name)
		w.mgr.RemovePeer(r.name)
	}

	// Add peers for newly discovered containers.
	w.mu.Lock()
	defer w.mu.Unlock()

	for id, c := range gmux {
		if _, ok := w.tracked[id]; ok {
			continue // already tracked
		}
		if c.IP == "" {
			continue // no reachable IP
		}

		token, err := w.docker.readToken(ctx, id)
		if err != nil {
			// gmuxd not ready yet; will retry on next scan.
			continue
		}

		name := w.uniqueName(deriveName(c))
		url := fmt.Sprintf("http://%s:%d", c.IP, gmuxdPort)

		cfg := config.PeerConfig{
			Name:  name,
			URL:   url,
			Token: token,
		}

		log.Printf("devcontainers: discovered %s (container %s, %s)", name, short(id), url)
		w.mgr.AddPeer(cfg)
		w.tracked[id] = name
	}
}

// Tracked returns the number of tracked containers (for testing).
func (w *Watcher) Tracked() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.tracked)
}

// --- Helpers ---

// waitForSocket blocks until the Docker socket exists or ctx is cancelled.
// Returns true if the socket is available, false if ctx was cancelled or
// the socket cannot be watched. A zero socketPath skips the check
// (used by tests with an in-memory dockerRunner).
func (w *Watcher) waitForSocket(ctx context.Context) bool {
	if w.socketPath == "" {
		return true
	}
	if _, err := os.Stat(w.socketPath); err == nil {
		return true
	}

	parent := filepath.Dir(w.socketPath)
	if _, err := os.Stat(parent); err != nil {
		log.Printf("devcontainers: %s parent %q missing, discovery disabled", w.socketPath, parent)
		<-ctx.Done()
		return false
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("devcontainers: fsnotify: %v, discovery disabled", err)
		<-ctx.Done()
		return false
	}
	defer fsw.Close()

	if err := fsw.Add(parent); err != nil {
		log.Printf("devcontainers: cannot watch %q: %v, discovery disabled", parent, err)
		<-ctx.Done()
		return false
	}

	// Re-check after adding the watch to avoid a race where the socket
	// appears between the initial Stat and the Add.
	if _, err := os.Stat(w.socketPath); err == nil {
		return true
	}

	log.Printf("devcontainers: waiting for Docker socket at %s", w.socketPath)
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-fsw.Events:
			if !ok {
				return false
			}
			if ev.Name == w.socketPath && ev.Op&fsnotify.Create != 0 {
				log.Printf("devcontainers: Docker socket appeared")
				return true
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return false
			}
			log.Printf("devcontainers: fsnotify: %v", err)
			return false
		}
	}
}

// dockerSocketPath returns the path to the Docker daemon socket,
// honoring DOCKER_HOST for unix sockets. Returns empty for non-unix
// DOCKER_HOST values (TCP, SSH) since socket-watching doesn't apply.
func dockerSocketPath() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		if strings.HasPrefix(host, "unix://") {
			return strings.TrimPrefix(host, "unix://")
		}
		return "" // TCP/SSH — can't watch, proceed immediately
	}
	return "/var/run/docker.sock"
}

// isGmuxContainer returns true if a container is running gmuxd via the
// devcontainer feature. Detection: GMUXD_LISTEN env var is set by the
// feature's containerEnv.
func isGmuxContainer(c container) bool {
	for _, e := range c.Env {
		if strings.HasPrefix(e, "GMUXD_LISTEN=") {
			return true
		}
	}
	return false
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// deriveName produces a peer name from container metadata.
// Priority: devcontainer.local_folder label basename, then container name.
func deriveName(c container) string {
	if folder, ok := c.Labels["devcontainer.local_folder"]; ok {
		base := filepath.Base(folder)
		if name := slugify(base); name != "" {
			return name
		}
	}
	if name := slugify(c.Name); name != "" {
		return name
	}
	return short(c.ID)
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// uniqueName appends a container ID prefix if the base name is already taken.
func (w *Watcher) uniqueName(base string) string {
	if !w.nameInUse(base) {
		return base
	}
	// Try suffixed names.
	for i := 2; i < 100; i++ {
		name := fmt.Sprintf("%s-%d", base, i)
		if !w.nameInUse(name) {
			return name
		}
	}
	return base // give up, will be caught by AddPeer's no-op on duplicate
}

// nameInUse checks tracked names and existing peers. Must hold w.mu.
func (w *Watcher) nameInUse(name string) bool {
	for _, tracked := range w.tracked {
		if tracked == name {
			return true
		}
	}
	return w.mgr.GetPeer(name) != nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
