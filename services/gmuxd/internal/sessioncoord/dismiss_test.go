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
			s.ParentSessionID = &p
		}
		if !noExit[id] {
			at := centralstore.UnixMillis(100)
			s.ExitedAt = &at
		}
		return s
	}
	return []centralstore.Session{mk("151zemtf", ""), mk("10cel6cx", "151zemtf"), mk("1k41uwyr", "10cel6cx"), mk("1108gm0e", "")}
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
		return []centralstore.SessionID{"151zemtf", "10cel6cx", "1k41uwyr"}, centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)

	dismissed, err := coord.Dismiss(context.Background(), "151zemtf")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dismissed, []centralstore.SessionID{"151zemtf", "10cel6cx", "1k41uwyr"}) {
		t.Fatalf("dismissed=%v", dismissed)
	}
	if len(dur.dismissCalls) != 1 || dur.dismissCalls[0] != "151zemtf" || gotAt != 777 {
		t.Fatalf("calls=%v at=%d", dur.dismissCalls, gotAt)
	}
	if sink.count() != 1 {
		t.Fatalf("published %d outcomes, want 1", sink.count())
	}
}

func TestDismissBlockedByLiveSubtreeMember(t *testing.T) {
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions("1k41uwyr"), nil }
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "1k41uwyr", Generation: 1}, dead: make(chan struct{})})

	if _, err := coord.Dismiss(context.Background(), "151zemtf"); !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("err=%v", err)
	}
	if len(dur.dismissCalls) != 0 || sink.count() != 0 {
		t.Fatalf("blocked dismissal reached the store: calls=%v published=%d", dur.dismissCalls, sink.count())
	}
	// A live runner outside the subtree does not block.
	if _, err := coord.Dismiss(context.Background(), "1108gm0e"); err != nil {
		t.Fatalf("unrelated live runner blocked dismissal: %v", err)
	}
}

func TestDismissBlockedByInFlightSubtreeClaim(t *testing.T) {
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	coord := newDismissCoord(t, dur, &fakeDirtySink{})
	coord.mu.Lock()
	coord.ops["10cel6cx"] = &LifecycleClaim{op: "resume"}
	coord.mu.Unlock()

	_, err := coord.Dismiss(context.Background(), "151zemtf")
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
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions("10cel6cx"), nil }
	sink := &fakeDirtySink{}
	coord := newDismissCoord(t, dur, sink)
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := coord.Dismiss(ctx, "151zemtf"); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("err=%v", err)
	}
	if len(dur.dismissCalls) != 0 {
		t.Fatal("blocked dismissal reached the store")
	}
	// A fully exited subtree is dismissable even while the window is open.
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions(), nil }
	if _, err := coord.Dismiss(ctx, "151zemtf"); err != nil {
		t.Fatalf("exited subtree blocked during window: %v", err)
	}
	// After the barrier closes, the exit-less member no longer blocks.
	dur.listSessions = func() ([]centralstore.Session, error) { return treeSessions("10cel6cx"), nil }
	if _, err := coord.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Dismiss(ctx, "151zemtf"); err != nil {
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
	if _, err := coord.Dismiss(ctx, "151zemtf"); !errors.Is(err, boom) {
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
func (d *gateDurable) AcknowledgeDeadSession(context.Context, centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}
func (d *gateDurable) ReplaceProjectCatalogAndRematch(context.Context, []centralstore.ProjectEntrySpec, []centralstore.LocalPeerMatchInput, centralstore.UnixMillis) (centralstore.ProjectCatalog, centralstore.MutationResult, error) {
	return nil, centralstore.MutationResult{}, nil
}
func (d *gateDurable) PlaceUnplacedSessions(context.Context, []centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error) {
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
		list:    func() ([]centralstore.Session, error) { return treeSessions("10cel6cx"), nil },
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: "10cel6cx", Alive: true}})
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
		_, err := coord.Dismiss(context.Background(), "151zemtf")
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

