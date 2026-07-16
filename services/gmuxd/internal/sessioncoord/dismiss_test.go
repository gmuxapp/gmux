package sessioncoord

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// treeSessions builds the p → c → g launch chain plus an unrelated root x.
// exitless controls which members look alive-at-last-run (no exit fact).
func treeSessions(exitless ...centralstore.SessionID) []centralstore.Session {
	noExit := map[centralstore.SessionID]bool{}
	for _, id := range exitless {
		noExit[id] = true
	}
	mk := func(id centralstore.SessionID, parent centralstore.SessionID) centralstore.Session {
		s := centralstore.Session{ID: id, Adapter: "shell", Version: 1}
		if parent != "" {
			p := parent
			s.LaunchParentID = &p
		}
		if !noExit[id] {
			at := centralstore.UnixMillis(100)
			s.ExitedAt = &at
		}
		return s
	}
	return []centralstore.Session{mk("sess-p", ""), mk("sess-c", "sess-p"), mk("sess-g", "sess-c"), mk("sess-x", "")}
}

func newDismissCoord(t *testing.T, dur *fakeDurable, sink *fakeDirtySink) *Coordinator {
	t.Helper()
	return New(nil, newFakeClient(RunnerMeta{}), dur, sink,
		nil, WithClock(func() centralstore.UnixMillis { return 777 }))
}

func TestDismissDeadSubtreeCommitsAndPublishes(t *testing.T) {
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	var gotAt centralstore.UnixMillis
	dur.dismissResult = func(root centralstore.SessionID, at centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
		gotAt = at
		return []centralstore.SessionID{"sess-p", "sess-c", "sess-g"}, centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)

	dismissed, err := coord.Dismiss(context.Background(), "sess-p")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dismissed, []centralstore.SessionID{"sess-p", "sess-c", "sess-g"}) {
		t.Fatalf("dismissed=%v", dismissed)
	}
	if len(dur.dismissCalls) != 1 || dur.dismissCalls[0] != "sess-p" || gotAt != 777 {
		t.Fatalf("calls=%v at=%d", dur.dismissCalls, gotAt)
	}
	if sink.count() != 1 {
		t.Fatalf("published %d outcomes, want 1", sink.count())
	}
}

func TestDismissBlockedByLiveSubtreeMember(t *testing.T) {
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions("sess-g"), nil }
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "sess-g", Generation: 1}, dead: make(chan struct{})})

	if _, err := coord.Dismiss(context.Background(), "sess-p"); !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("err=%v", err)
	}
	if len(dur.dismissCalls) != 0 || sink.count() != 0 {
		t.Fatalf("blocked dismissal reached the store: calls=%v published=%d", dur.dismissCalls, sink.count())
	}
	// A live runner outside the subtree does not block.
	if _, err := coord.Dismiss(context.Background(), "sess-x"); err != nil {
		t.Fatalf("unrelated live runner blocked dismissal: %v", err)
	}
}

func TestDismissBlockedByInFlightSubtreeClaim(t *testing.T) {
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	coord := newDismissCoord(t, dur, &fakeDirtySink{})
	coord.mu.Lock()
	coord.ops["sess-c"] = &LifecycleClaim{op: "resume"}
	coord.mu.Unlock()

	_, err := coord.Dismiss(context.Background(), "sess-p")
	// ErrSubtreeBusy wraps ErrLifecycleOpInFlight: one sentinel suffices for
	// UI busy-retry mapping, and errors.Is matches both.
	if !errors.Is(err, ErrSubtreeBusy) || !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("err=%v", err)
	}
	if len(dur.dismissCalls) != 0 {
		t.Fatal("blocked dismissal reached the store")
	}
}

func TestDismissBlockedDuringConvergenceWindowForExitlessMember(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions("sess-c"), nil }
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := coord.Dismiss(ctx, "sess-p"); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("err=%v", err)
	}
	if len(dur.dismissCalls) != 0 {
		t.Fatal("blocked dismissal reached the store")
	}
	// A fully exited subtree is dismissable even while the window is open.
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	if _, err := coord.Dismiss(ctx, "sess-p"); err != nil {
		t.Fatalf("exited subtree blocked during window: %v", err)
	}
	// After the barrier closes, the exit-less member no longer blocks.
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions("sess-c"), nil }
	if _, err := coord.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Dismiss(ctx, "sess-p"); err != nil {
		t.Fatalf("dismissal blocked after barrier: %v", err)
	}
}

func TestDismissUnknownRootAndDurableFailure(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)

	if _, err := coord.Dismiss(ctx, "missing"); !errors.Is(err, centralstore.ErrSessionNotFound) {
		t.Fatalf("err=%v", err)
	}
	boom := errors.New("boom")
	dur.dismissResult = func(centralstore.SessionID, centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
		return nil, centralstore.MutationResult{}, boom
	}
	if _, err := coord.Dismiss(ctx, "sess-p"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if sink.count() != 0 {
		t.Fatal("failed dismissal must publish nothing")
	}
}

