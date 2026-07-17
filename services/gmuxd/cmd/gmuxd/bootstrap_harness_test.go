package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// harnessFleet is deliberately a transport, rather than a durable-store fake:
// these tests exercise the composed bootstrap/coordinator/store/cache graph.
type harnessFleet struct {
	mu      sync.RWMutex
	metas   map[string]sessioncoord.RunnerMeta
	streams map[string]*harnessStream
	active  atomic.Int64
	ignore  map[string]chan struct{}
}

type harnessStream struct {
	events chan sessioncoord.RunnerEvent
	once   sync.Once
}

func (s *harnessStream) Events() <-chan sessioncoord.RunnerEvent { return s.events }
func (s *harnessStream) Close() error                            { s.once.Do(func() { close(s.events) }); return nil }

func newHarnessFleet(n int) *harnessFleet {
	f := &harnessFleet{metas: make(map[string]sessioncoord.RunnerMeta), streams: make(map[string]*harnessStream), ignore: make(map[string]chan struct{})}
	for i := 0; i < n; i++ {
		ep, id := fmt.Sprintf("runner-%03d", i), centralstore.SessionID(fmt.Sprintf("sess-harness-%03d", i))
		f.metas[ep] = sessioncoord.RunnerMeta{PID: 1000 + i, Registration: centralstore.RunnerRegistration{ID: id, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1}}
		f.streams[ep] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 32)}
	}
	return f
}

func (f *harnessFleet) Subscribe(ctx context.Context, ep string) (sessioncoord.EventStream, error) {
	f.active.Add(1)
	defer f.active.Add(-1)
	f.mu.RLock()
	block, noncompliant := f.ignore[ep]
	s := f.streams[ep]
	f.mu.RUnlock()
	if noncompliant {
		<-block
	}
	if s == nil {
		return nil, fmt.Errorf("unknown endpoint %s", ep)
	}
	return s, nil
}
func (f *harnessFleet) Meta(ctx context.Context, ep string) (sessioncoord.RunnerMeta, error) {
	f.mu.RLock()
	m, ok := f.metas[ep]
	f.mu.RUnlock()
	if !ok {
		return sessioncoord.RunnerMeta{}, fmt.Errorf("unknown endpoint %s", ep)
	}
	return m, nil
}
func (f *harnessFleet) endpoints() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, 0, len(f.metas))
	for ep := range f.metas {
		out = append(out, ep)
	}
	return out
}

func openHarness(t *testing.T, dir string, fleet *harnessFleet, frames func(context.Context, wire.Frames)) (*centralstore.Store, *Bootstrap) {
	t.Helper()
	s, err := centralstore.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBootstrap(BootstrapConfig{Store: s, Runners: fleet, Control: bootstrapControl{}, Spawner: bootstrapSpawner{}, Reconciler: bootstrapReconciler{}, Converter: &wire.Converter{}, Frames: frames, Endpoints: EndpointSourceFunc(func(context.Context) ([]string, error) { return fleet.endpoints(), nil }), Clock: func() centralstore.UnixMillis { return 100 }, RunnerBudget: 30 * time.Millisecond, ConvergeDeadline: 100 * time.Millisecond, RetryInitial: time.Millisecond, RetryMaximum: 2 * time.Millisecond})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	return s, b
}

func TestHarnessGlobalDeadlineJoinsWorkersAndWithholdsReadinessForNoncompliantTransport(t *testing.T) {
	fleet := newHarnessFleet(1)
	block := make(chan struct{})
	fleet.ignore["runner-000"] = block
	store, b := openHarness(t, t.TempDir(), fleet, nil)
	defer store.Close()
	done := make(chan error, 1)
	go func() { _, err := b.Converge(context.Background()); done <- err }()
	deadline := time.After(time.Second)
	for fleet.active.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("transport was not entered")
		default:
		}
	}
	select {
	case <-b.Coordinator.Converged():
		t.Fatal("noncompliant transport reported ready")
	case <-time.After(150 * time.Millisecond):
	}
	select {
	case err := <-done:
		t.Fatalf("Converge returned without joining transport: %v", err)
	default:
	}
	close(block)
	if err := <-done; err == nil {
		t.Fatal("noncompliant transport released readiness")
	}
	if got := fleet.active.Load(); got != 0 {
		t.Fatalf("Converge returned with %d workers", got)
	}
	select {
	case <-b.Coordinator.Converged():
		t.Fatal("failure closed convergence barrier")
	default:
	}
}