// gatedLifecycleStore exposes the point after Dismiss has checked its durable
// subtree and runtime liveness but before DismissSessionTree commits. Reparent
// entry is separately observable so the tests prove it is parked on the
// coordinator mutex rather than merely not scheduled yet.
type gatedLifecycleStore struct {
	*centralstore.Store
	dismissEntered  chan struct{}
	dismissRelease  chan struct{}
	reparentEntered chan struct{}
}

func (s *gatedLifecycleStore) DismissSessionTree(ctx context.Context, root centralstore.SessionID, at centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
	close(s.dismissEntered)
	<-s.dismissRelease
	return s.Store.DismissSessionTree(ctx, root, at)
}

func (s *gatedLifecycleStore) SetSessionParent(ctx context.Context, id centralstore.SessionID, parent *centralstore.SessionID) (centralstore.MutationResult, error) {
	close(s.reparentEntered)
	return s.Store.SetSessionParent(ctx, id, parent)
}

func newGatedLifecycleCoord(t *testing.T, rows ...centralstore.NewSession) (*Coordinator, *gatedLifecycleStore) {
	t.Helper()
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, row := range rows {
		if _, _, err := store.InsertSession(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	gated := &gatedLifecycleStore{
		Store:           store,
		dismissEntered:  make(chan struct{}),
		dismissRelease:  make(chan struct{}),
		reparentEntered: make(chan struct{}),
	}
	return New(nil, nil, gated, &fakeDirtySink{}, nil,
		WithClock(func() centralstore.UnixMillis { return 777 })), gated
}

// TestReparentSerializesWithDismissMoveIn reproduces the previously unsafe
// schedule: a live root is adopted after Dismiss has checked an unrelated
// dead root but before its subtree transaction. Reparent must wait until the
// dismissal commits, leaving the live session visible.
func TestReparentSerializesWithDismissMoveIn(t *testing.T) {
	ctx := context.Background()
	coord, store := newGatedLifecycleCoord(t,
		centralstore.NewSession{ID: "root", Adapter: "shell", CreatedAt: 1},
		centralstore.NewSession{ID: "live", Adapter: "shell", CreatedAt: 2},
	)
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "live", Generation: 1}, dead: make(chan struct{})})

	dismissDone := make(chan struct {
		ids []centralstore.SessionID
		err error
	}, 1)
	go func() {
		ids, err := coord.Dismiss(ctx, "root")
		dismissDone <- struct {
			ids []centralstore.SessionID
			err error
		}{ids, err}
	}()
	<-store.dismissEntered
	released := false
	defer func() {
		if !released {
			close(store.dismissRelease)
		}
	}()

	atMutex := make(chan struct{})
	coord.beforeReparentLock = func() { close(atMutex) }
	parent := centralstore.SessionID("root")
	reparentDone := make(chan error, 1)
	go func() {
		_, err := coord.SetSessionParent(ctx, "live", &parent)
		reparentDone <- err
	}()
	<-atMutex
	select {
	case <-store.reparentEntered:
		t.Fatal("reparent entered the store inside dismissal's checked-subtree window")
	case err := <-reparentDone:
		t.Fatalf("reparent finished inside dismissal's checked-subtree window: %v", err)
	default:
	}

	close(store.dismissRelease)
	released = true
	dismissed := <-dismissDone
	if dismissed.err != nil || !reflect.DeepEqual(dismissed.ids, []centralstore.SessionID{"root"}) {
		t.Fatalf("Dismiss: ids=%v err=%v", dismissed.ids, dismissed.err)
	}
	if err := <-reparentDone; err != nil {
		t.Fatalf("SetSessionParent: %v", err)
	}
	<-store.reparentEntered
	live, ok, err := store.Session(ctx, "live")
	if err != nil || !ok {
		t.Fatalf("live session lookup: ok=%v err=%v", ok, err)
	}
	if live.DismissedAt != nil {
		t.Fatalf("live session was hidden: dismissed_at=%v", *live.DismissedAt)
	}
	if _, installed := coord.registry.current("live"); !installed {
		t.Fatal("live generation disappeared")
	}
}

