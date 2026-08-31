package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

type recordingCoordErrors struct {
	mu  sync.Mutex
	err []error
}

func (s *recordingCoordErrors) Error(_ context.Context, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = append(s.err, err)
}
func (s *recordingCoordErrors) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.err) }
func (s *recordingCoordErrors) last() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.err) == 0 {
		return nil
	}
	return s.err[len(s.err)-1]
}

func TestComposedSupervisedExitReachesNotificationRouter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fleet := newHarnessFleet(0)
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	parentEP, childEP := "parent-runner", "child-runner"
	parentInc, childInc := "parent-inc", "child-inc"
	fleet.metas[parentEP] = sessioncoord.RunnerMeta{Incarnation: parentInc, Registration: centralstore.RunnerRegistration{ID: parent, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1}}
	fleet.metas[childEP] = sessioncoord.RunnerMeta{Incarnation: childInc, Registration: centralstore.RunnerRegistration{ID: child, Adapter: "shell", Alive: true, CreatedAt: 2, ObservedAt: 2, ParentSessionID: &parent}}
	fleet.streams[parentEP] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: parentInc}
	fleet.streams[childEP] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: childInc}
	errs := &recordingCoordErrors{}
	coord := sessioncoord.New(nil, fleet, store, nil, errs, sessioncoord.WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
	defer coord.Close()
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: parentEP, AssertedID: parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: childEP, AssertedID: child}); err != nil {
		t.Fatal(err)
	}

	seed, outcomes, unsubscribe, err := coord.SubscribeOutcomesSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	router := newCentralNotifyRouter(presence.New(presence.Callbacks{}), notifyConfig{GracePeriod: time.Hour, IdleThreshold: time.Minute})
	defer router.CancelAllPending()
	go router.Run(ctx, seed, outcomes)
	router.scheduleNotification(string(child), "unread", "Child", "New output", "shell", "result-1")

	fleet.streams[childEP].events <- successfulExitEventForRouter()
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, ok, readErr := store.Session(ctx, child)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if ok && row.ExitedAt != nil && !row.Unread && !hasPendingNotification(router, string(child)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervised outcome did not reach router: row=%+v pending=%v lastErr=%v", row, hasPendingNotification(router, string(child)), errs.last())
		}
		time.Sleep(time.Millisecond)
	}
	if errs.count() != 0 {
		t.Fatalf("supervised exit reported error: %v", errs.last())
	}
}

func successfulExitEventForRouter() sessioncoord.RunnerEvent {
	active, hasError, interrupted := false, false, false
	unread, token, at, code, alive := true, "result-1", centralstore.UnixMillis(20), 0, false
	return sessioncoord.RunnerEvent{ObservedAt: 20, Alive: &alive, Facts: centralstore.RunnerFacts{
		Active: &active, Error: &hasError, Interrupted: &interrupted,
		Unread: &unread, UnreadToken: &token,
		ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &at}, ExitCode: centralstore.NullablePatch[int]{Set: &code},
	}}
}

func TestBootstrapNilSemanticClassifierDisablesAutoRead(t *testing.T) {
	fleet := newHarnessFleet(0)
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	fleet.metas["parent"] = sessioncoord.RunnerMeta{Incarnation: "p-inc", Registration: centralstore.RunnerRegistration{ID: parent, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1}}
	fleet.metas["child"] = sessioncoord.RunnerMeta{Incarnation: "c-inc", Registration: centralstore.RunnerRegistration{ID: child, Adapter: "agent", Alive: true, CreatedAt: 2, ObservedAt: 2, ParentSessionID: &parent}}
	fleet.streams["parent"] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: "p-inc"}
	fleet.streams["child"] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: "c-inc"}
	store, boot := openHarness(t, t.TempDir(), fleet, nil) // SemanticAgent intentionally nil.
	defer store.Close()
	defer boot.Close()
	ctx := context.Background()
	if _, err := boot.Coordinator.Register(ctx, sessioncoord.RegisterRequest{Endpoint: "parent", AssertedID: parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := boot.Coordinator.Register(ctx, sessioncoord.RegisterRequest{Endpoint: "child", AssertedID: child}); err != nil {
		t.Fatal(err)
	}
	fleet.streams["child"].events <- successfulExitEventForRouter()
	deadline := time.Now().Add(time.Second)
	for {
		row, ok, err := store.Session(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		if ok && row.ExitedAt != nil {
			if !row.Unread {
				t.Fatal("nil classifier enabled auto-read and classified agent as process")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("child exit did not commit")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestComposedFastDeadRegistrationAutoReadsAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fleet := newHarnessFleet(0)
	parent, child := centralstore.SessionID("parent01"), centralstore.SessionID("child001")
	fleet.metas["parent"] = sessioncoord.RunnerMeta{Incarnation: "p-inc", Registration: centralstore.RunnerRegistration{ID: parent, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1}}
	fleet.streams["parent"] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: "p-inc"}
	unread, token, active, hasError, interrupted := true, "fast-result", false, false, false
	code, exited := 0, centralstore.UnixMillis(20)
	fleet.metas["child"] = sessioncoord.RunnerMeta{Incarnation: "c-inc", Registration: centralstore.RunnerRegistration{ID: child, Adapter: "shell", Alive: false, CreatedAt: 2, ObservedAt: 20, ParentSessionID: &parent, Facts: centralstore.RunnerFacts{
		Active: &active, Error: &hasError, Interrupted: &interrupted, Unread: &unread, UnreadToken: &token,
		ExitCode: centralstore.NullablePatch[int]{Set: &code}, ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited},
	}}}
	fleet.streams["child"] = &harnessStream{events: make(chan sessioncoord.RunnerEvent, 8), incarnation: "c-inc"}
	coord := sessioncoord.New(nil, fleet, store, nil, nil, sessioncoord.WithProcessChildAutoRead(func(centralstore.Session) bool { return false }))
	defer coord.Close()
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: "parent", AssertedID: parent}); err != nil {
		t.Fatal(err)
	}
	seed, outcomes, unsubscribe, err := coord.SubscribeOutcomesSeed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	router := newCentralNotifyRouter(presence.New(presence.Callbacks{}), notifyConfig{GracePeriod: time.Hour, IdleThreshold: time.Minute})
	defer router.CancelAllPending()
	go router.Run(ctx, seed, outcomes)
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: "child", AssertedID: child}); err != nil {
		t.Fatal(err)
	}
	row, ok, err := store.Session(ctx, child)
	if err != nil || !ok {
		t.Fatalf("child row: ok=%v err=%v", ok, err)
	}
	if row.Unread || row.UnreadToken != token || row.ExitCode == nil || *row.ExitCode != 0 || row.LastActivityAt == nil {
		t.Fatalf("fast-dead registration was not atomically supervised: %+v", row)
	}
	if hasPendingNotification(router, string(child)) {
		t.Fatal("fast-dead supervised registration leaked attention")
	}
}
