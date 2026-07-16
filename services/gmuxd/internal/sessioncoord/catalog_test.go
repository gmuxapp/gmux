package sessioncoord

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func TestReplaceCatalogCommitsRematchAndAutoAssignsLive(t *testing.T) {
	dur := newFakeDurable(0)
	peerInputs := []centralstore.LocalPeerMatchInput{{Subject: centralstore.LocalPeerSubject{PeerKey: "peer1", SessionID: "sess-remote"}, CWD: "/r"}}
	dur.placeUnplacedResult = func(ids []centralstore.SessionID, at centralstore.UnixMillis) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	sink := &fakeDirtySink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil,
		WithClock(func() centralstore.UnixMillis { return 42 }),
		WithLocalPeerMatchInputs(func() []centralstore.LocalPeerMatchInput { return peerInputs }))

	// Two live sessions in the registry; the auto-assign pass must offer
	// exactly these (the store skips placed/dismissed/non-matching itself).
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "sess-a", Generation: 1}, dead: make(chan struct{})})
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "sess-b", Generation: 2}, dead: make(chan struct{})})

	if _, err := coord.ReplaceCatalog(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(dur.replaceCatalogCalls) != 1 || !reflect.DeepEqual(dur.replaceCatalogCalls[0], peerInputs) {
		t.Fatalf("replace calls=%v", dur.replaceCatalogCalls)
	}
	if len(dur.placeUnplacedCalls) != 1 || !reflect.DeepEqual(dur.placeUnplacedCalls[0], []centralstore.SessionID{"sess-a", "sess-b"}) {
		t.Fatalf("auto-assign candidates=%v", dur.placeUnplacedCalls)
	}
	// One combined invalidation for the whole operation.
	if sink.count() != 1 {
		t.Fatalf("published %d outcomes, want 1 combined", sink.count())
	}
}

func TestReplaceCatalogSkipsAutoAssignWithoutLiveSessions(t *testing.T) {
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil)

	if _, err := coord.ReplaceCatalog(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(dur.placeUnplacedCalls) != 0 {
		t.Fatal("no live sessions: auto-assign must not run")
	}
	if sink.count() != 1 {
		t.Fatalf("published %d outcomes, want 1", sink.count())
	}
}

func TestReplaceCatalogFailurePublishesNothing(t *testing.T) {
	dur := newFakeDurable(0)
	boom := errors.New("replace failed")
	dur.replaceCatalogResult = func([]centralstore.ProjectEntrySpec, []centralstore.LocalPeerMatchInput, centralstore.UnixMillis) (centralstore.ProjectCatalog, centralstore.MutationResult, error) {
		return nil, centralstore.MutationResult{}, boom
	}
	sink := &fakeDirtySink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil)

	if _, err := coord.ReplaceCatalog(context.Background(), nil); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if sink.count() != 0 || len(dur.placeUnplacedCalls) != 0 {
		t.Fatalf("failed replace must publish nothing and skip auto-assign: published=%d placed=%d", sink.count(), len(dur.placeUnplacedCalls))
	}
}

func TestReplaceCatalogAutoAssignFailureStillPublishesReplace(t *testing.T) {
	dur := newFakeDurable(0)
	boom := errors.New("place failed")
	dur.placeUnplacedResult = func([]centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{}, boom
	}
	sink := &fakeDirtySink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil)
	coord.registry.install(registryEntry{Runtime: Runtime{SessionID: "sess-a", Generation: 1}, dead: make(chan struct{})})

	catalog, err := coord.ReplaceCatalog(context.Background(), nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if catalog == nil {
		t.Fatal("committed catalog must be returned despite the auto-assign failure")
	}
	if sink.count() != 1 {
		t.Fatalf("committed replace must still publish: %d", sink.count())
	}
}
