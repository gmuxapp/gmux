package sessioncoord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// checkingSpawner is a spawner that answers the pre-stop question the same way
// it answers the spawn: it refuses any row carrying a conversation ref it
// cannot resolve. That is the production shape (productionRunnerSpawner.CanSpawn
// and Spawn share one resolver), reduced to what the ordering tests need.
type checkingSpawner struct {
	mu       sync.Mutex
	calls    []centralstore.Session
	endpoint string
	// refuse reports whether the spawner would decline this row.
	refuse func(centralstore.Session) bool
}

func (s *checkingSpawner) refuses(row centralstore.Session) bool {
	return s.refuse != nil && s.refuse(row)
}

func (s *checkingSpawner) CanSpawn(row centralstore.Session) error {
	if s.refuses(row) {
		return errors.New("cannot be resumed: conversation not resolvable")
	}
	return nil
}

func (s *checkingSpawner) Spawn(_ context.Context, row centralstore.Session) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, row)
	s.mu.Unlock()
	if s.refuses(row) {
		return "", errors.New("cannot be resumed: conversation not resolvable")
	}
	return s.endpoint, nil
}

func (s *checkingSpawner) spawned() []centralstore.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]centralstore.Session(nil), s.calls...)
}

// TestRestartRefusesBeforeStoppingWhenTheSpawnerWouldRefuse pins the ordering,
// not the value: a row the spawner declines must keep its running process.
// Moving the check after the stop (or after Restart's spawn) fails here,
// because the terminate would have been issued and the live entry lost.
func TestRestartRefusesBeforeStoppingWhenTheSpawnerWouldRefuse(t *testing.T) {
	id := sid(360)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	live := centralstore.Session{ID: id, Version: 2, Adapter: "claude", Command: []string{"claude"}, ConversationRef: "/gone.jsonl"}
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) { return live, true, nil }
	// Errors if called at all, so a check moved after the stop surfaces as a
	// terminate failure instead of blocking on a death this fake never reports.
	control := &fakeControl{err: errors.New("terminate must not be issued for a refused restart")}
	spawner := &checkingSpawner{endpoint: "ep360", refuse: func(row centralstore.Session) bool { return row.ConversationRef != "" }}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control), WithRunnerSpawner(spawner))

	registerLive(t, coord, "ep360")
	_, err := coord.Restart(context.Background(), id)
	if err == nil {
		t.Fatal("restart of a row the spawner refuses must fail")
	}
	if !strings.Contains(err.Error(), "session left running") {
		t.Errorf("error %q must say the session was left running", err)
	}
	if control.count() != 0 {
		t.Errorf("terminate issued %d time(s): restart stopped a session it could not respawn", control.count())
	}
	if n := len(spawner.spawned()); n != 0 {
		t.Errorf("spawn attempted %d time(s) after a refusal", n)
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("the live runner must survive a refused restart")
	}
}

// TestRestartSpawnsTheRowItAuthorized is the F3 race, made deterministic: a
// runner fact (a first-bind conversation_file) lands between the pre-stop
// decision and the spawn. Fact application does not take the lifecycle claim,
// so the store row genuinely changes underneath the operation. The spawn must
// use the row the restart was authorized against, or restart becomes a kill
// that cannot come back.
func TestRestartSpawnsTheRowItAuthorized(t *testing.T) {
	id := sid(361)
	client := newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}})
	dur := newFakeDurable(0)
	authorized := centralstore.Session{ID: id, Version: 2, Adapter: "claude", Command: []string{"claude"}}
	var reads int
	var mu sync.Mutex
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads == 1 {
			return authorized, true, nil // pre-stop: no conversation bound yet
		}
		// Post-stop re-read: the hook bound a transcript the adapter cannot
		// parse yet, which flips the policy branch from rerun to resume.
		rebound := authorized
		rebound.ConversationRef = "/fresh-unparseable.jsonl"
		return rebound, true, nil
	}
	control := &fakeControl{}
	spawner := &checkingSpawner{endpoint: "ep361", refuse: func(row centralstore.Session) bool { return row.ConversationRef != "" }}
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithRunnerControl(control), WithRunnerSpawner(spawner))

	if _, err := coord.Restart(context.Background(), id); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	calls := spawner.spawned()
	if len(calls) != 1 {
		t.Fatalf("spawns = %d, want 1", len(calls))
	}
	if calls[0].ConversationRef != "" {
		t.Errorf("spawned with the re-read row (ref %q); the authorized row had none",
			calls[0].ConversationRef)
	}
	if reads < 2 {
		t.Fatalf("the post-stop re-read never happened (reads=%d); the test no longer exercises the race", reads)
	}
}
