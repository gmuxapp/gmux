package sessioncoord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func exitedNilSession(id centralstore.SessionID, version centralstore.RowVersion) centralstore.Session {
	return centralstore.Session{ID: id, Version: version, Adapter: "shell"}
}

func exitedSession(id centralstore.SessionID, version centralstore.RowVersion) centralstore.Session {
	x := centralstore.UnixMillis(9)
	s := exitedNilSession(id, version)
	s.ExitedAt = &x
	return s
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestConvergenceSweepsOnlyPreviouslyAliveUnclaimedRows(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			exitedNilSession("1izwq4o0", 3),
			exitedSession("1fpaqea0", 7),
		}, nil
	}
	dirty := &fakeDirtySink{}
	c := New(nil, newFakeClient(RunnerMeta{}), durable, dirty, nil)

	if isClosed(c.Converged()) {
		t.Fatal("barrier must not be complete before the window closes")
	}
	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := c.FinishConvergence(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty {
		t.Fatalf("result=%#v", result)
	}
	if len(durable.swept) != 1 {
		t.Fatalf("swept calls=%d, want one durable sweep", len(durable.swept))
	}
	got := durable.swept[0]
	if len(got) != 1 || got[0] != "1izwq4o0" {
		t.Fatalf("sweep candidates=%#v", got)
	}
	if !isClosed(c.Converged()) {
		t.Fatal("barrier-completion signal must fire after the sweep")
	}
	if dirty.count() != 1 {
		t.Fatalf("dirty publications=%d, want exactly one invalidation", dirty.count())
	}
}

func TestConvergenceExcludesRunnersThatReRegisteredDuringWindow(t *testing.T) {
	ctx := context.Background()
	id := sid(1)
	durable := newFakeDurable(1)
	// The row re-registers with identical durable facts: RegisterRunner
	// reports no change and the version stays where it was, so only the
	// live-registry exclusion protects the row from the sweep.
	durable.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return exitedNilSession(id, 1), centralstore.MutationResult{SessionVersion: 1}, nil
	}
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(id, 1), exitedNilSession("1ve25bnc", 2)}, nil
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Adapter: "shell", Alive: true}})
	c := New(nil, client, durable, nil, nil)

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: "sock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	got := durable.swept[0]
	if len(got) != 1 || got[0] != "1ve25bnc" {
		t.Fatalf("sweep candidates=%#v, want only the unclaimed row", got)
	}
}

// TestConvergenceSweepsRunnerWhoseStreamDroppedDuringWindow covers the
// register-then-lose-stream flavor at the coordinator level: a runner
// re-registers during the window, then its stream closes without exit facts,
// removing the registry generation. With no live generation and no recorded
// exit, the row must be swept at window close regardless of version churn.
func TestConvergenceSweepsRunnerWhoseStreamDroppedDuringWindow(t *testing.T) {
	ctx := context.Background()
	id := sid(2)
	durable := newFakeDurable(0)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(id, 1)}, nil
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Adapter: "shell", Alive: true}})
	c := New(nil, client, durable, nil, nil)

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: "sock"}); err != nil {
		t.Fatal(err)
	}
	// Stream drops without an exit event: the drain goroutine removes the
	// generation from the registry.
	client.stream.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, live := c.registry.current(id); !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registry entry never removed after stream close")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	got := durable.swept[0]
	if len(got) != 1 || got[0] != id {
		t.Fatalf("stream-dropped runner escaped the sweep: %#v", got)
	}
}

func TestRecoveryStateFreshDaemonReadyImmediatelyWithoutFlap(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	c := New(nil, newFakeClient(RunnerMeta{}), durable, nil, nil)

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	want := RecoveryState{Status: "ready", Expected: 0, Recovered: 0}
	for i := 0; i < 3; i++ {
		if got := c.RecoveryState(); got != want {
			t.Fatalf("open empty recovery state[%d] = %+v, want %+v", i, got, want)
		}
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != want {
		t.Fatalf("empty recovery flapped at terminal close: got %+v, want %+v", got, want)
	}
}

func TestRecoveryStateOnlyRemoteAndDeadSessionsReadyImmediately(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	durable.listSessions = func() ([]centralstore.Session, error) {
		// Durable.ListSessions is local-only: remote sessions live in the peer
		// world projection and therefore contribute no recovery candidates.
		// The local rows present here are already terminal.
		return []centralstore.Session{exitedSession("1fpaqea0", 1), exitedSession("1ve25bnc", 2)}, nil
	}
	c := New(nil, newFakeClient(RunnerMeta{}), durable, nil, nil)

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	want := RecoveryState{Status: "ready", Expected: 0, Recovered: 0}
	if got := c.RecoveryState(); got != want {
		t.Fatalf("remote/dead-only recovery state = %+v, want %+v", got, want)
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != want {
		t.Fatalf("remote/dead-only recovery flapped at terminal close: got %+v, want %+v", got, want)
	}
}

func TestRecoveryStateDerivesProgressAndTerminalTransition(t *testing.T) {
	ctx := context.Background()
	liveID := sid(6)
	missingID := centralstore.SessionID("1ve25bnc")
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(liveID, 1), exitedNilSession(missingID, 2), exitedSession("1fpaqea0", 3)}, nil
	}
	durable.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return exitedNilSession(liveID, 1), centralstore.MutationResult{SessionVersion: 1}, nil
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: liveID, Adapter: "shell", Alive: true}})
	c := New(nil, client, durable, nil, nil)

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "recovering", Expected: 2, Recovered: 0}) {
		t.Fatalf("initial recovery state = %+v", got)
	}
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: "sock"}); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "recovering", Expected: 2, Recovered: 1}) {
		t.Fatalf("partial recovery state = %+v", got)
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "ready", Expected: 2, Recovered: 1}) {
		t.Fatalf("terminal recovery state = %+v", got)
	}
	if len(durable.swept) != 1 || len(durable.swept[0]) != 1 || durable.swept[0][0] != missingID {
		t.Fatalf("terminal transition did not sweep missing runner: %#v", durable.swept)
	}
}

