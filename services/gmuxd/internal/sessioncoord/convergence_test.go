package sessioncoord

import (
	"context"
	"errors"
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
			exitedNilSession("sess-alive-unknown", 3),
			exitedSession("sess-already-dead", 7),
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
	if len(got) != 1 || got[0] != "sess-alive-unknown" {
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
		return []centralstore.Session{exitedNilSession(id, 1), exitedNilSession("sess-gone", 2)}, nil
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
	if len(got) != 1 || got[0] != "sess-gone" {
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
		return []centralstore.Session{exitedNilSession("sess-x", 4)}, nil
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
	if len(durable.swept) != 2 || len(durable.swept[1]) != 1 || durable.swept[1][0] != "sess-x" {
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
