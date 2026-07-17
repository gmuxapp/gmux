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

// ── lifecycle fakes ──────────────────────────────────────────────────────────

// fakeControl records Terminate calls and can fail, kill the runner's stream
// (simulating process death observed as a stream drop), or do nothing
// (simulating a runner that never dies).
type fakeControl struct {
	mu        sync.Mutex
	calls     []string
	err       error
	onKill    func(endpoint string)
	entered   chan struct{} // closed on first Terminate, if non-nil
	enterOnce sync.Once
}

func (f *fakeControl) Terminate(_ context.Context, endpoint string) error {
	f.mu.Lock()
	f.calls = append(f.calls, endpoint)
	err := f.err
	kill := f.onKill
	f.mu.Unlock()
	if f.entered != nil {
		f.enterOnce.Do(func() { close(f.entered) })
	}
	if err != nil {
		return err
	}
	if kill != nil {
		kill(endpoint)
	}
	return nil
}
func (f *fakeControl) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeSpawner returns a fixed endpoint and can fail or block.
type fakeSpawner struct {
	mu       sync.Mutex
	calls    []centralstore.Session
	cleanups []string
	endpoint string
	err      error
	block    chan struct{} // if non-nil, Spawn blocks until closed
	entered  chan struct{} // closed on first Spawn, if non-nil
	once     sync.Once
}

func (f *fakeSpawner) Spawn(ctx context.Context, s centralstore.Session) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	if f.entered != nil {
		f.once.Do(func() { close(f.entered) })
	}
	if f.block != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.block:
		}
	}
	if f.err != nil {
		return "", f.err
	}
	return f.endpoint, nil
}

func (f *fakeSpawner) CleanupSpawn(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, endpoint)
	return nil
}
func (f *fakeSpawner) FinalizeSpawn(string) {}
func (f *fakeSpawner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// deadRow returns a durable session row with a recorded exit.
func deadRow(id centralstore.SessionID, version centralstore.RowVersion) centralstore.Session {
	x := ts(500)
	return centralstore.Session{ID: id, Version: version, Adapter: "shell", ExitedAt: &x}
}

func fixedClock(ms int64) Option {
	return WithClock(func() centralstore.UnixMillis { return ts(ms) })
}

// registerLive registers a live runner and fails the test on error.
func registerLive(t *testing.T, coord *Coordinator, endpoint string) Runtime {
	t.Helper()
	rt, err := coord.Register(context.Background(), RegisterRequest{Endpoint: endpoint})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return rt
}

// trackingDurable wraps fakeDurable with an appliedCh signal per apply call.
func withApplySignal(dur *fakeDurable) chan centralstore.RunnerObservation {
	ch := make(chan centralstore.RunnerObservation, 16)
	inner := dur.applyResult
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		r, err := inner(obs)
		ch <- obs
		return r, err
	}
	return ch
}

// ── observed-exit / stream-drop synthesis ────────────────────────────────────

// TestStreamDropSynthesizesDurableExit verifies that a mid-life stream drop
// without an observed exit fact commits a synthesized exit (explicit
// timestamp, no exit code, no turn-state fields) before liveness is removed.
func TestStreamDropSynthesizesDurableExit(t *testing.T) {
	id := sid(300)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	applied := withApplySignal(dur)
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, fixedClock(777))

	registerLive(t, coord, "ep300")
	client.stream.Close()

	select {
	case obs := <-applied:
		if obs.Facts.ExitedAt.Set == nil || *obs.Facts.ExitedAt.Set != ts(777) {
			t.Fatalf("expected synthesized ExitedAt=777, got %+v", obs.Facts.ExitedAt)
		}
		if obs.Facts.ExitCode.Set != nil || obs.Facts.Working != nil || obs.Facts.Unread != nil || obs.Facts.Error != nil {
			t.Fatalf("synthesized exit must not carry exit code or turn state: %+v", obs.Facts)
		}
	case <-time.After(time.Second):
		t.Fatal("no synthesized exit observation after stream drop")
	}
	// Liveness removed after the commit.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(coord.Registry().Snapshot()) == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("registry entry not removed after synthesized exit")
}

// TestStreamDropAfterExitFactDoesNotOverwrite verifies that a stream drop
// following an event that already carried exit facts does not write a second
// (synthesized) exit.
func TestStreamDropAfterExitFactDoesNotOverwrite(t *testing.T) {
	id := sid(301)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	applied := withApplySignal(dur)
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, fixedClock(777))

	registerLive(t, coord, "ep301")
	client.stream.send(RunnerEvent{ObservedAt: ts(100), Alive: aliveFalse, Facts: centralstore.RunnerFacts{ExitedAt: exitedAt(100)}})

	obs := <-applied
	if obs.Facts.ExitedAt.Set == nil || *obs.Facts.ExitedAt.Set != ts(100) {
		t.Fatalf("expected real exit at 100, got %+v", obs.Facts.ExitedAt)
	}
	client.stream.Close()

	// Entry must already be removed by the exit event; no second apply may
	// arrive from the drop.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case extra := <-applied:
			t.Fatalf("unexpected second observation after stream drop: %+v", extra)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("registry entry not removed")
	}
}