func TestConvergenceWindowLifecycleContract(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	c := New(nil, newFakeClient(RunnerMeta{}), durable, nil, nil)

	if _, err := c.FinishConvergence(ctx, 1); !errors.Is(err, ErrConvergenceNotOpen) {
		t.Fatalf("finish before begin: %v", err)
	}
	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.BeginConvergence(ctx); !errors.Is(err, ErrConvergenceOpen) {
		t.Fatalf("double begin: %v", err)
	}
	if _, err := c.FinishConvergence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishConvergence(ctx, 1); !errors.Is(err, ErrConvergenceClosed) {
		t.Fatalf("double finish: %v", err)
	}
	if err := c.BeginConvergence(ctx); !errors.Is(err, ErrConvergenceClosed) {
		t.Fatalf("begin after close: %v", err)
	}
}

func TestConvergenceSweepFailureKeepsWindowOpenForRetry(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession("1108gm0e", 4)}, nil
	}
	boom := errors.New("sweep failed")
	durable.sweepResult = func([]centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{}, boom
	}
	c := New(nil, newFakeClient(RunnerMeta{}), durable, nil, nil)
	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishConvergence(ctx, 500); !errors.Is(err, boom) {
		t.Fatalf("want sweep error, got %v", err)
	}
	if isClosed(c.Converged()) {
		t.Fatal("failed sweep must not complete the barrier")
	}
	durable.mu.Lock()
	durable.sweepResult = nil
	durable.mu.Unlock()
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if !isClosed(c.Converged()) {
		t.Fatal("retry must complete the barrier")
	}
	if len(durable.swept) != 2 || len(durable.swept[1]) != 1 || durable.swept[1][0] != "1108gm0e" {
		t.Fatalf("retry sweep candidates=%#v", durable.swept)
	}
}

func TestConvergenceListFailureLeavesWindowUnopened(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	boom := errors.New("list failed")
	durable.listSessions = func() ([]centralstore.Session, error) { return nil, boom }
	c := New(nil, newFakeClient(RunnerMeta{}), durable, nil, nil)
	if err := c.BeginConvergence(ctx); !errors.Is(err, boom) {
		t.Fatalf("want list error, got %v", err)
	}
	durable.mu.Lock()
	durable.listSessions = nil
	durable.mu.Unlock()
	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishConvergence(ctx, 1); err != nil {
		t.Fatal(err)
	}
}

func TestConvergenceBarrierSignalObservableFromAnotherGoroutine(t *testing.T) {
	ctx := context.Background()
	durable := newFakeDurable(0)
	c := New(nil, newFakeClient(RunnerMeta{}), durable, nil, nil)
	done := make(chan struct{})
	go func() {
		<-c.Converged()
		close(done)
	}()
	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishConvergence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not observe barrier completion")
	}
}