// gateDurable is a lock-free Durable whose RegisterRunner parks on a gate,
// making the registration commit window observable. Other methods delegate
// to simple closures.
type gateDurable struct {
	entered chan struct{}
	release chan struct{}
	list    func() ([]centralstore.Session, error)
	dismiss func(centralstore.SessionID, centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error)
	listed  chan struct{}
}

func (d *gateDurable) RegisterRunner(_ context.Context, reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
	close(d.entered)
	<-d.release
	return centralstore.Session{ID: reg.ID, Version: 1}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
}
func (d *gateDurable) ApplyRunnerObservation(context.Context, centralstore.RunnerObservation) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}
func (d *gateDurable) Session(context.Context, centralstore.SessionID) (centralstore.Session, bool, error) {
	return centralstore.Session{}, false, nil
}
func (d *gateDurable) ListSessions(context.Context) ([]centralstore.Session, error) {
	select {
	case <-d.listed:
	default:
		close(d.listed)
	}
	return d.list()
}
func (d *gateDurable) SweepDeadSessions(context.Context, []centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}
func (d *gateDurable) DismissSessionTree(_ context.Context, root centralstore.SessionID, at centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
	return d.dismiss(root, at)
}
func (d *gateDurable) RemoveSessionAtVersion(context.Context, centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}

// TestDismissSerializesWithRegisterCommitWindow parks a registration inside
// its RegisterRunner commit (lifecycle mutex held) and proves a concurrent
// Dismiss cannot even read the subtree until the registration completes —
// after which the freshly installed live generation blocks it.
func TestDismissSerializesWithRegisterCommitWindow(t *testing.T) {
	dur := &gateDurable{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		listed:  make(chan struct{}),
		list:    func() ([]centralstore.Session, error) { return treeSessions("sess-c"), nil },
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: "sess-c", Alive: true}})
	coord := New(nil, client, dur, &fakeDirtySink{}, nil,
		WithClock(func() centralstore.UnixMillis { return 777 }))
	atMutex := make(chan struct{})
	coord.beforeDismissLock = func() { close(atMutex) }

	registerDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep"})
		registerDone <- err
	}()
	<-dur.entered // registration is parked inside its commit, mutex held

	dismissErr := make(chan error, 1)
	go func() {
		_, err := coord.Dismiss(context.Background(), "sess-p")
		dismissErr <- err
	}()

	// Deterministic, not scheduler-dependent: wait until Dismiss has
	// provably reached its mutex acquisition. The registration still holds
	// the mutex (its commit is parked), so Dismiss cannot have progressed to
	// its subtree read — the non-blocking checks below cannot pass vacuously.
	<-atMutex
	select {
	case <-dur.listed:
		t.Fatal("Dismiss read the subtree inside the registration commit window")
	case err := <-dismissErr:
		t.Fatalf("Dismiss finished inside the commit window: %v", err)
	default:
	}

	close(dur.release)
	if err := <-registerDone; err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := <-dismissErr; !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("dismiss after registration install: err=%v", err)
	}
}

func TestRemoveCommitsAndPublishes(t *testing.T) {
	dur := newFakeDurable(0)
	var gotVersion centralstore.RowVersion
	dur.removeResult = func(id centralstore.SessionID, observed centralstore.RowVersion) (centralstore.MutationResult, error) {
		gotVersion = observed
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)

	if err := coord.Remove(context.Background(), "sess-p", 4); err != nil {
		t.Fatal(err)
	}
	if len(dur.removeCalls) != 1 || dur.removeCalls[0] != "sess-p" || gotVersion != 4 {
		t.Fatalf("calls=%v version=%d", dur.removeCalls, gotVersion)
	}
	if sink.count() != 1 {
		t.Fatalf("published %d outcomes, want 1", sink.count())
	}
}

func TestRemoveBlockedByLivenessClaimAndWindow(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)

	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "sess-p", Generation: 1}, dead: make(chan struct{})})
	if err := coord.Remove(ctx, "sess-p", 1); !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("err=%v", err)
	}
	coord.registry.remove("sess-p", 1)

	coord.mu.Lock()
	coord.ops["sess-p"] = &LifecycleClaim{op: "stop"}
	coord.mu.Unlock()
	if err := coord.Remove(ctx, "sess-p", 1); !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("err=%v", err)
	}
	coord.mu.Lock()
	delete(coord.ops, "sess-p")
	coord.mu.Unlock()

	// Exit-less row during the open convergence window: liveness unknown.
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "sess-p", Version: 1}, true, nil
	}
	dur.listSessions = func() ([]centralstore.Session, error) { return nil, nil }
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.Remove(ctx, "sess-p", 1); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("err=%v", err)
	}
	// An exited row is removable during the window.
	at := centralstore.UnixMillis(9)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "sess-p", Version: 1, ExitedAt: &at}, true, nil
	}
	if err := coord.Remove(ctx, "sess-p", 1); err != nil {
		t.Fatalf("exited row blocked during window: %v", err)
	}
	if len(dur.removeCalls) != 1 || sink.count() != 1 {
		t.Fatalf("calls=%v published=%d", dur.removeCalls, sink.count())
	}
}