// TestStreamDropOfReplacedGenerationDoesNotSynthesize verifies that a
// replaced generation's stream drop cannot commit a synthesized exit onto
// the replacement's row.
//
// The old drain must still be alive when the drop lands (a drain killed by
// the replacement's closeEntry context-cancel never reaches the synthesis
// path, which made the previous version of this test vacuous). The test
// simulates the replacement's fence window directly: it holds c.mu and
// supersedes the old generation (exactly what Register does), closes the old
// stream — whose context is still alive, so the drain deterministically
// takes the channel-closed branch into synthesis — then installs the
// replacement and releases the mutex. The synthesized apply must be dropped
// by the fence/generation check.
func TestStreamDropOfReplacedGenerationDoesNotSynthesize(t *testing.T) {
	id := sid(302)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	applied := withApplySignal(dur)
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, fixedClock(777))

	rt1 := registerLive(t, coord, "ep302")
	oldStream := client.stream

	// Open the fence window by hand: hold the lifecycle mutex and supersede
	// gen1, mirroring Register's replacement path.
	coord.mu.Lock()
	if !coord.registry.supersede(id, rt1.Generation) {
		coord.mu.Unlock()
		t.Fatal("supersede failed")
	}
	// Drop the old stream while its context is alive: the drain reads the
	// closed channel (not ctx.Done) and enters the synthesis path; its apply
	// hits the fence and waits on c.mu.
	oldStream.Close()
	// Install the replacement (closes gen1's dead channel) and end the window.
	gen2 := rt1.Generation + 1000
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: id, Generation: gen2, Endpoint: "ep302b", Subscribed: true}, dead: make(chan struct{})})
	coord.mu.Unlock()

	// The synthesized apply now resolves its fence wait and must be dropped
	// by the generation check (installed generation is gen2, not gen1).
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case obs := <-applied:
			t.Fatalf("replaced generation's drop reached the store: %+v", obs)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	snap := coord.Registry().Snapshot()
	if len(snap) != 1 || snap[0].Generation != gen2 {
		t.Fatalf("replacement not still installed: %+v", snap)
	}
}

// TestStreamDropDuringFailedReplacementSynthesizes verifies the fence
// interleaving: the old generation's stream drops while a replacement is
// inside its (ultimately failing) fence window; after restore, the
// still-installed generation's synthesized exit must commit.
func TestStreamDropDuringFailedReplacementSynthesizes(t *testing.T) {
	id := sid(303)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})

	registerEntered := make(chan struct{})
	registerRelease := make(chan struct{})
	appliedCh := make(chan centralstore.RunnerObservation, 4)
	var mu sync.Mutex
	registerCalls := 0
	dur := &scheduledDurable{
		register: func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
			mu.Lock()
			registerCalls++
			n := registerCalls
			mu.Unlock()
			if n == 2 {
				close(registerEntered)
				<-registerRelease
				return centralstore.Session{}, centralstore.MutationResult{}, errors.New("replacement db failure")
			}
			return centralstore.Session{Version: 1}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
		},
		apply: func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
			appliedCh <- obs
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
		},
	}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, fixedClock(888))

	registerLive(t, coord, "ep303")
	oldStream := client.stream

	client.stream = newFakeStream()
	cl, release, claimErr := coord.claim(id, "test")
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	defer release()
	regDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep303", Replace: true, Claim: cl})
		regDone <- err
	}()
	<-registerEntered

	// Drop the old stream while the fence window is open; then fail the
	// replacement.
	oldStream.Close()
	close(registerRelease)
	if err := <-regDone; err == nil {
		t.Fatal("expected replacement to fail")
	}

	select {
	case obs := <-appliedCh:
		if obs.Facts.ExitedAt.Set == nil || *obs.Facts.ExitedAt.Set != ts(888) {
			t.Fatalf("expected synthesized exit after fence restore, got %+v", obs)
		}
	case <-time.After(time.Second):
		t.Fatal("synthesized exit did not commit after the fence lifted")
	}
}

// ── Stop ─────────────────────────────────────────────────────────────────────

// TestStopKillsWaitsAndConverges verifies the happy path: Terminate is
// delivered, death arrives as a stream drop, the synthesized exit lands
// durably, and Stop returns nil.
func TestStopKillsWaitsAndConverges(t *testing.T) {
	id := sid(310)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	// Session reflects the synthesized exit once it was applied. Both the
	// applyResult override and the session closure run under dur.mu.
	var exited []centralstore.RunnerObservation
	base := dur.applyResult
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		r, err := base(obs)
		if err == nil && obs.Facts.ExitedAt.Set != nil {
			exited = append(exited, obs)
		}
		return r, err
	}
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		if len(exited) > 0 {
			return deadRow(id, 2), true, nil
		}
		return centralstore.Session{ID: id, Version: 1, Adapter: "shell"}, true, nil
	}
	control := &fakeControl{onKill: func(string) { client.stream.Close() }}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, WithRunnerControl(control), fixedClock(900))

	registerLive(t, coord, "ep310")
	if err := coord.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	dur.mu.Lock()
	if len(exited) != 1 || *exited[0].Facts.ExitedAt.Set != ts(900) {
		dur.mu.Unlock()
		t.Fatalf("expected one synthesized exit at 900, got %+v", exited)
	}
	dur.mu.Unlock()
	if control.count() != 1 {
		t.Fatalf("expected 1 Terminate, got %d", control.count())
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("live entry survived Stop")
	}
}