func TestHarnessUnreadAcknowledgementSurvivesCloseReopen(t *testing.T) {
	dir := t.TempDir()
	fleet := newHarnessFleet(0)
	s, b := openHarness(t, dir, fleet, nil)
	exited := centralstore.UnixMillis(10)
	row, _, err := s.InsertSession(context.Background(), centralstore.NewSession{ID: "sess-unread-restart", Adapter: "shell", Command: []string{"sh"}, Remotes: map[string]string{}, Unread: true, CreatedAt: 1, ExitedAt: &exited})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Coordinator.AcknowledgeDead(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, b = openHarness(t, dir, fleet, nil)
	defer s.Close()
	got, ok, err := s.Session(context.Background(), row.ID)
	if err != nil || !ok || got.Unread {
		t.Fatalf("reopened row=%+v ok=%v err=%v", got, ok, err)
	}
	if _, err = b.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessRestartMidConvergenceSweepsOnlyMissingRunner(t *testing.T) {
	dir := t.TempDir()
	fleet := newHarnessFleet(2)
	s, first := openHarness(t, dir, fleet, nil)
	firstCtx, stopFirst := context.WithCancel(context.Background())
	if _, err := first.Converge(firstCtx); err != nil {
		t.Fatal(err)
	}
	// Stop the daemon while both durable rows are still exit-less. Installed
	// streams deliberately outlive request contexts, so releasing the daemon
	// store is the down boundary; only then does the runner disappear.
	stopFirst()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, st := range fleet.streams {
		_ = st.Close()
	}

	// Remove one endpoint only while no daemon owns the store. The second
	// daemon then dies after BeginConvergence; its in-memory window must not
	// poison the third daemon's fresh convergence.
	fleet2 := newHarnessFleet(1)
	s, second := openHarness(t, dir, fleet2, nil)
	if err := second.Coordinator.BeginConvergence(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, third := openHarness(t, dir, fleet2, nil)
	defer s.Close()
	if _, err := third.Converge(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, _, _ := s.Session(context.Background(), "sess-harness-000")
	missing, _, _ := s.Session(context.Background(), "sess-harness-001")
	if live.ExitedAt != nil || missing.ExitedAt == nil {
		t.Fatalf("live=%+v missing=%+v", live, missing)
	}
}

func TestHarnessPostOpenPreListenReopensCleanly(t *testing.T) {
	dir := t.TempDir()
	s, lock, err := bootstrapOwnership(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	s, lock, err = bootstrapOwnership(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer lock.Close()
	findings, err := s.CheckState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("integrity findings: %+v", findings)
	}
}

func TestHarnessStressRace(t *testing.T) {
	duration := 5 * time.Second
	if testing.Short() {
		duration = 300 * time.Millisecond
	}
	fleet := newHarnessFleet(100)
	ctx, cancel := context.WithCancel(context.Background())
	var frameCount atomic.Int64
	store, b := openHarness(t, t.TempDir(), fleet, func(_ context.Context, f wire.Frames) {
		if f.Sessions != nil {
			frameCount.Add(1)
		}
	})
	defer store.Close()
	b.cfg.RunnerBudget, b.cfg.ConvergeDeadline = 3*time.Second, 10*time.Second
	if _, err := b.Converge(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.StartPostConvergence(ctx, fleet.endpoints()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					f := b.Cache.Current()
					if f.Sessions == nil || f.World == nil {
						t.Error("cache lost matched pair")
						return
					}
				}
			}
		}()
	}
	rows, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		row := rows[i%len(rows)]
		unread := i%2 == 0
		res, err := store.ApplyCommonFacts(ctx, row.ID, row.Version, centralstore.CommonFactsPatch{Unread: &unread})
		if err == nil {
			row.Version = res.SessionVersion
			rows[i%len(rows)] = row
			b.Composer.Invalidate(res)
		}
		if err != nil && err != centralstore.ErrStaleVersion {
			t.Fatal(err)
		}
	}
	timer := time.NewTimer(duration)
	<-timer.C
	close(stop)
	cancel()
	wg.Wait()
	if frameCount.Load() == 0 {
		t.Fatal("composer emitted no frames")
	}
	if got := len(b.Registry.Snapshot()); got != 100 {
		t.Fatalf("registrations=%d", got)
	}
}
