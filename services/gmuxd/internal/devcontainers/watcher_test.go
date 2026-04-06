package devcontainers

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// fakeDocker implements dockerRunner for testing. Mutations must go
// through setContainers/setTokens to stay thread-safe with the watcher
// goroutine.
type fakeDocker struct {
	mu         sync.Mutex
	containers []container
	tokens     map[string]string // containerID → token
	eventCh    chan struct{}
}

func (f *fakeDocker) list(_ context.Context) ([]container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]container{}, f.containers...), nil
}

func (f *fakeDocker) readToken(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tok, ok := f.tokens[id]; ok {
		return tok, nil
	}
	return "", context.DeadlineExceeded
}

func (f *fakeDocker) events(_ context.Context) (<-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eventCh == nil {
		f.eventCh = make(chan struct{})
	}
	return f.eventCh, nil
}

func (f *fakeDocker) setContainers(cs []container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = cs
}

func (f *fakeDocker) setTokens(t map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = t
}

// callbackDocker lets tests provide a custom readToken function.
type callbackDocker struct {
	containers []container
	onRead     func(ctx context.Context, id string) (string, error)
}

func (c *callbackDocker) list(_ context.Context) ([]container, error) {
	return c.containers, nil
}

func (c *callbackDocker) readToken(ctx context.Context, id string) (string, error) {
	return c.onRead(ctx, id)
}

func (c *callbackDocker) events(_ context.Context) (<-chan struct{}, error) {
	return make(chan struct{}), nil
}

func setupManager(t *testing.T) *peering.Manager {
	t.Helper()
	mgr := peering.NewManager(nil, store.New(), "test-host")
	mgr.Start()
	t.Cleanup(mgr.Stop)
	return mgr
}

// newTestWatcher returns a watcher with fast retry timings suitable
// for tests. Production uses longer delays (see tokenRetryDelay).
func newTestWatcher(mgr *peering.Manager, docker dockerRunner) *Watcher {
	w := newWatcher(mgr, docker)
	w.tokenRetryDelay = 5 * time.Millisecond
	return w
}

func gmuxContainer(id, name, ip, folder string) container {
	labels := map[string]string{}
	if folder != "" {
		labels["devcontainer.local_folder"] = folder
	}
	return container{
		ID:     id,
		Name:   name,
		Env:    []string{"PATH=/usr/bin", "GMUXD_LISTEN=0.0.0.0"},
		Labels: labels,
		IP:     ip,
	}
}

func TestScan_DiscoversNewContainer(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("abc123full", "my-devcontainer", "172.17.0.2", "/home/user/dev/myproject"),
		},
		tokens: map[string]string{"abc123full": "secret-token"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1", w.Tracked())
	}
	peer := mgr.GetPeer("myproject")
	if peer == nil {
		t.Fatal("expected peer 'myproject' to be registered")
	}
	if peer.Config.URL != "http://172.17.0.2:8790" {
		t.Errorf("URL = %q, want %q", peer.Config.URL, "http://172.17.0.2:8790")
	}
	if peer.Config.Token != "secret-token" {
		t.Errorf("token = %q, want %q", peer.Config.Token, "secret-token")
	}
}

func TestScan_RemovesStalePeer(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("abc123", "dev", "172.17.0.2", "/home/user/dev/myproject"),
		},
		tokens: map[string]string{"abc123": "tok"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1 after first scan", w.Tracked())
	}

	// Container stops.
	fd.containers = nil
	w.scan(context.Background())

	if w.Tracked() != 0 {
		t.Fatalf("tracked = %d, want 0 after removal", w.Tracked())
	}
	if mgr.GetPeer("myproject") != nil {
		t.Fatal("peer should be removed after container stops")
	}
}

func TestScan_SkipsContainerWithoutGmuxdEnv(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{{
			ID:   "plain-container",
			Name: "redis",
			Env:  []string{"REDIS_PORT=6379"},
			IP:   "172.17.0.3",
		}},
		tokens: map[string]string{},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 0 {
		t.Fatalf("tracked = %d, want 0 (non-gmux container)", w.Tracked())
	}
}

func TestScan_SkipsContainerWithoutToken(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("starting-up", "dev", "172.17.0.2", "/home/user/dev/proj"),
		},
		tokens: map[string]string{}, // no token yet
	}

	w := newTestWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 0 {
		t.Fatalf("tracked = %d, want 0 (token not ready)", w.Tracked())
	}

	// Token becomes available on next scan.
	fd.tokens["starting-up"] = "now-ready"
	w.scan(context.Background())

	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1 after retry", w.Tracked())
	}
}