// TestStopDeathViaExplicitExitEvent verifies Stop returns after death is
// observed through a well-formed exit event.
func TestStopDeathViaExplicitExitEvent(t *testing.T) {
	id := sid(311)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	control := &fakeControl{onKill: func(string) {
		client.stream.send(RunnerEvent{ObservedAt: ts(100), Alive: aliveFalse, Facts: centralstore.RunnerFacts{ExitedAt: exitedAt(100)}})
	}}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, WithRunnerControl(control))

	registerLive(t, coord, "ep311")
	if err := coord.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("live entry survived Stop")
	}
}

// TestStopNotAlive verifies stopping a session without a live generation.
func TestStopNotAlive(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), nil, nil, WithRunnerControl(&fakeControl{}))
	if err := coord.Stop(context.Background(), sid(312)); !errors.Is(err, ErrSessionNotAlive) {
		t.Fatalf("expected ErrSessionNotAlive, got %v", err)
	}
}

// TestStopNoControlConfigured verifies the unconfigured-boundary error.
func TestStopNoControlConfigured(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), nil, nil)
	if err := coord.Stop(context.Background(), sid(313)); !errors.Is(err, ErrNoRunnerControl) {
		t.Fatalf("expected ErrNoRunnerControl, got %v", err)
	}
}

// TestStopTerminateFailure verifies a Terminate error propagates and leaves
// the live entry installed.
func TestStopTerminateFailure(t *testing.T) {
	id := sid(314)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	control := &fakeControl{err: errors.New("signal failed")}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control))

	registerLive(t, coord, "ep314")
	err := coord.Stop(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "signal failed") || control.count() != 1 {
		t.Fatalf("expected terminate failure, got %v", err)
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("live entry must survive a failed terminate")
	}
}

// TestStopRunnerNeverDies verifies the bounded wait: kill delivered but the
// runner never dies; Stop returns the context error, the entry stays
// installed, and nothing durable changed.
func TestStopRunnerNeverDies(t *testing.T) {
	id := sid(315)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	entered := make(chan struct{})
	control := &fakeControl{entered: entered} // Terminate succeeds; nothing dies
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control))

	registerLive(t, coord, "ep315")
	appliesBefore := len(dur.applied)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coord.Stop(ctx, id) }()
	<-entered // kill delivered; runner ignores it
	cancel()  // deterministic bound expiry

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("entry must remain installed when the runner never dies")
	}
	dur.mu.Lock()
	defer dur.mu.Unlock()
	if len(dur.applied) != appliesBefore {
		t.Fatal("no durable write may happen when death was not observed")
	}
}

// TestStopRepairsFailedSynthesis verifies ensureDurableExit: the drain's
// synthesized exit fails against the store, yet Stop repairs the row at its
// current version and returns nil.
func TestStopRepairsFailedSynthesis(t *testing.T) {
	id := sid(316)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	var mu sync.Mutex
	failFirstExit := true
	var repaired []centralstore.RunnerObservation
	base := dur.applyResult
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		if obs.Facts.ExitedAt.Set != nil {
			mu.Lock()
			defer mu.Unlock()
			if failFirstExit {
				failFirstExit = false
				return centralstore.MutationResult{}, errors.New("db unavailable")
			}
			repaired = append(repaired, obs)
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
		}
		return base(obs)
	}
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(repaired) > 0 {
			return deadRow(id, 2), true, nil
		}
		return centralstore.Session{ID: id, Version: 1, Adapter: "shell"}, true, nil // still exit-less
	}
	errSink := &fakeErrorSink{}
	control := &fakeControl{onKill: func(string) { client.stream.Close() }}
	coord := New(nil, client, dur, &fakeDirtySink{}, errSink, WithRunnerControl(control), fixedClock(950))

	registerLive(t, coord, "ep316")
	if err := coord.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(repaired) != 1 || *repaired[0].Facts.ExitedAt.Set != ts(950) || repaired[0].ObservedVersion != 1 {
		t.Fatalf("expected one repair at version 1, got %+v", repaired)
	}
	if errSink.count() == 0 {
		t.Fatal("the failed synthesis must have been reported")
	}
}

// ── Resume ───────────────────────────────────────────────────────────────────

