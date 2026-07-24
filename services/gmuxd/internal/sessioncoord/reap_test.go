package sessioncoord

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// multiClient serves per-endpoint metas; unknown endpoints fail Meta.
type multiClient struct {
	mu    sync.Mutex
	metas map[string]RunnerMeta
}

func (c *multiClient) Subscribe(context.Context, string) (EventStream, error) {
	return newFakeStream(), nil
}

func (c *multiClient) Meta(_ context.Context, endpoint string) (RunnerMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.metas[endpoint]
	if !ok {
		return RunnerMeta{}, errors.New("unreachable")
	}
	return m, nil
}

func reapCoord(t *testing.T, client RunnerClient, control *fakeControl) *Coordinator {
	t.Helper()
	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, &fakeErrorSink{}, WithRunnerControl(control))
	closeBarrier(t, coord)
	return coord
}

func TestReapOrphansTerminatesSupersededDuplicate(t *testing.T) {
	id := sid(1)
	client := &multiClient{metas: map[string]RunnerMeta{
		"ep-orphan": {Registration: centralstore.RunnerRegistration{ID: id, Adapter: "pi", Alive: true}},
	}}
	control := &fakeControl{}
	coord := reapCoord(t, client, control)
	installLive(coord, id, "ep-winner") // the resume winner, different endpoint

	reaped, err := coord.ReapOrphans(context.Background(), []string{"ep-orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0] != "ep-orphan" {
		t.Fatalf("reaped=%v", reaped)
	}
	if control.count() != 1 {
		t.Fatalf("terminates=%d", control.count())
	}
	// The winner's generation is untouched.
	if e, live := coord.registry.current(id); !live || e.Endpoint != "ep-winner" {
		t.Fatalf("winner entry=%#v live=%v", e, live)
	}
	// The reap claim was released.
	coord.mu.Lock()
	_, busy := coord.ops[id]
	coord.mu.Unlock()
	if busy {
		t.Fatal("claim leaked")
	}
}

func TestReapOrphansNeverKillsTheRegisteredEndpointOrUnclaimedRunners(t *testing.T) {
	registered := sid(2)
	unclaimed := sid(3)
	client := &multiClient{metas: map[string]RunnerMeta{
		"ep-registered": {Registration: centralstore.RunnerRegistration{ID: registered, Adapter: "pi", Alive: true}},
		"ep-unclaimed":  {Registration: centralstore.RunnerRegistration{ID: unclaimed, Adapter: "pi", Alive: true}},
		"ep-anonymous":  {},
	}}
	control := &fakeControl{}
	coord := reapCoord(t, client, control)
	installLive(coord, registered, "ep-registered")

	reaped, err := coord.ReapOrphans(context.Background(),
		[]string{"ep-registered", "ep-unclaimed", "ep-anonymous", "ep-unreachable"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 0 || control.count() != 0 {
		t.Fatalf("reaped=%v terminates=%d", reaped, control.count())
	}
}

func TestReapOrphansSkipsClaimedSession(t *testing.T) {
	id := sid(4)
	client := &multiClient{metas: map[string]RunnerMeta{
		"ep-orphan": {Registration: centralstore.RunnerRegistration{ID: id, Adapter: "pi", Alive: true}},
	}}
	control := &fakeControl{}
	coord := reapCoord(t, client, control)
	installLive(coord, id, "ep-winner")
	coord.mu.Lock()
	coord.ops[id] = &LifecycleClaim{op: "restart"}
	coord.mu.Unlock()

	reaped, err := coord.ReapOrphans(context.Background(), []string{"ep-orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 0 || control.count() != 0 {
		t.Fatal("a claimed session must never be reaped")
	}
}

func TestReapOrphansTerminateFailureIsReportedAndSkipped(t *testing.T) {
	id := sid(5)
	client := &multiClient{metas: map[string]RunnerMeta{
		"ep-orphan": {Registration: centralstore.RunnerRegistration{ID: id, Adapter: "pi", Alive: true}},
	}}
	control := &fakeControl{err: errors.New("kill failed")}
	errs := &fakeErrorSink{}
	coord := New(nil, client, newFakeDurable(0), &fakeDirtySink{}, errs, WithRunnerControl(control))
	closeBarrier(t, coord)
	installLive(coord, id, "ep-winner")

	reaped, err := coord.ReapOrphans(context.Background(), []string{"ep-orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped=%v", reaped)
	}
	if errs.count() != 1 {
		t.Fatalf("errors=%d", errs.count())
	}
	coord.mu.Lock()
	_, busy := coord.ops[id]
	coord.mu.Unlock()
	if busy {
		t.Fatal("claim leaked after failure")
	}
}

func TestReapOrphansPreconditions(t *testing.T) {
	coord := New(nil, &multiClient{}, newFakeDurable(0), nil, nil)
	if _, err := coord.ReapOrphans(context.Background(), nil); !errors.Is(err, ErrNoRunnerControl) {
		t.Fatalf("no control: %v", err)
	}
	coord = New(nil, &multiClient{}, newFakeDurable(0), nil, nil, WithRunnerControl(&fakeControl{}))
	if _, err := coord.ReapOrphans(context.Background(), nil); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("open barrier: %v", err)
	}
}
