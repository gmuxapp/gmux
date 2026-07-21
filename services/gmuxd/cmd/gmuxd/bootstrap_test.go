package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
)

type bootstrapStream struct{ events chan sessioncoord.RunnerEvent }

func (s *bootstrapStream) Events() <-chan sessioncoord.RunnerEvent { return s.events }
func (s *bootstrapStream) Close() error                            { return nil }

type bootstrapRunners struct {
	mu      sync.Mutex
	metas   map[string]sessioncoord.RunnerMeta
	blocked map[string]bool
}

func (r *bootstrapRunners) Subscribe(ctx context.Context, ep string) (sessioncoord.EventStream, error) {
	if r.blocked[ep] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &bootstrapStream{events: make(chan sessioncoord.RunnerEvent)}, nil
}
func (r *bootstrapRunners) Meta(ctx context.Context, ep string) (sessioncoord.RunnerMeta, error) {
	if r.blocked[ep] {
		<-ctx.Done()
		return sessioncoord.RunnerMeta{}, ctx.Err()
	}
	m, ok := r.metas[ep]
	if !ok {
		return sessioncoord.RunnerMeta{}, errors.New("missing")
	}
	return m, nil
}

type bootstrapReconciler struct{}

func (bootstrapReconciler) ReconcileRetained(context.Context, string, []sessioncoord.ReconcileCandidate) ([]sessioncoord.ReconcileDecision, error) {
	return nil, nil
}

type bootstrapControl struct{}

func (bootstrapControl) Terminate(context.Context, string) error { return nil }

type bootstrapSpawner struct{}

func (bootstrapSpawner) Spawn(context.Context, centralstore.Session) (string, error) { return "", nil }

func TestBootstrapOwnershipVerifiesBeforeTakeover(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, centralstore.DatabaseName), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, _, err := bootstrapOwnership(context.Background(), dir, nil, func(context.Context) error { called = true; return nil })
	if err == nil {
		t.Fatal("corrupt database passed verification")
	}
	if called {
		t.Fatal("incumbent takeover ran before verification failed")
	}
}

func TestBootstrapOwnershipUsesPersistentLifetimeLock(t *testing.T) {
	dir := t.TempDir()
	store, lock, err := bootstrapOwnership(context.Background(), dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, statetool.LockFileName)
	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("second owner acquired daemon lock")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock inode was removed: %v", err)
	}
	if err = syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = syscall.Flock(int(contender.Fd()), syscall.LOCK_UN)
	_ = contender.Close()
}

func TestBootstrapConvergenceClassifiesCandidatesAndSeedsBus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := centralstore.UnixMillis(1000)
	runners := &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{
		"good": {Registration: centralstore.RunnerRegistration{ID: "sess-bootstrap", Adapter: "shell", Alive: true, CreatedAt: now, ObservedAt: now}},
	}, blocked: map[string]bool{"slow": true}}
	b, err := newBootstrap(BootstrapConfig{Store: store, Runners: runners, Control: bootstrapControl{}, Spawner: bootstrapSpawner{}, Reconciler: bootstrapReconciler{}, Converter: &wire.Converter{}, Endpoints: EndpointSourceFunc(func(context.Context) ([]string, error) { return []string{"good", "slow"}, nil }), Clock: func() centralstore.UnixMillis { return now }, RunnerBudget: 100 * time.Millisecond, ConvergeDeadline: 2 * time.Second, RetryInitial: time.Millisecond, RetryMaximum: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := b.Converge(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 {
		t.Fatalf("endpoints=%v", eps)
	}
	select {
	case <-b.Coordinator.Converged():
	default:
		t.Fatal("readiness barrier withheld after durable finish")
	}
	if err := b.StartPostConvergence(ctx, []string{"good"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.firstPair:
	default:
		t.Fatal("post-convergence returned before matched pair")
	}

	seed, events, unsubscribe, err := b.SubscribeOutcomes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if events == nil || len(seed) != 1 || seed[0].ID != "sess-bootstrap" || !seed[0].Alive || seed[0].Generation == 0 {
		t.Fatalf("seed=%+v events=%v", seed, events)
	}
}

func TestServeDoesNotReferenceCentralBootstrap(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"newBootstrap(", "bootstrapOwnership("} {
		if containsString(string(data), needle) {
			t.Fatalf("serve production file references inert bootstrap entry %q", needle)
		}
	}
}

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