// TestResumeSpawnsAndReplaces verifies the happy path: spawn, registration
// with Replace/NewGeneration provenance, live entry installed.
func TestResumeSpawnsAndReplaces(t *testing.T) {
	id := sid(320)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 3), true, nil }
	spawner := &fakeSpawner{endpoint: "ep320"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	rt, err := coord.Resume(context.Background(), id)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if rt.SessionID != id || !rt.Subscribed {
		t.Fatalf("runtime = %+v", rt)
	}
	if spawner.count() != 1 {
		t.Fatalf("expected 1 spawn, got %d", spawner.count())
	}
	if spawner.calls[0].ID != id || spawner.calls[0].ExitedAt == nil {
		t.Fatalf("spawner must receive the durable dead row, got %+v", spawner.calls[0])
	}
	if len(dur.registered) != 1 || !dur.registered[0].NewGeneration {
		t.Fatalf("resume must register with NewGeneration provenance: %+v", dur.registered)
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("expected live entry after resume")
	}
}

// TestResumeOfLiveSessionFails verifies clean failure with no spawn.
func TestResumeOfLiveSessionFails(t *testing.T) {
	id := sid(321)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	spawner := &fakeSpawner{endpoint: "epX"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	registerLive(t, coord, "ep321")
	if _, err := coord.Resume(context.Background(), id); !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("expected ErrSessionAlive, got %v", err)
	}
	if spawner.count() != 0 {
		t.Fatal("no spawn may happen for a live session")
	}
}

// TestResumeUnknownSession verifies ErrSessionNotFound passthrough.
func TestResumeUnknownSession(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), nil, nil, WithRunnerSpawner(&fakeSpawner{}))
	if _, err := coord.Resume(context.Background(), sid(322)); !errors.Is(err, centralstore.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

// TestResumeNoSpawnerConfigured verifies the unconfigured-boundary error.
func TestResumeNoSpawnerConfigured(t *testing.T) {
	coord := New(nil, newFakeClient(RunnerMeta{}), newFakeDurable(0), nil, nil)
	if _, err := coord.Resume(context.Background(), sid(323)); !errors.Is(err, ErrNoRunnerSpawner) {
		t.Fatalf("expected ErrNoRunnerSpawner, got %v", err)
	}
}

// TestConcurrentResumeSingleSpawn verifies that concurrent resume calls for
// one session cannot double-spawn: the loser fails ErrLifecycleOpInFlight
// while the winner is still inside Spawn.
func TestConcurrentResumeSingleSpawn(t *testing.T) {
	id := sid(324)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 3), true, nil }
	entered := make(chan struct{})
	release := make(chan struct{})
	spawner := &fakeSpawner{endpoint: "ep324", entered: entered, block: release}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	done := make(chan error, 1)
	go func() {
		_, err := coord.Resume(context.Background(), id)
		done <- err
	}()
	<-entered // winner is inside Spawn, claim held

	if _, err := coord.Resume(context.Background(), id); !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("expected ErrLifecycleOpInFlight, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("winner resume: %v", err)
	}
	if spawner.count() != 1 {
		t.Fatalf("expected exactly one spawn, got %d", spawner.count())
	}
}

// TestResumeSpawnFailureReleasesClaim verifies a spawn failure changes
// nothing and a later resume succeeds.
func TestResumeSpawnFailureReleasesClaim(t *testing.T) {
	id := sid(325)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 3), true, nil }
	spawner := &fakeSpawner{endpoint: "ep325", err: errors.New("exec failed")}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	if _, err := coord.Resume(context.Background(), id); err == nil {
		t.Fatal("expected spawn failure")
	}
	if len(dur.registered) != 0 || len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("spawn failure must leave DB and registry untouched")
	}

	spawner.err = nil
	if _, err := coord.Resume(context.Background(), id); err != nil {
		t.Fatalf("retry after spawn failure: %v", err)
	}
}

// TestResumeRegistrationFailureReleasesClaim verifies the "stream never
// arrives after spawn" path: subscribe fails, resume errors, nothing durable
// changed, claim released.
func TestResumeRegistrationFailureReleasesClaim(t *testing.T) {
	id := sid(326)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	client.subscribeErr = errors.New("no socket")
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 3), true, nil }
	spawner := &fakeSpawner{endpoint: "ep326"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	if _, err := coord.Resume(context.Background(), id); err == nil {
		t.Fatal("expected registration failure")
	}
	if len(dur.registered) != 0 || len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("registration failure must leave DB and registry untouched")
	}
	spawner.mu.Lock()
	if len(spawner.cleanups) != 1 || spawner.cleanups[0] != "ep326" {
		t.Fatalf("registration failure cleanup=%v", spawner.cleanups)
	}
	spawner.mu.Unlock()
	client.mu.Lock()
	client.subscribeErr = nil
	client.mu.Unlock()
	if _, err := coord.Resume(context.Background(), id); err != nil {
		t.Fatalf("retry after registration failure: %v", err)
	}
}

// TestResumeIdentityMismatch verifies a spawned runner whose meta claims a
// different session ID aborts before any commit: no durable registration, no
// registry entry, provisional stream closed.
func TestResumeIdentityMismatch(t *testing.T) {
	id := sid(327)
	other := sid(328)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: other, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 3), true, nil }
	spawner := &fakeSpawner{endpoint: "ep327"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	_, err := coord.Resume(context.Background(), id)
	if !errors.Is(err, ErrResumeIdentityMismatch) {
		t.Fatalf("expected ErrResumeIdentityMismatch, got %v", err)
	}
	if len(dur.registered) != 0 {
		t.Fatalf("mismatch must abort before commit: %+v", dur.registered)
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatalf("mismatch must not install anything: %+v", coord.Registry().Snapshot())
	}
	if !client.stream.closed.Load() {
		t.Fatal("provisional stream must be closed after mismatch abort")
	}
}