// TestRecoveryStateDegradedWhenPassAbandonedCandidates pins the honesty
// contract: a closed window whose pass gave up on runners it could not reach
// must not advertise ready with a short count — that is the
// authoritative-looking "ready 0/N" an install gate would read as "the
// sessions are gone" while the runners are alive and about to re-register.
// The promotion to ready happens when the periodic pass recovers them, and
// the machine stays monotone (never back to recovering).
func TestRecoveryStateDegradedWhenPassAbandonedCandidates(t *testing.T) {
	ctx := context.Background()
	first, second := sid(11), sid(12)
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(first, 1), exitedNilSession(second, 2)}, nil
	}
	next := first
	durable.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return exitedNilSession(next, 1), centralstore.MutationResult{SessionVersion: 1}, nil
	}
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: first, Adapter: "shell", Alive: true}})
	c := New(nil, client, durable, nil, nil)

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "recovering", Expected: 2, Recovered: 0}) {
		t.Fatalf("open window recovery state = %+v", got)
	}
	// The pass reached neither runner in time.
	c.NoteAbandonedRecovery(first, second)
	if got := c.RecoveryState(); got != (RecoveryState{Status: "recovering", Expected: 2, Recovered: 0}) {
		t.Fatalf("abandonment must not close the window early: %+v", got)
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "degraded", Expected: 2, Recovered: 0}) {
		t.Fatalf("abandoned pass reported %+v, want degraded 0/2", got)
	}

	// The periodic discovery pass recovers them one at a time.
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: "sock-1"}); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "degraded", Expected: 2, Recovered: 1}) {
		t.Fatalf("partial recovery reported %+v, want degraded 1/2", got)
	}
	next = second
	client.mu.Lock()
	client.meta = RunnerMeta{Registration: centralstore.RunnerRegistration{ID: second, Adapter: "shell", Alive: true}}
	client.mu.Unlock()
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: "sock-2"}); err != nil {
		t.Fatal(err)
	}
	want := RecoveryState{Status: "ready", Expected: 2, Recovered: 2}
	for i := 0; i < 3; i++ {
		if got := c.RecoveryState(); got != want {
			t.Fatalf("settled recovery state[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestRecoveryStateEmptyCandidateSetStaysReadyDespiteAbandonment guards
// #521's contract: an empty candidate set reports ready 0/0 immediately.
// Endpoint discovery for brand-new runners can legitimately abandon probes
// (a stale socket, a runner mid-bind); that must never make an empty daemon
// advertise a recovery phase.
func TestRecoveryStateEmptyCandidateSetStaysReadyDespiteAbandonment(t *testing.T) {
	ctx := context.Background()
	c := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), nil, nil)
	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	c.NoteAbandonedRecovery(sid(21), sid(22), sid(23))
	want := RecoveryState{Status: "ready", Expected: 0, Recovered: 0}
	if got := c.RecoveryState(); got != want {
		t.Fatalf("empty open recovery state = %+v, want %+v", got, want)
	}
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != want {
		t.Fatalf("empty recovery state after close = %+v, want %+v", got, want)
	}
}

// recoveryFleet is a transport with per-endpoint identity and streams, so a
// test can recover several candidates and then kill exactly one of them.
type recoveryFleet struct {
	mu      sync.Mutex
	streams map[string]*fakeStream
}

func newRecoveryFleet() *recoveryFleet {
	return &recoveryFleet{streams: make(map[string]*fakeStream)}
}

func (f *recoveryFleet) stream(ep string) *fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.streams[ep]
	if !ok {
		s = newFakeStream()
		f.streams[ep] = s
	}
	return s
}

func (f *recoveryFleet) Subscribe(_ context.Context, ep string) (EventStream, error) {
	return f.stream(ep), nil
}

func (f *recoveryFleet) Meta(_ context.Context, ep string) (RunnerMeta, error) {
	id := centralstore.SessionID(strings.TrimSuffix(ep, ".sock"))
	return RunnerMeta{PID: 1, Registration: centralstore.RunnerRegistration{ID: id, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1}}, nil
}

// TestRecoveryStateReadyIsStickyAcrossOrdinarySessionExit is the S1
// regression. `degraded` must be decided once, at window close, from what the
// pass abandoned — NOT re-derived from the live registry against the immutable
// startup candidate set. Deriving it live meant that once a pass had abandoned
// anything, the first ordinary session exit (a user quitting a session, days
// later, on a perfectly healthy daemon) flipped health back to `degraded`
// forever, breaking every consumer that gates on `ready`.
func TestRecoveryStateReadyIsStickyAcrossOrdinarySessionExit(t *testing.T) {
	ctx := context.Background()
	first, second := sid(31), sid(32)
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(first, 1), exitedNilSession(second, 2)}, nil
	}
	durable.registerResult = func(reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return exitedNilSession(reg.ID, 1), centralstore.MutationResult{Changed: true, SessionVersion: 1}, nil
	}
	durable.applyResult = func(centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{Changed: true, SessionVersion: 2}, nil
	}
	fleet := newRecoveryFleet()
	c := New(nil, fleet, durable, &fakeDirtySink{}, nil)
	defer c.Close()

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	c.NoteAbandonedRecovery(first) // the pass never got an answer about `first`
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got.Status != "degraded" {
		t.Fatalf("abandoned pass reported %+v, want degraded", got)
	}
	for _, id := range []centralstore.SessionID{first, second} {
		if _, err := c.Register(ctx, RegisterRequest{Endpoint: string(id) + ".sock"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "ready", Expected: 2, Recovered: 2}) {
		t.Fatalf("recovered state = %+v, want ready 2/2", got)
	}

	// An ordinary session exit, long after recovery finished.
	fleet.stream(string(second) + ".sock").send(RunnerEvent{ObservedAt: ts(100), Alive: aliveFalse, Facts: centralstore.RunnerFacts{ExitedAt: exitedAt(100)}})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && c.RecoveryState().Recovered == 2 {
		time.Sleep(5 * time.Millisecond)
	}
	got := c.RecoveryState()
	if got.Recovered != 1 {
		t.Fatalf("exit was not observed: %+v", got)
	}
	if got.Status != "ready" {
		t.Fatalf("ordinary session exit flipped health back to %+v; readiness must be sticky", got)
	}
}

