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
		return centralstore.Session{ID: "sess-a", Version: 5, Unread: true}, true, nil
	}
	sink := &fakeDirtySink{}
	coord := ackCoord(dur, sink)

	if err := coord.AcknowledgeDead(context.Background(), "sess-a"); err != nil {
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
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "sess-a", Generation: 1}, dead: make(chan struct{})})

	if err := coord.AcknowledgeDead(context.Background(), "sess-a"); err != nil {
		t.Fatalf("live target must be a silent no-op: %v", err)
	}
	if len(dur.ackCalls) != 0 {
		t.Fatal("live target must not write")
	}
	if sink.count() != 0 {
		t.Fatal("no-op must publish nothing")
	}
}

func TestAcknowledgeDeadAlreadyClearSkipsWrite(t *testing.T) {
	dur := newFakeDurable(0)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "sess-a", Version: 5}, true, nil
	}
	coord := ackCoord(dur, &fakeDirtySink{})
	if err := coord.AcknowledgeDead(context.Background(), "sess-a"); err != nil {
		t.Fatal(err)
	}
	if len(dur.ackCalls) != 0 {
		t.Fatal("already-clear row must not write")
	}
}

func TestAcknowledgeDeadNotFound(t *testing.T) {
	dur := newFakeDurable(0)
	coord := ackCoord(dur, &fakeDirtySink{})
	if err := coord.AcknowledgeDead(context.Background(), "sess-a"); !errors.Is(err, centralstore.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestAcknowledgeDeadStaleRetryAndExhaustion(t *testing.T) {
	// Retry: two stale responses, then success.
	dur := newFakeDurable(0)
	version := centralstore.RowVersion(5)
	dur.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "sess-a", Version: version, Unread: true}, true, nil
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
	if err := coord.AcknowledgeDead(context.Background(), "sess-a"); err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	if calls != 3 || sink.count() != 1 {
		t.Fatalf("calls=%d published=%d", calls, sink.count())
	}

	// Exhaustion: permanently stale.
	dur2 := newFakeDurable(0)
	v2 := centralstore.RowVersion(5)
	dur2.session = func(centralstore.SessionID) (centralstore.Session, bool, error) {
		return centralstore.Session{ID: "sess-a", Version: v2, Unread: true}, true, nil
	}
	dur2.ackResult = func(_ centralstore.SessionID, _ centralstore.RowVersion) (centralstore.MutationResult, error) {
		v2++
		return centralstore.MutationResult{SessionVersion: v2}, centralstore.ErrStaleVersion
	}
	coord2 := ackCoord(dur2, &fakeDirtySink{})
	if err := coord2.AcknowledgeDead(context.Background(), "sess-a"); !errors.Is(err, ErrAckNotDurable) {
		t.Fatalf("expected ErrAckNotDurable, got %v", err)
	}
}