// TestResumeIdentityMismatchCannotHijackLiveSession verifies the M2 finding:
// a spawned runner mis-claiming a *live* other session must never supersede
// that session's generation. The other session's stream, generation, and
// durable row stay untouched.
func TestResumeIdentityMismatchCannotHijackLiveSession(t *testing.T) {
	id := sid(350)
	other := sid(351)
	otherClient := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: other, Alive: true}})
	// The spawn endpoint serves a runner claiming the live other session.
	hijackClient := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: other, Alive: true}})
	epClient := &endpointRunnerClient{routes: map[string]*fakeRunnerClient{
		"ep-other":  otherClient,
		"ep-hijack": hijackClient,
	}}
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 3), true, nil }
	spawner := &fakeSpawner{endpoint: "ep-hijack"}
	coord := New(nil, epClient, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	rtOther := registerLive(t, coord, "ep-other")
	registrationsBefore := len(dur.registered)

	_, err := coord.Resume(context.Background(), id)
	if !errors.Is(err, ErrResumeIdentityMismatch) {
		t.Fatalf("expected ErrResumeIdentityMismatch, got %v", err)
	}
	snap := coord.Registry().Snapshot()
	if len(snap) != 1 || snap[0].SessionID != other || snap[0].Generation != rtOther.Generation {
		t.Fatalf("live other session was disturbed: %+v (want gen %d)", snap, rtOther.Generation)
	}
	dur.mu.Lock()
	defer dur.mu.Unlock()
	if len(dur.registered) != registrationsBefore {
		t.Fatalf("hijack committed a registration: %+v", dur.registered[registrationsBefore:])
	}
	if !hijackClient.stream.closed.Load() {
		t.Fatal("hijacking runner's provisional stream must be closed")
	}
	// The other session's stream must still be live (not cancelled/closed).
	if otherClient.stream.closed.Load() {
		t.Fatal("live other session's stream was closed")
	}
}

// TestResumeExitlessRowDuringConvergenceWindow verifies the conservative
// guard: an exit-less candidate cannot be resumed while the window is open,
// and can be after FinishConvergence recorded its exit.
func TestResumeExitlessRowDuringConvergenceWindow(t *testing.T) {
	id := sid(329)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	exitless := centralstore.Session{ID: id, Version: 1, Adapter: "shell"}
	rows := []centralstore.Session{exitless}
	var mu sync.Mutex
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return rows[0], true, nil
	}
	dur.listSessions = func() ([]centralstore.Session, error) { return []centralstore.Session{exitless}, nil }
	spawner := &fakeSpawner{endpoint: "ep329"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	if err := coord.BeginConvergence(context.Background()); err != nil {
		t.Fatalf("BeginConvergence: %v", err)
	}
	if _, err := coord.Resume(context.Background(), id); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("expected ErrConvergencePending, got %v", err)
	}
	if spawner.count() != 0 {
		t.Fatal("no spawn during the open window")
	}

	if _, err := coord.FinishConvergence(context.Background(), ts(600)); err != nil {
		t.Fatalf("FinishConvergence: %v", err)
	}
	mu.Lock()
	rows[0] = deadRow(id, 2) // the sweep recorded the exit
	mu.Unlock()
	if _, err := coord.Resume(context.Background(), id); err != nil {
		t.Fatalf("Resume after barrier: %v", err)
	}
}

