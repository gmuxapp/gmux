package sessioncoord

import (
	"context"
	"errors"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func ackCoord(dur *fakeDurable, sink *fakeDirtySink) *Coordinator {
	return New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil)
}

func TestAcknowledgeDeadCommitsAndPublishes(t *testing.T) {
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "1mw5c5n9", Version: 5, Unread: true}, true, nil
	}
	sink := &fakeDirtySink{}
	coord := ackCoord(dur, sink)

	if err := coord.AcknowledgeDead(context.Background(), "1mw5c5n9"); err != nil {
		t.Fatal(err)
	}
	if len(dur.ackCalls) != 1 || dur.ackCalls[0] != 5 {
		t.Fatalf("ackCalls=%v, want [5]", dur.ackCalls)
	}
	if sink.count() != 1 {
		t.Fatalf("published %d outcomes, want 1", sink.count())
	}
}

func TestAcknowledgeDeadLiveTargetIsSilentNoOp(t *testing.T) {
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := ackCoord(dur, sink)
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "1mw5c5n9", Generation: 1}, dead: make(chan struct{})})

	if err := coord.AcknowledgeDead(context.Background(), "1mw5c5n9"); err != nil {
		t.Fatalf("live target must be a silent no-op: %v", err)
	}
	if len(dur.ackCalls) != 0 {
		t.Fatal("live target must not write")
	}
	if sink.count() != 0 {
		t.Fatal("no-op must publish nothing")
	}
}

func TestAcknowledgeDeadTokenNeverSucceedsForLiveOrFencedOwner(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "fenced"}[fenced], func(t *testing.T) {
			dur := newFakeDurable(0)
			coord := ackCoord(dur, &fakeDirtySink{})
			entry := registryEntry{Runtime: Runtime{SessionID: "1mw5c5n9", Generation: 1}, dead: make(chan struct{})}
			coord.registry.install(entry)
			if fenced && !coord.registry.supersede(entry.SessionID, entry.Generation) {
				t.Fatal("failed to establish replacement fence")
			}
			if err := coord.AcknowledgeDeadToken(context.Background(), entry.SessionID, "result-1"); !errors.Is(err, ErrAckOwnerChanged) {
				t.Fatalf("token acknowledgement error=%v, want ErrAckOwnerChanged", err)
			}
			if len(dur.ackCalls) != 0 {
				t.Fatal("runner-owned result was written through the durable path")
			}
		})
	}
}

func TestAcknowledgementRuntimeWaitsForReplacementInstall(t *testing.T) {
	coord := ackCoord(newFakeDurable(0), &fakeDirtySink{})
	id := centralstore.SessionID("1mw5c5n9")
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: id, Generation: 1, Endpoint: "old", Incarnation: "old-inc"}, dead: make(chan struct{})})

	// Reproduce Register's commit-to-install window exactly: c.mu remains held
	// while the old generation is fenced and until generation 2 is installed.
	coord.mu.Lock()
	if !coord.registry.supersede(id, 1) {
		coord.mu.Unlock()
		t.Fatal("failed to establish replacement fence")
	}
	started := make(chan struct{})
	resolved := make(chan Runtime, 1)
	go func() {
		close(started)
		runtime, _ := coord.AcknowledgementRuntime(id)
		resolved <- runtime
	}()
	<-started
	select {
	case got := <-resolved:
		coord.mu.Unlock()
		t.Fatalf("owner resolved inside commit-to-install: %+v", got)
	default:
	}
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: id, Generation: 2, Endpoint: "new", Incarnation: "new-inc"}, dead: make(chan struct{})})
	coord.mu.Unlock()

	if got := <-resolved; got.Generation != 2 || got.Endpoint != "new" || got.Incarnation != "new-inc" {
		t.Fatalf("resolved owner=%+v, want installed generation 2", got)
	}
}

func TestAcknowledgeDeadAlreadyClearSkipsWrite(t *testing.T) {
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "1mw5c5n9", Version: 5}, true, nil
	}
	coord := ackCoord(dur, &fakeDirtySink{})
	if err := coord.AcknowledgeDead(context.Background(), "1mw5c5n9"); err != nil {
		t.Fatal(err)
	}
	if len(dur.ackCalls) != 0 {
		t.Fatal("already-clear row must not write")
	}
}

func TestAcknowledgeDeadNotFound(t *testing.T) {
	dur := newFakeDurable(0)
	coord := ackCoord(dur, &fakeDirtySink{})
	if err := coord.AcknowledgeDead(context.Background(), "1mw5c5n9"); !errors.Is(err, centralstore.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestAcknowledgeDeadStaleRetryAndExhaustion(t *testing.T) {
	// Retry: two stale responses, then success.
	dur := newFakeDurable(0)
	version := centralstore.RowVersion(5)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "1mw5c5n9", Version: version, Unread: true}, true, nil
	}
	calls := 0
	dur.ackResult = func(_ centralstore.SessionID, observed centralstore.RowVersion) (centralstore.MutationResult, error) {
		calls++
		if calls < 3 {
			version++
			return centralstore.MutationResult{SessionVersion: version}, centralstore.ErrStaleVersion
		}
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: observed + 1}, nil
	}
	sink := &fakeDirtySink{}
	coord := ackCoord(dur, sink)
	if err := coord.AcknowledgeDead(context.Background(), "1mw5c5n9"); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if calls != 3 || sink.count() != 1 {
		t.Fatalf("calls=%d published=%d", calls, sink.count())
	}

	// Exhaustion: permanently stale.
	dur2 := newFakeDurable(0)
	v2 := centralstore.RowVersion(5)
	dur2.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "1mw5c5n9", Version: v2, Unread: true}, true, nil
	}
	dur2.ackResult = func(_ centralstore.SessionID, _ centralstore.RowVersion) (centralstore.MutationResult, error) {
		v2++
		return centralstore.MutationResult{SessionVersion: v2}, centralstore.ErrStaleVersion
	}
	coord2 := ackCoord(dur2, &fakeDirtySink{})
	if err := coord2.AcknowledgeDead(context.Background(), "1mw5c5n9"); !errors.Is(err, ErrAckNotDurable) {
		t.Fatalf("expected ErrAckNotDurable, got %v", err)
	}
}
