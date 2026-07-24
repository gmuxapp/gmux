package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

type trackingProductionSpawner struct {
	inner     *productionRunnerSpawner
	finalized atomic.Int32
}

func (s *trackingProductionSpawner) Spawn(ctx context.Context, row centralstore.Session) (string, error) {
	return s.inner.Spawn(ctx, row)
}
func (s *trackingProductionSpawner) CleanupSpawn(ctx context.Context, ep string) error {
	return s.inner.CleanupSpawn(ctx, ep)
}
func (s *trackingProductionSpawner) FinalizeSpawn(ep string) {
	s.finalized.Add(1)
	s.inner.FinalizeSpawn(ep)
}

type unixFakeRunner struct {
	listener   net.Listener
	server     *http.Server
	eventsSeen chan struct{}
	metaSeen   chan struct{}
	terminated chan struct{}
	termOnce   sync.Once
	metaStatus int
	id         string
}

func startUnixFakeRunner(t *testing.T, endpoint, id string, metaStatus int) *unixFakeRunner {
	t.Helper()
	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	r := &unixFakeRunner{listener: ln, eventsSeen: make(chan struct{}), metaSeen: make(chan struct{}), terminated: make(chan struct{}), metaStatus: metaStatus, id: id}
	var eventsOnce, metaOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, req *http.Request) {
		eventsOnce.Do(func() { close(r.eventsSeen) })
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-r.eventsSeen:
		default:
			t.Error("/meta requested before /events subscription")
		}
		metaOnce.Do(func() { close(r.metaSeen) })
		if metaStatus != http.StatusOK {
			http.Error(w, "broken", metaStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(runnerMetaWire{ID: id, Adapter: "shell", Alive: true, CreatedAt: time.UnixMilli(20).UTC().Format(time.RFC3339Nano), PID: 4321, RunnerVersion: "v2", BinaryHash: "hash2", CWD: "/replacement", Command: []string{"new"}, TerminalCols: 90, TerminalRows: 33})
	})
	r.server = &http.Server{Handler: mux}
	go r.server.Serve(ln)
	t.Cleanup(func() { _ = r.server.Close() })
	return r
}
func (r *unixFakeRunner) terminate(context.Context) error {
	r.termOnce.Do(func() { close(r.terminated); _ = r.server.Close() })
	return nil
}

func insertRetainedDead(t *testing.T, st *centralstore.Store, id centralstore.SessionID) centralstore.Session {
	t.Helper()
	cols, rows := uint16(120), uint16(41)
	exited := centralstore.UnixMillis(10)
	row, _, err := st.InsertSession(context.Background(), centralstore.NewSession{ID: id, Adapter: "shell", ConversationRef: "conversation", CWD: "/old", Command: []string{"old"}, CreatedAt: 1, ExitedAt: &exited, TerminalCols: &cols, TerminalRows: &rows})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestProductionSpawnerResumeComposedWithRealUnixRunner(t *testing.T) {
	// The spawner derives runner socket endpoints from paths.SessionSocketDir();
	// pin it to a temp dir so the test doesn't depend on the host's state dir
	// existing (fresh CI runners have no ~/.local/state/gmux/run/sessions).
	// t.TempDir() embeds the test name and can push socket paths past the
	// 108-byte sun_path limit, so use a short mkdtemp instead.
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row := insertRetainedDead(t, st, "sess-resume-composed")
	var got runnerLaunchRequest
	var child *unixFakeRunner
	prod := &productionRunnerSpawner{ResolveDir: func(centralstore.Session) (string, error) { return "/chosen", nil }, ResolveCommand: func(centralstore.Session) []string { return []string{"shell", "--resume", "conversation"} }}
	prod.Launch = func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
		got = req
		child = startUnixFakeRunner(t, req.Endpoint, string(row.ID), http.StatusOK)
		return runnerLaunchResult{Endpoint: req.Endpoint, PID: 4321, Terminate: child.terminate}, nil
	}
	spawner := &trackingProductionSpawner{inner: prod}
	coord := sessioncoord.New(nil, productionRunnerClient{}, st, nil, nil, sessioncoord.WithRunnerSpawner(spawner))
	rt, err := coord.Resume(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.metaSeen:
	default:
		t.Fatal("registration did not request metadata")
	}
	if rt.SessionID != row.ID || rt.Endpoint != got.Endpoint || rt.PID != 4321 {
		t.Fatalf("replacement runtime=%+v", rt)
	}
	if got.ResumeID != string(row.ID) || got.CWD != "/chosen" || got.InitialCols != 120 || got.Rows != 41 || !reflect.DeepEqual(got.Command, []string{"shell", "--resume", "conversation"}) {
		t.Fatalf("launch request=%+v", got)
	}
	if spawner.finalized.Load() != 1 {
		t.Fatalf("FinalizeSpawn calls=%d", spawner.finalized.Load())
	}
	prod.mu.Lock()
	retained := len(prod.launched)
	prod.mu.Unlock()
	if retained != 0 {
		t.Fatalf("launched handles=%d", retained)
	}
	select {
	case <-child.terminated:
		t.Fatal("successful child was terminated")
	default:
	}
	resp, err := runnerRequestContext(ctx, got.Endpoint, http.MethodGet, "/meta")
	if err != nil {
		t.Fatalf("child not alive: %v", err)
	}
	resp.Body.Close()
}