// TestResumeExitedRowDuringConvergenceWindow verifies rows with a recorded
// exit are resumable even while the window is open.
func TestResumeExitedRowDuringConvergenceWindow(t *testing.T) {
	id := sid(330)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	spawner := &fakeSpawner{endpoint: "ep330"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	if err := coord.BeginConvergence(context.Background()); err != nil {
		t.Fatalf("BeginConvergence: %v", err)
	}
	if _, err := coord.Resume(context.Background(), id); err != nil {
		t.Fatalf("Resume of exited row during window: %v", err)
	}
}

// TestResumedSessionExcludedFromSweep verifies a session resumed during the
// window has a live generation at FinishConvergence and is not swept.
func TestResumedSessionExcludedFromSweep(t *testing.T) {
	id := sid(331)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	exitless := centralstore.Session{ID: id, Version: 1, Adapter: "shell"}
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	dur.listSessions = func() ([]centralstore.Session, error) { return []centralstore.Session{exitless}, nil }
	spawner := &fakeSpawner{endpoint: "ep331"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerSpawner(spawner))

	if err := coord.BeginConvergence(context.Background()); err != nil {
		t.Fatalf("BeginConvergence: %v", err)
	}
	// Row is exit-less durably but has a recorded exit per Session (deadRow),
	// so resume is permitted; it installs a live generation.
	if _, err := coord.Resume(context.Background(), id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := coord.FinishConvergence(context.Background(), ts(700)); err != nil {
		t.Fatalf("FinishConvergence: %v", err)
	}
	dur.mu.Lock()
	defer dur.mu.Unlock()
	if len(dur.swept) != 1 || len(dur.swept[0]) != 0 {
		t.Fatalf("resumed live session must be excluded from the sweep: %+v", dur.swept)
	}
}

// ── Restart ──────────────────────────────────────────────────────────────────

// TestRestartComposesStopAndResume verifies the full path under one claim:
// terminate, observed death, spawn, replacement registration.
func TestRestartComposesStopAndResume(t *testing.T) {
	id := sid(340)
	firstStream := newFakeStream()
	secondStream := newFakeStream()
	streams := []*fakeStream{firstStream, secondStream}
	var smu sync.Mutex
	client := &switchingClient{meta: RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}, streams: streams, mu: &smu}
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	control := &fakeControl{onKill: func(string) { firstStream.Close() }}
	spawner := &fakeSpawner{endpoint: "ep340-b"}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{},
		WithRunnerControl(control), WithRunnerSpawner(spawner), fixedClock(1000))

	rt1 := registerLive(t, coord, "ep340-a")
	rt2, err := coord.Restart(context.Background(), id)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if control.count() != 1 || spawner.count() != 1 {
		t.Fatalf("terminate=%d spawn=%d", control.count(), spawner.count())
	}
	if rt2.Generation <= rt1.Generation || rt2.SessionID != id {
		t.Fatalf("expected a newer generation, got %+v after %+v", rt2, rt1)
	}
	if len(dur.registered) != 2 || !dur.registered[1].NewGeneration {
		t.Fatalf("restart registration must carry NewGeneration: %+v", dur.registered)
	}
}

// TestRestartStopPhaseFailureAbortsBeforeSpawn verifies a terminate failure
// prevents any spawn.
func TestRestartStopPhaseFailureAbortsBeforeSpawn(t *testing.T) {
	id := sid(341)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	control := &fakeControl{err: errors.New("kill failed")}
	spawner := &fakeSpawner{endpoint: "epX"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control), WithRunnerSpawner(spawner))

	registerLive(t, coord, "ep341")
	if _, err := coord.Restart(context.Background(), id); err == nil {
		t.Fatal("expected restart to fail in the stop phase")
	}
	if spawner.count() != 0 {
		t.Fatal("no spawn may happen after a failed stop phase")
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("live entry must survive")
	}
}

// TestRestartOfDeadSessionDegradesToResume verifies restart without a live
// generation performs a plain resume (no terminate).
func TestRestartOfDeadSessionDegradesToResume(t *testing.T) {
	id := sid(342)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	control := &fakeControl{}
	spawner := &fakeSpawner{endpoint: "ep342"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control), WithRunnerSpawner(spawner))

	rt, err := coord.Restart(context.Background(), id)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if control.count() != 0 || spawner.count() != 1 || rt.SessionID != id {
		t.Fatalf("terminate=%d spawn=%d rt=%+v", control.count(), spawner.count(), rt)
	}
}

// TestConcurrentStopAndRestartSerialize verifies the per-session claim covers
// heterogeneous lifecycle operations.
func TestConcurrentStopAndRestartSerialize(t *testing.T) {
	id := sid(343)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	entered := make(chan struct{})
	control := &fakeControl{entered: entered} // never kills; Stop blocks in wait
	spawner := &fakeSpawner{endpoint: "epX"}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control), WithRunnerSpawner(spawner))

	registerLive(t, coord, "ep343")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coord.Stop(ctx, id) }()
	<-entered // Stop holds the claim, blocked waiting for death

	if _, err := coord.Restart(context.Background(), id); !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("expected ErrLifecycleOpInFlight, got %v", err)
	}
	if _, err := coord.Resume(context.Background(), id); !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("expected ErrLifecycleOpInFlight, got %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v", err)
	}
	// Claim released: a new lifecycle op may start (fails on liveness, not on
	// the claim).
	if _, err := coord.Resume(context.Background(), id); !errors.Is(err, ErrSessionAlive) {
		t.Fatalf("expected ErrSessionAlive after release, got %v", err)
	}
}

// TestLifecycleOpsDifferentSessionsIndependent verifies claims are
// per-session.
func TestLifecycleOpsDifferentSessionsIndependent(t *testing.T) {
	id1, id2 := sid(344), sid(345)
	epClient := &endpointRunnerClient{routes: map[string]*fakeRunnerClient{
		"ep344": newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id1, Alive: true}}),
		"ep345": newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id2, Alive: true}}),
	}}
	dur := newFakeDurable(0)
	dur.session = func(id centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	entered := make(chan struct{})
	control := &fakeControl{entered: entered}
	spawner := &fakeSpawner{endpoint: "ep345"}
	coord := New(nil, epClient, dur, &fakeDirtySink{}, nil, WithRunnerControl(control), WithRunnerSpawner(spawner))

	registerLive(t, coord, "ep344")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- coord.Stop(ctx, id1) }()
	<-entered

	// A lifecycle op on another session proceeds while id1's claim is held.
	if _, err := coord.Resume(context.Background(), id2); err != nil {
		t.Fatalf("Resume of independent session: %v", err)
	}
	cancel()
	<-done
}