// TestRecoveryStateDegradedPersistsUntilTheAbandonedRunnerReturns pins the
// one-way promotion in both directions of the contract: the degraded bit is
// keyed on the abandoned candidate itself, so unrelated churn neither clears
// it nor re-triggers it, and only that candidate's return promotes it.
func TestRecoveryStateDegradedPersistsUntilTheAbandonedRunnerReturns(t *testing.T) {
	ctx := context.Background()
	abandonedID, other := sid(41), sid(42)
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(abandonedID, 1), exitedNilSession(other, 2)}, nil
	}
	durable.registerResult = func(reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return exitedNilSession(reg.ID, 1), centralstore.MutationResult{Changed: true, SessionVersion: 1}, nil
	}
	fleet := newRecoveryFleet()
	c := New(nil, fleet, durable, &fakeDirtySink{}, nil)
	defer c.Close()

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	// A stale socket belonging to no recovery candidate is not recovery news:
	// it must not make an unrelated shortfall look like abandonment.
	c.NoteAbandonedRecovery(sid(43))
	if got := c.RecoveryState(); got.Status != "recovering" {
		t.Fatalf("non-candidate note changed the open state: %+v", got)
	}
	c.NoteAbandonedRecovery(abandonedID)
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	// A note that arrives after the close cannot change the frozen decision.
	c.NoteAbandonedRecovery(other)
	// The unrelated candidate coming back does not clear the degraded bit.
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: string(other) + ".sock"}); err != nil {
		t.Fatal(err)
	}
	if got := c.RecoveryState(); got != (RecoveryState{Status: "degraded", Expected: 2, Recovered: 1}) {
		t.Fatalf("unrelated recovery reported %+v, want degraded 1/2", got)
	}
	// The abandoned runner answers on a later pass: promoted, for good.
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: string(abandonedID) + ".sock"}); err != nil {
		t.Fatal(err)
	}
	want := RecoveryState{Status: "ready", Expected: 2, Recovered: 2}
	for i := 0; i < 3; i++ {
		if got := c.RecoveryState(); got != want {
			t.Fatalf("promoted recovery state[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestRecoveryStateAbandonedButInstalledBeforeCloseIsReady pins the sweep
// intersection at the window close. One session id can be reached through two
// endpoints — a legacy socket directory keeps a stale pathname while the
// runner serves the current one — so the same pass can BOTH note an id as
// abandoned (the stale probe times out) and install it (the live probe
// registers). Nothing was swept dead on no evidence, and the promotion hook
// has already fired by the time the window closes, so taking the noted set
// wholesale would freeze this daemon in degraded for its entire lifetime.
func TestRecoveryStateAbandonedButInstalledBeforeCloseIsReady(t *testing.T) {
	ctx := context.Background()
	id := sid(51)
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{exitedNilSession(id, 1)}, nil
	}
	durable.registerResult = func(reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return exitedNilSession(reg.ID, 1), centralstore.MutationResult{Changed: true, SessionVersion: 1}, nil
	}
	c := New(nil, newRecoveryFleet(), durable, &fakeDirtySink{}, nil)
	defer c.Close()

	if err := c.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	// The live endpoint registers; the stale legacy pathname for the same id
	// times out and is noted. Both happen inside the same pass.
	if _, err := c.Register(ctx, RegisterRequest{Endpoint: string(id) + ".sock"}); err != nil {
		t.Fatal(err)
	}
	c.NoteAbandonedRecovery(id)
	if _, err := c.FinishConvergence(ctx, 500); err != nil {
		t.Fatal(err)
	}
	want := RecoveryState{Status: "ready", Expected: 1, Recovered: 1}
	if got := c.RecoveryState(); got != want {
		t.Fatalf("recovered-but-also-abandoned candidate reported %+v, want %+v", got, want)
	}
	if len(durable.swept) != 1 || len(durable.swept[0]) != 0 {
		t.Fatalf("an installed candidate must not be swept: %#v", durable.swept)
	}
}