// TestReparentSerializesWithDismissMoveOut proves the inverse schedule cannot
// silently narrow the subtree the user confirmed: moving a checked child out
// waits until both checked rows have been dismissed.
func TestReparentSerializesWithDismissMoveOut(t *testing.T) {
	ctx := context.Background()
	root := centralstore.SessionID("root")
	coord, store := newGatedLifecycleCoord(t,
		centralstore.NewSession{ID: root, Adapter: "shell", CreatedAt: 1},
		centralstore.NewSession{ID: "child", Adapter: "shell", CreatedAt: 2, ParentSessionID: &root},
	)

	dismissDone := make(chan struct {
		ids []centralstore.SessionID
		err error
	}, 1)
	go func() {
		ids, err := coord.Dismiss(ctx, root)
		dismissDone <- struct {
			ids []centralstore.SessionID
			err error
		}{ids, err}
	}()
	<-store.dismissEntered
	released := false
	defer func() {
		if !released {
			close(store.dismissRelease)
		}
	}()

	atMutex := make(chan struct{})
	coord.beforeReparentLock = func() { close(atMutex) }
	reparentDone := make(chan error, 1)
	go func() {
		_, err := coord.SetSessionParent(ctx, "child", nil)
		reparentDone <- err
	}()
	<-atMutex
	select {
	case <-store.reparentEntered:
		t.Fatal("move-out entered the store inside dismissal's checked-subtree window")
	case err := <-reparentDone:
		t.Fatalf("move-out finished inside dismissal's checked-subtree window: %v", err)
	default:
	}

	close(store.dismissRelease)
	released = true
	dismissed := <-dismissDone
	if dismissed.err != nil || !reflect.DeepEqual(dismissed.ids, []centralstore.SessionID{"root", "child"}) {
		t.Fatalf("Dismiss: ids=%v err=%v", dismissed.ids, dismissed.err)
	}
	if err := <-reparentDone; err != nil {
		t.Fatalf("SetSessionParent: %v", err)
	}
	child, ok, err := store.Session(ctx, "child")
	if err != nil || !ok || child.DismissedAt == nil {
		t.Fatalf("checked child escaped dismissal: row=%#v ok=%v err=%v", child, ok, err)
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

	if err := coord.Remove(context.Background(), "151zemtf", 4); err != nil {
		t.Fatal(err)
	}
	if len(dur.removeCalls) != 1 || dur.removeCalls[0] != "151zemtf" || gotVersion != 4 {
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

	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "151zemtf", Generation: 1}, dead: make(chan struct{})})
	if err := coord.Remove(ctx, "151zemtf", 1); !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("err=%v", err)
	}
	coord.registry.remove("151zemtf", 1)

	coord.mu.Lock()
	coord.ops["151zemtf"] = &LifecycleClaim{op: "stop"}
	coord.mu.Unlock()
	if err := coord.Remove(ctx, "151zemtf", 1); !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("err=%v", err)
	}
	coord.mu.Lock()
	delete(coord.ops, "151zemtf")
	coord.mu.Unlock()

	// Exit-less row during the open convergence window: liveness unknown.
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "151zemtf", Version: 1}, true, nil
	}
	dur.listSessions = func() ([]centralstore.Session, error) { return nil, nil }
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.Remove(ctx, "151zemtf", 1); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("err=%v", err)
	}
	// An exited row is removable during the window.
	at := centralstore.UnixMillis(9)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "151zemtf", Version: 1, ExitedAt: &at}, true, nil
	}
	if err := coord.Remove(ctx, "151zemtf", 1); err != nil {
		t.Fatalf("exited row blocked during window: %v", err)
	}
	if len(dur.removeCalls) != 1 || sink.count() != 1 {
		t.Fatalf("calls=%v published=%d", dur.removeCalls, sink.count())
	}
}