// ── ensureDurableExit / repair-vs-registration interleavings ────────────────

// TestEnsureDurableExitBlockedByRegistrationCommitWindow verifies the M1
// finding: a repair attempt must not stamp an exit onto a freshly committed,
// not-yet-installed live registration. RegisterRunner is parked between its
// commit signal and its return (mirroring TestReplacementFencesInFlightOldApply);
// because Register holds the lifecycle mutex across commit+install, the
// repair must not reach the durable at all while parked, and after install
// it must observe the live generation and return ErrStopSuperseded — never
// applying an exit.
func TestEnsureDurableExitBlockedByRegistrationCommitWindow(t *testing.T) {
	id := sid(360)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})

	registerCommitted := make(chan struct{})
	registerRelease := make(chan struct{})
	sessionCalls := make(chan struct{}, 8)
	var mu sync.Mutex
	var exitApplies []centralstore.RunnerObservation
	dur := &scheduledDurable{
		register: func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
			// Commit has happened; the registry entry is not installed yet.
			// Park inside the commit-to-install window (c.mu held by Register).
			close(registerCommitted)
			<-registerRelease
			return centralstore.Session{ID: id, Version: 7}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 7}, nil
		},
		apply: func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
			mu.Lock()
			exitApplies = append(exitApplies, obs)
			mu.Unlock()
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
		},
	}
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		sessionCalls <- struct{}{}
		// Exit-less committed row (the registration's own commit at v7).
		return centralstore.Session{ID: id, Version: 7, Adapter: "shell"}, true, nil
	}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, fixedClock(999))

	// Start the registration; it parks inside RegisterRunner holding c.mu.
	regDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep360"})
		regDone <- err
	}()
	<-registerCommitted

	// Start the repair (as Stop would after observing death). It must block
	// on c.mu: no Session read, no apply while the window is open.
	repairDone := make(chan error, 1)
	go func() { repairDone <- coord.ensureDurableExit(context.Background(), id) }()

	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-sessionCalls:
			t.Fatal("repair reached the durable inside the commit-to-install window")
		case err := <-repairDone:
			t.Fatalf("repair finished inside the window: %v", err)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Close the window: Register installs the live generation and unlocks.
	close(registerRelease)
	if err := <-regDone; err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := <-repairDone; !errors.Is(err, ErrStopSuperseded) {
		t.Fatalf("expected ErrStopSuperseded, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(exitApplies) != 0 {
		t.Fatalf("repair stamped an exit onto a live registration: %+v", exitApplies)
	}
}

// TestStopSupersededByReplacement verifies the L1 finding: when a Replace
// registration closes the awaited dead channel and a live replacement
// generation is installed at repair time, Stop must report ErrStopSuperseded,
// not plain success — the targeted process was never confirmed dead.
func TestStopSupersededByReplacement(t *testing.T) {
	id := sid(361)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	entered := make(chan struct{})
	control := &fakeControl{entered: entered} // Terminate delivered; runner ignores it
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, WithRunnerControl(control))

	registerLive(t, coord, "ep361")
	appliesBefore := len(dur.applied)

	done := make(chan error, 1)
	go func() { done <- coord.Stop(context.Background(), id) }()
	<-entered // Stop is waiting on the dead channel

	// A live replacement generation is installed while Stop waits (in
	// production this is a claimed Resume/Restart of a racing caller; a bare
	// Replace registration cannot pass the claim-token guard while Stop's
	// own claim is held, so the replacement is installed directly at the
	// registry seam). install closes the old dead channel.
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: id, Generation: 99, Endpoint: "ep361b", Subscribed: true}, dead: make(chan struct{})})

	if err := <-done; !errors.Is(err, ErrStopSuperseded) {
		t.Fatalf("expected ErrStopSuperseded, got %v", err)
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("replacement generation must stay installed")
	}
	dur.mu.Lock()
	defer dur.mu.Unlock()
	if len(dur.applied) != appliesBefore {
		t.Fatalf("no exit may be stamped onto the live replacement: %+v", dur.applied)
	}
}