func TestProductionSpawnerWaitsForRunnerSocketBeforeRegistration(t *testing.T) {
	// The spawner derives runner endpoints from the configured socket dir;
	// fresh CI runners do not have the real state directory yet.
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row := insertRetainedDead(t, st, "sess-delayed-runner")

	childReady := make(chan *unixFakeRunner, 1)
	prod := &productionRunnerSpawner{
		ResolveDir:     func(centralstore.Session) (string, error) { return t.TempDir(), nil },
		ResolveCommand: func(centralstore.Session) []string { return []string{"shell"} },
		ReadyTimeout:   time.Second,
	}
	prod.Launch = func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
		processWait := make(chan error)
		go func() {
			time.Sleep(75 * time.Millisecond)
			childReady <- startUnixFakeRunner(t, req.Endpoint, string(row.ID), http.StatusOK)
		}()
		return runnerLaunchResult{
			Endpoint: req.Endpoint,
			PID:      4321,
			Wait:     processWait,
			Terminate: func(ctx context.Context) error {
				select {
				case child := <-childReady:
					return child.terminate(ctx)
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}, nil
	}

	coord := sessioncoord.New(nil, productionRunnerClient{}, st, nil, nil, sessioncoord.WithRunnerSpawner(prod))
	started := time.Now()
	rt, err := coord.Resume(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 75*time.Millisecond {
		t.Fatalf("resume returned before delayed socket was ready: %s", elapsed)
	}
	if rt.SessionID != row.ID {
		t.Fatalf("replacement runtime=%+v", rt)
	}
}

func TestProductionSpawnerResumeRegistrationFailureCleansAndReleasesClaim(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row := insertRetainedDead(t, st, "sess-resume-failure")
	var launches atomic.Int32
	terminated := make(chan struct{}, 2)
	prod := &productionRunnerSpawner{ResolveDir: func(centralstore.Session) (string, error) { return t.TempDir(), nil }, ResolveCommand: func(centralstore.Session) []string { return []string{"x"} }}
	prod.Launch = func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
		n := launches.Add(1)
		ep := req.Endpoint + time.Now().Format(".150405.000000000")
		child := startUnixFakeRunner(t, ep, string(row.ID), http.StatusInternalServerError)
		return runnerLaunchResult{Endpoint: ep, Terminate: func(ctx context.Context) error { err := child.terminate(ctx); terminated <- struct{}{}; return err }, PID: int(n)}, nil
	}
	spawner := &trackingProductionSpawner{inner: prod}
	coord := sessioncoord.New(nil, productionRunnerClient{}, st, nil, nil, sessioncoord.WithRunnerSpawner(spawner))
	for i := 0; i < 2; i++ {
		if _, err := coord.Resume(ctx, row.ID); err == nil {
			t.Fatal("meta failure accepted")
		}
		select {
		case <-terminated:
		case <-time.After(time.Second):
			t.Fatal("failed child not terminated")
		}
	}
	if launches.Load() != 2 {
		t.Fatalf("retry launches=%d", launches.Load())
	}
	if spawner.finalized.Load() != 0 {
		t.Fatalf("failed spawn finalized")
	}
	prod.mu.Lock()
	retained := len(prod.launched)
	prod.mu.Unlock()
	if retained != 0 {
		t.Fatalf("cleanup handles=%d", retained)
	}
	if _, ok, err := st.Session(ctx, row.ID); err != nil || !ok {
		t.Fatalf("retained row lost: ok=%v err=%v", ok, err)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("unexpected cancellation")
	}
}

// shortSocketDir returns a temp dir short enough for Unix socket paths.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