func TestScan_RetriesReadTokenUntilReady(t *testing.T) {
	mgr := setupManager(t)

	// Token appears only on the 2nd readToken call.
	var callCount int
	var callMu sync.Mutex
	fd := &callbackDocker{
		containers: []container{
			gmuxContainer("slow", "dev", "172.17.0.2", "/home/user/dev/proj"),
		},
		onRead: func(_ context.Context, id string) (string, error) {
			callMu.Lock()
			defer callMu.Unlock()
			callCount++
			if callCount < 2 {
				return "", context.DeadlineExceeded
			}
			return "tok", nil
		},
	}

	w := newTestWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1 (should succeed on retry)", w.Tracked())
	}
	if callCount != 2 {
		t.Errorf("readToken calls = %d, want 2 (1 fail + 1 success)", callCount)
	}
}

func TestScan_ParallelReadTokens(t *testing.T) {
	// Three candidate containers: one that always succeeds, one that
	// always fails (broken), and one that succeeds after a delay.
	// Parallel reads should complete without the broken container
	// blocking the others.
	mgr := setupManager(t)

	var slowCalls atomic.Int32
	fd := &callbackDocker{
		containers: []container{
			gmuxContainer("fast", "fast-dev", "172.17.0.2", "/a"),
			gmuxContainer("broken", "broken-dev", "172.17.0.3", "/b"),
			gmuxContainer("slow", "slow-dev", "172.17.0.4", "/c"),
		},
		onRead: func(_ context.Context, id string) (string, error) {
			switch id {
			case "fast":
				return "tok-fast", nil
			case "broken":
				return "", context.DeadlineExceeded
			case "slow":
				if slowCalls.Add(1) < 2 {
					return "", context.DeadlineExceeded
				}
				return "tok-slow", nil
			}
			return "", context.DeadlineExceeded
		},
	}

	w := newTestWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 2 {
		t.Fatalf("tracked = %d, want 2 (fast + slow, not broken)", w.Tracked())
	}
	if mgr.GetPeer("a") == nil {
		t.Error("fast container should be registered (peer 'a')")
	}
	if mgr.GetPeer("c") == nil {
		t.Error("slow container should be registered after retry (peer 'c')")
	}
	if mgr.GetPeer("b") != nil {
		t.Error("broken container should not be registered")
	}
}

func TestScan_RetryReadTokenRespectsContextCancel(t *testing.T) {
	mgr := setupManager(t)

	fd := &callbackDocker{
		containers: []container{
			gmuxContainer("stuck", "dev", "172.17.0.2", "/home/user/dev/proj"),
		},
		onRead: func(_ context.Context, _ string) (string, error) {
			return "", context.DeadlineExceeded
		},
	}

	w := newTestWatcher(mgr, fd)
	w.tokenRetryDelay = 10 * time.Second // would block for >20s if not cancelled

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.scan(ctx)
	}()

	// Cancel while scan is sleeping between retries.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("scan did not return after context cancel")
	}
}

func TestScan_SkipsContainerWithoutIP(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{{
			ID:   "no-network",
			Name: "dev",
			Env:  []string{"GMUXD_LISTEN=0.0.0.0"},
			IP:   "", // no IP
		}},
		tokens: map[string]string{"no-network": "tok"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 0 {
		t.Fatalf("tracked = %d, want 0 (no IP)", w.Tracked())
	}
}

func TestScan_IdempotentForTrackedContainers(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("abc", "dev", "172.17.0.2", "/home/user/proj"),
		},
		tokens: map[string]string{"abc": "tok"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())
	w.scan(context.Background())
	w.scan(context.Background())

	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1 (idempotent)", w.Tracked())
	}
}

func TestScan_UniqueNamesForDuplicateProjects(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("aaa", "dev1", "172.17.0.2", "/home/user/work/myapp"),
			gmuxContainer("bbb", "dev2", "172.17.0.3", "/home/other/myapp"),
		},
		tokens: map[string]string{"aaa": "tok1", "bbb": "tok2"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	if w.Tracked() != 2 {
		t.Fatalf("tracked = %d, want 2", w.Tracked())
	}
	// One should be "myapp", the other "myapp-2".
	p1 := mgr.GetPeer("myapp")
	p2 := mgr.GetPeer("myapp-2")
	if p1 == nil || p2 == nil {
		t.Fatalf("expected peers 'myapp' and 'myapp-2', got p1=%v p2=%v", p1, p2)
	}
}