// TestStopWaitsOutFenceWindow verifies the L2 finding: a Stop that observes a
// fenced (superseded) entry waits the fence window out instead of reporting
// ErrSessionNotAlive; when the window resolves by restore (failed
// replacement), the still-live old generation is a legitimate stop target.
func TestStopWaitsOutFenceWindow(t *testing.T) {
	id := sid(362)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return deadRow(id, 2), true, nil }
	control := &fakeControl{onKill: func(string) { client.stream.Close() }}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, WithRunnerControl(control), fixedClock(970))

	rt := registerLive(t, coord, "ep362")

	// Simulate a replacement's fence window: hold c.mu and supersede, exactly
	// as Register does before its RegisterRunner call.
	coord.mu.Lock()
	if !coord.registry.supersede(id, rt.Generation) {
		coord.mu.Unlock()
		t.Fatal("supersede failed")
	}

	done := make(chan error, 1)
	go func() { done <- coord.stopClaimed(context.Background(), id) }()

	// While the window is open the stop must not have terminated anything.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if control.count() != 0 {
			t.Fatal("terminate delivered inside the fence window")
		}
		select {
		case err := <-done:
			t.Fatalf("stop finished inside the fence window: %v", err)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	// The replacement fails: restore the old generation and end the window.
	coord.registry.restore(id, rt.Generation)
	coord.mu.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("stop after restored fence: %v", err)
	}
	if control.count() != 1 {
		t.Fatalf("expected 1 Terminate after the fence resolved, got %d", control.count())
	}
}

// TestConcurrentStopStopClaimCollision verifies two concurrent Stop calls on
// one session: the second fails ErrLifecycleOpInFlight, exactly one
// Terminate is delivered.
func TestConcurrentStopStopClaimCollision(t *testing.T) {
	id := sid(363)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	entered := make(chan struct{})
	control := &fakeControl{entered: entered} // never kills; first Stop blocks in wait
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control))

	registerLive(t, coord, "ep363")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coord.Stop(ctx, id) }()
	<-entered // first Stop holds the claim

	if err := coord.Stop(context.Background(), id); !errors.Is(err, ErrLifecycleOpInFlight) {
		t.Fatalf("expected ErrLifecycleOpInFlight, got %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Stop: %v", err)
	}
	if control.count() != 1 {
		t.Fatalf("expected exactly 1 Terminate, got %d", control.count())
	}
}

// TestEnsureDurableExitSessionNotFoundIsBenign verifies BUG-1: a row deleted
// between the read and the conditional apply is a no-op, not an error.
func TestEnsureDurableExitSessionNotFoundIsBenign(t *testing.T) {
	id := sid(364)
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, Version: 4, Adapter: "shell"}, true, nil // exit-less
	}
	dur.applyResult = func(centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{}, centralstore.ErrSessionNotFound
	}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, fixedClock(980))
	if err := coord.ensureDurableExit(context.Background(), id); err != nil {
		t.Fatalf("ErrSessionNotFound must be benign, got %v", err)
	}

	// Row absent at the read is equally benign.
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{}, false, nil
	}
	if err := coord.ensureDurableExit(context.Background(), id); err != nil {
		t.Fatalf("absent row must be benign, got %v", err)
	}
}

// TestEnsureDurableExitStaleUsesResponseVersion verifies BUG-2: on
// ErrStaleVersion the retry uses the stale response's SessionVersion directly
// instead of relying on the re-read alone.
func TestEnsureDurableExitStaleUsesResponseVersion(t *testing.T) {
	id := sid(365)
	dur := newFakeDurable(0)
	// The read is stuck at an old version (simulating read-to-apply churn the
	// re-read alone would keep losing to).
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, Version: 1, Adapter: "shell"}, true, nil
	}
	var versions []centralstore.RowVersion
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		versions = append(versions, obs.ObservedVersion)
		if len(versions) == 1 {
			return centralstore.MutationResult{SessionVersion: 9}, centralstore.ErrStaleVersion
		}
		if obs.ObservedVersion != 9 {
			return centralstore.MutationResult{SessionVersion: 9}, centralstore.ErrStaleVersion
		}
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 10}, nil
	}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, fixedClock(981))
	if err := coord.ensureDurableExit(context.Background(), id); err != nil {
		t.Fatalf("ensureDurableExit: %v", err)
	}
	if len(versions) != 2 || versions[1] != 9 {
		t.Fatalf("retry must use the stale response's version directly: %v", versions)
	}
}

// TestEnsureDurableExitStaleExhaustion verifies the bounded retry budget:
// three consecutive stale responses yield ErrExitNotDurable.
func TestEnsureDurableExitStaleExhaustion(t *testing.T) {
	id := sid(366)
	dur := newFakeDurable(0)
	version := centralstore.RowVersion(1)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: id, Version: version, Adapter: "shell"}, true, nil
	}
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		version = obs.ObservedVersion + 1 // perpetual churn
		return centralstore.MutationResult{SessionVersion: version}, centralstore.ErrStaleVersion
	}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, fixedClock(982))
	if err := coord.ensureDurableExit(context.Background(), id); !errors.Is(err, ErrExitNotDurable) {
		t.Fatalf("expected ErrExitNotDurable, got %v", err)
	}
}

// switchingClient returns successive streams for successive Subscribe calls
// (used to give a restart's replacement its own stream).
type switchingClient struct {
	meta    RunnerMeta
	streams []*fakeStream
	mu      *sync.Mutex
	next    int
}

func (c *switchingClient) Subscribe(_ context.Context, _ string) (EventStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streams[c.next]
	if c.next < len(c.streams)-1 {
		c.next++
	}
	return s, nil
}
func (c *switchingClient) Meta(context.Context, string) (RunnerMeta, error) { return c.meta, nil }