func TestScan_DoesNotConflictWithManualPeer(t *testing.T) {
	st := store.New()
	mgr := peering.NewManager([]config.PeerConfig{
		{Name: "server", URL: "http://10.0.0.5:8790", Token: "manual-tok"},
	}, st, "test-host")
	mgr.Start()
	t.Cleanup(mgr.Stop)

	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("ccc", "dev", "172.17.0.2", "/home/user/server"),
		},
		tokens: map[string]string{"ccc": "tok"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	// Should get "server-2" since "server" is taken by the manual peer.
	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1", w.Tracked())
	}
	if mgr.GetPeer("server-2") == nil {
		t.Fatal("expected 'server-2' to avoid conflict with manual 'server'")
	}
}

func TestScan_ContainerRestart(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{
			gmuxContainer("old-id", "dev", "172.17.0.2", "/home/user/proj"),
		},
		tokens: map[string]string{"old-id": "tok1"},
	}

	w := newWatcher(mgr, fd)
	w.scan(context.Background())

	if mgr.GetPeer("proj") == nil {
		t.Fatal("expected peer 'proj' after first scan")
	}

	// Container restarts with a new ID and possibly new IP/token.
	fd.containers = []container{
		gmuxContainer("new-id", "dev", "172.17.0.3", "/home/user/proj"),
	}
	fd.tokens = map[string]string{"new-id": "tok2"}
	w.scan(context.Background())

	if w.Tracked() != 1 {
		t.Fatalf("tracked = %d, want 1", w.Tracked())
	}
	peer := mgr.GetPeer("proj")
	if peer == nil {
		t.Fatal("expected peer 'proj' after restart")
	}
	if peer.Config.Token != "tok2" {
		t.Errorf("token = %q, want %q (should be from new container)", peer.Config.Token, "tok2")
	}
}

func TestDeriveName_FolderLabel(t *testing.T) {
	c := container{
		ID:     "abc",
		Name:   "boring-name",
		Labels: map[string]string{"devcontainer.local_folder": "/home/user/dev/My Cool Project"},
	}
	got := deriveName(c)
	if got != "my-cool-project" {
		t.Errorf("deriveName = %q, want %q", got, "my-cool-project")
	}
}

func TestDeriveName_ContainerName(t *testing.T) {
	c := container{ID: "abc", Name: "focused_einstein"}
	got := deriveName(c)
	if got != "focused-einstein" {
		t.Errorf("deriveName = %q, want %q", got, "focused-einstein")
	}
}

func TestDeriveName_FallbackToID(t *testing.T) {
	c := container{ID: "abc123def456", Name: ""}
	got := deriveName(c)
	if got != "abc123def456" {
		t.Errorf("deriveName = %q, want %q", got, "abc123def456")
	}
}

func TestEvents_TriggerScan(t *testing.T) {
	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: nil, // no containers initially
		tokens:     map[string]string{},
		eventCh:    make(chan struct{}, 1),
	}

	w := newWatcher(mgr, fd)

	// Start the watcher loop in the background.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx)
	}()

	// Initial scan should find nothing.
	deadline := time.After(500 * time.Millisecond)
	for w.Tracked() != 0 {
		select {
		case <-deadline:
			t.Fatal("unexpected containers tracked before event")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Add a container and fire an event.
	fd.setContainers([]container{
		gmuxContainer("abc", "dev", "172.17.0.2", "/home/u/proj"),
	})
	fd.setTokens(map[string]string{"abc": "tok"})
	fd.eventCh <- struct{}{}

	// Wait for the scan to pick up the container.
	deadline = time.After(2 * time.Second)
	for w.Tracked() != 1 {
		select {
		case <-deadline:
			t.Fatalf("container not discovered via event: tracked=%d", w.Tracked())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestEvents_RescanOnReconnect(t *testing.T) {
	mgr := setupManager(t)

	// Channel we can close to simulate stream ending.
	eventCh := make(chan struct{})
	fd := &reconnectingDocker{
		streams: []chan struct{}{eventCh},
		tokens:  map[string]string{"abc": "tok"},
	}

	w := newWatcher(mgr, fd)
	w.reconnectIn = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx)
	}()

	// Wait for first scan (finds nothing).
	time.Sleep(50 * time.Millisecond)
	if w.Tracked() != 0 {
		t.Fatal("unexpected containers at start")
	}

	// Simulate a container starting while the events stream is alive,
	// but AFTER the initial scan (so the first scan missed it). Then
	// close the stream to force a reconnect with a rescan.
	fd.mu.Lock()
	fd.containers = []container{gmuxContainer("abc", "dev", "172.17.0.2", "/home/u/proj")}
	fd.mu.Unlock()

	// Close the stream without sending any event. The reconnect path
	// must still rescan to discover the container.
	close(eventCh)

	// Provide a new stream for the reconnection attempt.
	nextStream := make(chan struct{})
	fd.mu.Lock()
	fd.streams = append(fd.streams, nextStream)
	fd.mu.Unlock()

	// Wait for reconnect + rescan.
	deadline := time.After(2 * time.Second)
	for w.Tracked() != 1 {
		select {
		case <-deadline:
			t.Fatalf("container not discovered after reconnect: tracked=%d", w.Tracked())
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// reconnectingDocker hands out a new event channel each time events()
// is called, simulating stream reconnection.
type reconnectingDocker struct {
	mu         sync.Mutex
	containers []container
	tokens     map[string]string
	streams    []chan struct{}
	callCount  int
}

func (r *reconnectingDocker) list(_ context.Context) ([]container, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]container{}, r.containers...), nil
}

func (r *reconnectingDocker) readToken(_ context.Context, id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tok, ok := r.tokens[id]; ok {
		return tok, nil
	}
	return "", context.DeadlineExceeded
}

func (r *reconnectingDocker) events(_ context.Context) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.callCount >= len(r.streams) {
		// Return a never-closing channel so the loop parks.
		return make(chan struct{}), nil
	}
	ch := r.streams[r.callCount]
	r.callCount++
	return ch, nil
}

func TestWaitForSocket_AppearsLater(t *testing.T) {
	dir := t.TempDir()
	sockPath := dir + "/docker.sock"

	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{gmuxContainer("abc", "dev", "172.17.0.2", "/home/u/proj")},
		tokens:     map[string]string{"abc": "tok"},
		eventCh:    make(chan struct{}),
	}
	w := newWatcher(mgr, fd)
	w.socketPath = sockPath

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx)
	}()

	// Watcher should be blocked waiting for the socket.
	time.Sleep(50 * time.Millisecond)
	if w.Tracked() != 0 {
		t.Fatal("scan should not run before socket exists")
	}

	// Create the socket file. fsnotify should fire a CREATE event.
	f, err := os.Create(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Watcher should now proceed and discover the container.
	deadline := time.After(2 * time.Second)
	for w.Tracked() != 1 {
		select {
		case <-deadline:
			t.Fatalf("scan did not run after socket appeared: tracked=%d", w.Tracked())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestWaitForSocket_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	sockPath := dir + "/docker.sock"
	f, err := os.Create(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	mgr := setupManager(t)
	fd := &fakeDocker{
		containers: []container{gmuxContainer("abc", "dev", "172.17.0.2", "/home/u/proj")},
		tokens:     map[string]string{"abc": "tok"},
		eventCh:    make(chan struct{}),
	}
	w := newWatcher(mgr, fd)
	w.socketPath = sockPath

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx)
	}()

	// Should proceed immediately since socket exists.
	deadline := time.After(1 * time.Second)
	for w.Tracked() != 1 {
		select {
		case <-deadline:
			t.Fatalf("scan did not run immediately: tracked=%d", w.Tracked())
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestDockerSocketPath(t *testing.T) {
	tests := []struct {
		name       string
		dockerHost string
		want       string
	}{
		{"unset", "", "/var/run/docker.sock"},
		{"unix", "unix:///run/user/1000/docker.sock", "/run/user/1000/docker.sock"},
		{"tcp empty", "tcp://remote:2375", ""},
		{"ssh empty", "ssh://user@host", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tt.dockerHost)
			if got := dockerSocketPath(); got != tt.want {
				t.Errorf("dockerSocketPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsGmuxContainer(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want bool
	}{
		{"with GMUXD_LISTEN", []string{"PATH=/bin", "GMUXD_LISTEN=0.0.0.0"}, true},
		{"without GMUXD_LISTEN", []string{"PATH=/bin", "REDIS_PORT=6379"}, false},
		{"empty env", nil, false},
		{"partial match", []string{"GMUXD_LISTEN_OTHER=x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := container{Env: tt.env}
			if got := isGmuxContainer(c); got != tt.want {
				t.Errorf("isGmuxContainer = %v, want %v", got, tt.want)
			}
		})
	}
}
