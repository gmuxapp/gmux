package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ── fake helpers ─────────────────────────────────────────────────────────────

// fakeStream is a controllable EventStream.
type fakeStream struct {
	ch     chan RunnerEvent
	closed atomic.Bool
}

func newFakeStream() *fakeStream                 { return &fakeStream{ch: make(chan RunnerEvent, 64)} }
func (s *fakeStream) Events() <-chan RunnerEvent { return s.ch }
func (s *fakeStream) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.ch)
	}
	return nil
}
func (s *fakeStream) send(ev RunnerEvent) { s.ch <- ev }
func (s *fakeStream) close()              { s.Close() } //nolint:unused

// fakeRunnerClient returns a fixed stream and meta. subscribeBlock/metaBlock
// let tests inject delays to verify lock-free I/O.
type fakeRunnerClient struct {
	mu           sync.Mutex
	stream       *fakeStream
	meta         RunnerMeta
	subscribeErr error
	metaErr      error
	// subscribeBlock is closed by the test when Subscribe may proceed.
	subscribeBlock chan struct{}
	// metaBlock is closed by the test when Meta may proceed.
	metaBlock chan struct{}
}

func newFakeClient(meta RunnerMeta) *fakeRunnerClient {
	return &fakeRunnerClient{stream: newFakeStream(), meta: meta}
}

func (c *fakeRunnerClient) Subscribe(ctx context.Context, _ string) (EventStream, error) {
	if c.subscribeBlock != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.subscribeBlock:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subscribeErr != nil {
		return nil, c.subscribeErr
	}
	return c.stream, nil
}

func (c *fakeRunnerClient) Meta(ctx context.Context, _ string) (RunnerMeta, error) {
	if c.metaBlock != nil {
		select {
		case <-ctx.Done():
			return RunnerMeta{}, ctx.Err()
		case <-c.metaBlock:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metaErr != nil {
		return RunnerMeta{}, c.metaErr
	}
	return c.meta, nil
}

// fakeDurable records calls and can simulate errors or stale-version
// conditions.
type fakeDurable struct {
	mu sync.Mutex
	// registerResult is returned by RegisterRunner.
	registerResult func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error)
	// applyResult is called per ApplyRunnerObservation.
	applyResult func(centralstore.RunnerObservation) (centralstore.MutationResult, error)
	// registered records all RegisterRunner calls.
	registered []centralstore.RunnerRegistration
	// applied records all ApplyRunnerObservation calls.
	applied []centralstore.RunnerObservation

	// session backs Session for lifecycle tests.
	session func(centralstore.SessionID) (centralstore.Session, bool, error)
	// listSessions backs ListSessions for convergence tests.
	listSessions func() ([]centralstore.Session, error)
	// sweepResult backs SweepDeadSessions; swept records its calls.
	sweepResult func([]centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error)
	swept       [][]centralstore.SessionID
	// dismissResult backs DismissSessionTree; dismissCalls records its calls.
	dismissResult func(centralstore.SessionID, centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error)
	dismissCalls  []centralstore.SessionID
	// removeResult backs RemoveSessionAtVersion; removeCalls records its calls.
	removeResult func(centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error)
	removeCalls  []centralstore.SessionID
	// ackResult backs AcknowledgeDeadSession; ackCalls records its calls.
	ackResult func(centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error)
	ackCalls  []centralstore.RowVersion
	// replaceCatalogResult backs ReplaceProjectCatalogAndRematch;
	// replaceCatalogCalls records the peer inputs of each call.
	replaceCatalogResult func([]centralstore.ProjectEntrySpec, []centralstore.LocalPeerMatchInput, centralstore.UnixMillis) (centralstore.ProjectCatalog, centralstore.MutationResult, error)
	replaceCatalogCalls  [][]centralstore.LocalPeerMatchInput
	// placeUnplacedResult backs PlaceUnplacedSessions; placeUnplacedCalls
	// records the candidate sets of each call.
	placeUnplacedResult func([]centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error)
	placeUnplacedCalls  [][]centralstore.SessionID
}

func newFakeDurable(version centralstore.RowVersion) *fakeDurable {
	v := version
	return &fakeDurable{
		registerResult: func(reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
			v++
			return centralstore.Session{Version: v}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: v}, nil
		},
		applyResult: func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
			if obs.ObservedVersion != v {
				cur := v
				return centralstore.MutationResult{SessionVersion: cur}, centralstore.ErrStaleVersion
			}
			v++
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: v}, nil
		},
	}
}

func (d *fakeDurable) RegisterRunner(ctx context.Context, reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.registered = append(d.registered, reg)
	return d.registerResult(reg)
}

func (d *fakeDurable) ApplyRunnerObservation(ctx context.Context, obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applied = append(d.applied, obs)
	return d.applyResult(obs)
}

func (d *fakeDurable) Session(ctx context.Context, id centralstore.SessionID) (centralstore.Session, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return centralstore.Session{}, false, nil
	}
	return d.session(id)
}

func (d *fakeDurable) AcknowledgeDeadSession(ctx context.Context, id centralstore.SessionID, observed centralstore.RowVersion) (centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ackCalls = append(d.ackCalls, observed)
	if d.ackResult == nil {
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: observed + 1}, nil
	}
	return d.ackResult(id, observed)
}

func (d *fakeDurable) ReplaceProjectCatalogAndRematch(ctx context.Context, specs []centralstore.ProjectEntrySpec, peers []centralstore.LocalPeerMatchInput, at centralstore.UnixMillis) (centralstore.ProjectCatalog, centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replaceCatalogCalls = append(d.replaceCatalogCalls, peers)
	if d.replaceCatalogResult == nil {
		return centralstore.ProjectCatalog{}, centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	return d.replaceCatalogResult(specs, peers, at)
}

func (d *fakeDurable) PlaceUnplacedSessions(ctx context.Context, ids []centralstore.SessionID, at centralstore.UnixMillis) (centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.placeUnplacedCalls = append(d.placeUnplacedCalls, append([]centralstore.SessionID(nil), ids...))
	if d.placeUnplacedResult == nil {
		return centralstore.MutationResult{}, nil
	}
	return d.placeUnplacedResult(ids, at)
}

func (d *fakeDurable) ListSessions(ctx context.Context) ([]centralstore.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listSessions == nil {
		return nil, nil
	}
	return d.listSessions()
}

func (d *fakeDurable) SweepDeadSessions(ctx context.Context, candidates []centralstore.SessionID, at centralstore.UnixMillis) (centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.swept = append(d.swept, append([]centralstore.SessionID(nil), candidates...))
	if d.sweepResult == nil {
		return centralstore.MutationResult{Changed: len(candidates) > 0, SessionsDirty: len(candidates) > 0}, nil
	}
	return d.sweepResult(candidates, at)
}

func (d *fakeDurable) DismissSessionTree(ctx context.Context, root centralstore.SessionID, at centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dismissCalls = append(d.dismissCalls, root)
	if d.dismissResult == nil {
		return []centralstore.SessionID{root}, centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	return d.dismissResult(root, at)
}

func (d *fakeDurable) RemoveSessionAtVersion(ctx context.Context, id centralstore.SessionID, observed centralstore.RowVersion) (centralstore.MutationResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeCalls = append(d.removeCalls, id)
	if d.removeResult == nil {
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true}, nil
	}
	return d.removeResult(id, observed)
}

// fakeErrorSink collects reported errors.
type fakeErrorSink struct {
	mu   sync.Mutex
	errs []error
}

func (s *fakeErrorSink) Error(_ context.Context, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}
func (s *fakeErrorSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.errs)
}
func (s *fakeErrorSink) last() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) == 0 {
		return nil
	}
	return s.errs[len(s.errs)-1]
}

// fakeDirtySink collects committed outcomes. It can optionally block until
// unblocked, enabling re-entry and blocking tests.
type fakeDirtySink struct {
	mu      sync.Mutex
	results []centralstore.MutationResult
	block   chan struct{} // if non-nil, Committed blocks until closed
}

func (s *fakeDirtySink) Committed(_ context.Context, r centralstore.MutationResult) {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
}
func (s *fakeDirtySink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.results)
}

// sid returns a deterministic SessionID for tests.
func sid(n int) centralstore.SessionID { return centralstore.SessionID(fmt.Sprintf("sess-%04d", n)) }

// aliveTrue/aliveFalse are helper pointers.
var (
	aliveTrue  = func() *bool { b := true; return &b }()
	aliveFalse = func() *bool { b := false; return &b }()
)

func ts(ms int64) centralstore.UnixMillis { return centralstore.UnixMillis(ms) }

func exitedAt(ms int64) centralstore.NullablePatch[centralstore.UnixMillis] {
	t := centralstore.UnixMillis(ms)
	return centralstore.NullablePatch[centralstore.UnixMillis]{Set: &t}
}

// newCoord is a test constructor with sensible defaults.
func newCoord(client *fakeRunnerClient, durable *fakeDurable, dirty *fakeDirtySink, errSink *fakeErrorSink) *Coordinator {
	if errSink == nil {
		// Avoid a typed-nil ErrorSink interface value.
		return New(nil, client, durable, dirty, nil)
	}
	return New(nil, client, durable, dirty, errSink)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestSubscribeBeforeMetaReplayOrder verifies that events buffered between
// Subscribe and Meta are replayed in order over the meta baseline.
func TestSubscribeBeforeMetaReplayOrder(t *testing.T) {
	slug1 := "slug-one"
	slug2 := "slug-two"
	id := sid(1)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{
			ID:    id,
			Alive: true,
			Facts: centralstore.RunnerFacts{Slug: &slug1},
		},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, nil)

	// Queue events before Register is called; they'll be waiting in the
	// stream channel before Meta resolves.
	client.stream.send(RunnerEvent{ObservedAt: ts(10), Facts: centralstore.RunnerFacts{Slug: &slug2}})
	client.stream.send(RunnerEvent{ObservedAt: ts(20), Facts: centralstore.RunnerFacts{Slug: &slug1}})

	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runtime.SessionID != id {
		t.Fatalf("session ID mismatch: got %s", runtime.SessionID)
	}
	if len(dur.registered) != 1 {
		t.Fatalf("expected 1 RegisterRunner call, got %d", len(dur.registered))
	}
	// Last replayed event sets Slug back to slug1.
	reg := dur.registered[0]
	if reg.Facts.Slug == nil || *reg.Facts.Slug != slug1 {
		t.Fatalf("expected slug %q, got %v", slug1, reg.Facts.Slug)
	}
	if reg.ObservedAt != ts(20) {
		t.Fatalf("expected ObservedAt 20, got %d", reg.ObservedAt)
	}
}

// TestEventDuringBlockedMeta verifies that events arriving while Meta is
// blocked are buffered and merged into the registration.
func TestEventDuringBlockedMeta(t *testing.T) {
	slug1 := "before-meta"
	slug2 := "during-meta"
	id := sid(2)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{
			ID:    id,
			Alive: true,
			Facts: centralstore.RunnerFacts{Slug: &slug1},
		},
	}
	client := newFakeClient(meta)
	metaUnblock := make(chan struct{})
	client.metaBlock = metaUnblock

	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, nil)

	// Start registration in background; it will block at Meta.
	regDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep2"})
		regDone <- err
	}()

	// Let Subscribe complete, then send an event while Meta is blocked.
	time.Sleep(10 * time.Millisecond)
	client.stream.send(RunnerEvent{ObservedAt: ts(5), Facts: centralstore.RunnerFacts{Slug: &slug2}})

	// Unblock Meta.
	close(metaUnblock)
	if err := <-regDone; err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Slug from the event (during meta) should override the baseline.
	if len(dur.registered) != 1 {
		t.Fatalf("expected 1 RegisterRunner call")
	}
	reg := dur.registered[0]
	if reg.Facts.Slug == nil || *reg.Facts.Slug != slug2 {
		t.Fatalf("expected %q, got %v", slug2, reg.Facts.Slug)
	}
}

// TestBoundedBackpressureNoLoss verifies that the buffer applies backpressure
// when full and no event is dropped.
func TestBoundedBackpressureNoLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan RunnerEvent, 1)
	out := bufferEvents(ctx, in)

	// Fill the buffer to capacity.
	for i := range bufferCap {
		in <- RunnerEvent{ObservedAt: ts(int64(i + 1))}
	}

	// Buffer is full; reading all of them must produce events in order.
	for i := range bufferCap {
		select {
		case ev := <-out:
			if ev.ObservedAt != ts(int64(i+1)) {
				t.Fatalf("event %d: got ObservedAt %d", i, ev.ObservedAt)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
}

// TestCurrentCloseRemovesGeneration verifies that closing the stream for the
// current generation removes it from the registry.
func TestCurrentCloseRemovesGeneration(t *testing.T) {
	id := sid(3)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, nil)

	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep3"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !runtime.Subscribed {
		t.Fatal("expected Subscribed")
	}

	// Close the stream; drain should exit and remove the entry.
	client.stream.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if snap := coord.Registry().Snapshot(); len(snap) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("registry entry not removed after stream close")
}

// TestStaleCloseDoesNotRemoveCurrentGeneration verifies that a stale stream
// closure (from an old generation) does not affect the current generation.
func TestStaleCloseDoesNotRemoveCurrentGeneration(t *testing.T) {
	id := sid(4)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, nil)

	// Register first generation.
	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep4"})
	if err != nil {
		t.Fatalf("Register gen1: %v", err)
	}
	oldStream := client.stream

	// Replace with a second generation (under a lifecycle claim, like
	// Resume/Restart).
	client.stream = newFakeStream()
	cl, release := testClaim(t, coord, id)
	_, err = coord.Register(context.Background(), RegisterRequest{Endpoint: "ep4", Replace: true, Claim: cl})
	release()
	if err != nil {
		t.Fatalf("Register gen2: %v", err)
	}
	if snap := coord.Registry().Snapshot(); len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}
	gen2 := coord.Registry().Snapshot()[0].Generation

	// Close the old stream (generation 1).
	oldStream.Close()
	time.Sleep(20 * time.Millisecond)

	// Current generation must still be installed.
	snap := coord.Registry().Snapshot()
	if len(snap) != 1 || snap[0].Generation != gen2 {
		t.Fatalf("expected gen2 still installed, got %+v", snap)
	}
}

// scheduledDurable is a Durable whose behavior is fully test-controlled,
// used for deterministic replacement schedules.
type scheduledDurable struct {
	register func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error)
	apply    func(centralstore.RunnerObservation) (centralstore.MutationResult, error)
	session  func(centralstore.SessionID) (centralstore.Session, bool, error)
}

func (d *scheduledDurable) RegisterRunner(_ context.Context, reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
	return d.register(reg)
}
func (d *scheduledDurable) ApplyRunnerObservation(_ context.Context, obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
	return d.apply(obs)
}
func (d *scheduledDurable) Session(_ context.Context, id centralstore.SessionID) (centralstore.Session, bool, error) {
	if d.session == nil {
		return centralstore.Session{}, false, nil
	}
	return d.session(id)
}
func (d *scheduledDurable) ListSessions(context.Context) ([]centralstore.Session, error) {
	return nil, nil
}
func (d *scheduledDurable) SweepDeadSessions(context.Context, []centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}
func (d *scheduledDurable) AcknowledgeDeadSession(context.Context, centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}
func (d *scheduledDurable) ReplaceProjectCatalogAndRematch(context.Context, []centralstore.ProjectEntrySpec, []centralstore.LocalPeerMatchInput, centralstore.UnixMillis) (centralstore.ProjectCatalog, centralstore.MutationResult, error) {
	return nil, centralstore.MutationResult{}, nil
}
func (d *scheduledDurable) PlaceUnplacedSessions(context.Context, []centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}
func (d *scheduledDurable) DismissSessionTree(context.Context, centralstore.SessionID, centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error) {
	return nil, centralstore.MutationResult{}, nil
}
func (d *scheduledDurable) RemoveSessionAtVersion(context.Context, centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
	return centralstore.MutationResult{}, nil
}

// TestReplacementFencesInFlightOldApply forces the HIGH-severity schedule
// where an old-generation apply is in flight while the replacement's
// RegisterRunner has committed but the new generation is not yet installed.
// The fence set before the commit must prevent the stale-version retry from
// committing the old event onto the freshly registered row.
func TestReplacementFencesInFlightOldApply(t *testing.T) {
	id := sid(5)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}
	client := newFakeClient(meta)

	applyEntered := make(chan struct{})
	applyRelease := make(chan struct{})
	registerCommitted := make(chan struct{})
	registerRelease := make(chan struct{})

	var mu sync.Mutex
	var applied []centralstore.RunnerObservation
	registerCalls := 0

	dur := &scheduledDurable{
		register: func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
			mu.Lock()
			registerCalls++
			n := registerCalls
			mu.Unlock()
			if n == 2 {
				// Replacement registration: the store commit has happened
				// (version is now 2) but Register has not installed the new
				// generation yet. Hold this window open.
				close(registerCommitted)
				<-registerRelease
				return centralstore.Session{Version: 2}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
			}
			return centralstore.Session{Version: 1}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
		},
		apply: func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
			mu.Lock()
			applied = append(applied, obs)
			n := len(applied)
			mu.Unlock()
			if n == 1 {
				close(applyEntered)
				<-applyRelease
				// The replacement registration committed while this apply was
				// in flight; the row version moved on.
				return centralstore.MutationResult{SessionVersion: 2}, centralstore.ErrStaleVersion
			}
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
		},
	}
	errSink := &fakeErrorSink{}
	coord := New(nil, client, dur, &fakeDirtySink{}, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep5"}); err != nil {
		t.Fatalf("Register gen1: %v", err)
	}
	oldStream := client.stream

	// Old-generation event; its apply blocks inside the durable.
	oldStream.send(RunnerEvent{ObservedAt: ts(10), Alive: aliveTrue})
	<-applyEntered

	// Replacement registration; blocks in the commit-to-install window.
	client.stream = newFakeStream()
	cl, release := testClaim(t, coord, id)
	defer release()
	regDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep5", Replace: true, Claim: cl})
		regDone <- err
	}()
	<-registerCommitted

	// Release the old apply: it observes ErrStaleVersion with the committed
	// replacement version. The fence must stop the retry from re-applying
	// with the new version.
	close(applyRelease)

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n > 1 {
			t.Fatal("stale-version retry committed onto the replacement registration")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(registerRelease)
	if err := <-regDone; err != nil {
		t.Fatalf("Register gen2: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 1 || applied[0].ObservedVersion != 1 {
		t.Fatalf("applied = %#v, want exactly the original v1 observation", applied)
	}
	if errSink.count() != 0 {
		t.Fatalf("unexpected error: %v", errSink.last())
	}
}

// TestFailedReplacementCommitsInFlightOldApply verifies that when a
// replacement's RegisterRunner FAILS, an old-generation event whose apply ran
// into the fence window is not silently discarded: after the fence is lifted
// (restore), the still-installed old generation's observation must commit.
func TestFailedReplacementCommitsInFlightOldApply(t *testing.T) {
	id := sid(8)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}
	client := newFakeClient(meta)

	registerEntered := make(chan struct{})
	registerRelease := make(chan struct{})

	var mu sync.Mutex
	var applied []centralstore.RunnerObservation
	registerCalls := 0

	dur := &scheduledDurable{
		register: func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
			mu.Lock()
			registerCalls++
			n := registerCalls
			mu.Unlock()
			if n == 2 {
				// Failing replacement: hold the fence window open, then fail.
				close(registerEntered)
				<-registerRelease
				return centralstore.Session{}, centralstore.MutationResult{}, errors.New("replacement db failure")
			}
			return centralstore.Session{Version: 1}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
		},
		apply: func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
			mu.Lock()
			applied = append(applied, obs)
			mu.Unlock()
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
		},
	}
	errSink := &fakeErrorSink{}
	sink := &fakeDirtySink{}
	coord := New(nil, client, dur, sink, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep8f"}); err != nil {
		t.Fatalf("Register gen1: %v", err)
	}
	oldStream := client.stream
	gen1 := coord.Registry().Snapshot()[0].Generation

	// Start the failing replacement; wait until it is inside the fence
	// window (fence set under c.mu before RegisterRunner runs).
	client.stream = newFakeStream()
	cl, release := testClaim(t, coord, id)
	defer release()
	regDone := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep8f", Replace: true, Claim: cl})
		regDone <- err
	}()
	<-registerEntered

	// Old-generation event; its apply hits the fence and must wait for the
	// lifecycle mutex instead of discarding.
	oldStream.send(RunnerEvent{ObservedAt: ts(10), Alive: aliveTrue})
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(applied) != 0 {
		mu.Unlock()
		t.Fatal("apply committed inside the fence window")
	}
	mu.Unlock()

	// Fail the replacement; the fence is restored and the event must commit.
	close(registerRelease)
	if err := <-regDone; err == nil {
		t.Fatal("expected replacement registration to fail")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(applied) != 1 || applied[0].ObservedVersion != 1 {
		mu.Unlock()
		t.Fatalf("applied = %#v, want the old-generation event committed after the fence lifted", applied)
	}
	mu.Unlock()
	if errSink.count() != 0 {
		t.Fatalf("unexpected error: %v", errSink.last())
	}
	snap := coord.Registry().Snapshot()
	if len(snap) != 1 || snap[0].Generation != gen1 {
		t.Fatalf("old generation not still installed: %+v", snap)
	}
}

// TestReplacementDiscardsOldEvents verifies that an old-generation event whose
// apply completes only after the replacement is installed cannot commit with
// the new generation's version token.
func TestReplacementDiscardsOldEvents(t *testing.T) {
	id := sid(6)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}
	client := newFakeClient(meta)

	applyEntered := make(chan struct{})
	applyRelease := make(chan struct{})
	var mu sync.Mutex
	var applied []centralstore.RunnerObservation

	version := centralstore.RowVersion(0)
	dur := &scheduledDurable{
		register: func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
			mu.Lock()
			version++
			v := version
			mu.Unlock()
			return centralstore.Session{Version: v}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: v}, nil
		},
		apply: func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
			mu.Lock()
			applied = append(applied, obs)
			n := len(applied)
			mu.Unlock()
			if n == 1 {
				close(applyEntered)
				<-applyRelease
				return centralstore.MutationResult{SessionVersion: 2}, centralstore.ErrStaleVersion
			}
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
		},
	}
	coord := New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{})

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep6"}); err != nil {
		t.Fatalf("Register gen1: %v", err)
	}
	oldStream := client.stream

	// Old-generation event captured with the old version token; blocked in
	// the durable until after the replacement is fully installed.
	oldStream.send(RunnerEvent{ObservedAt: ts(10), Alive: aliveTrue})
	<-applyEntered

	client.stream = newFakeStream()
	cl, release := testClaim(t, coord, id)
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep6", Replace: true, Claim: cl}); err != nil {
		t.Fatalf("Register gen2: %v", err)
	}
	release()

	// Release the old apply; the stale retry must be discarded because the
	// installed generation is no longer the event's generation.
	close(applyRelease)

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n > 1 {
			t.Fatal("old-generation event committed after replacement")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 1 || applied[0].ObservedVersion != 1 {
		t.Fatalf("applied = %#v, want exactly the original v1 observation", applied)
	}
}

// TestFastDeadReplacementSynthesizesExitAndRemovesOldEntry verifies that a
// replacement whose stream closed before registration completed (and which
// carries no exit facts) still registers as dead with a synthesized exit
// timestamp and removes the old generation entry.
func TestFastDeadReplacementSynthesizesExitAndRemovesOldEntry(t *testing.T) {
	id := sid(7)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true, ObservedAt: ts(42)}}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep7f"}); err != nil {
		t.Fatalf("Register gen1: %v", err)
	}
	if len(coord.Registry().Snapshot()) != 1 {
		t.Fatal("expected gen1 installed")
	}

	// Replacement stream closes before Register runs.
	client.stream = newFakeStream()
	client.stream.Close()
	cl, release := testClaim(t, coord, id)
	defer release()
	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep7f", Replace: true, Claim: cl})
	if err != nil {
		t.Fatalf("Register replacement: %v", err)
	}
	if runtime.Subscribed {
		t.Fatal("expected Subscribed=false for fast-dead replacement")
	}
	if len(dur.registered) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(dur.registered))
	}
	reg := dur.registered[1]
	if !reg.NewGeneration || reg.Alive {
		t.Fatalf("replacement registration = %+v", reg)
	}
	if reg.Facts.ExitedAt.Set == nil || *reg.Facts.ExitedAt.Set != ts(42) {
		t.Fatalf("expected synthesized ExitedAt=42, got %+v", reg.Facts.ExitedAt)
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("old generation entry not removed after fast-dead replacement")
	}
}

// TestActiveNonReplace verifies that registering an already-active session
// without Replace returns ErrGenerationActive.
func TestActiveNonReplace(t *testing.T) {
	id := sid(6)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep6"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep6"})
	if !errors.Is(err, ErrGenerationActive) {
		t.Fatalf("expected ErrGenerationActive, got %v", err)
	}
}

// TestSubscribeFailureCleanup verifies that a Subscribe failure cancels
// provisional resources and leaves the registry unchanged.
func TestSubscribeFailureCleanup(t *testing.T) {
	id := sid(7)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	client.subscribeErr = errors.New("subscribe failed")

	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep7"})
	if err == nil || !strings.Contains(err.Error(), "subscribe failed") {
		t.Fatalf("expected subscribe error, got %v", err)
	}
	if len(dur.registered) != 0 {
		t.Fatal("RegisterRunner should not be called after Subscribe failure")
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("registry should be empty after Subscribe failure")
	}
}

// TestMetaFailureCleanup verifies that a Meta failure cancels the provisional
// stream and leaves the registry unchanged.
func TestMetaFailureCleanup(t *testing.T) {
	id := sid(8)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	client.metaErr = errors.New("meta failed")

	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep8"})
	if err == nil || !strings.Contains(err.Error(), "meta failed") {
		t.Fatalf("expected meta error, got %v", err)
	}
	if len(dur.registered) != 0 {
		t.Fatal("RegisterRunner should not be called after Meta failure")
	}
	if !client.stream.closed.Load() {
		t.Fatal("provisional stream not closed after Meta failure")
	}
}

// TestRegisterFailureCleanup verifies that a RegisterRunner failure cancels
// the stream and leaves the registry unchanged.
func TestRegisterFailureCleanup(t *testing.T) {
	id := sid(9)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	dur.registerResult = func(_ centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return centralstore.Session{}, centralstore.MutationResult{}, errors.New("db failure")
	}
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep9"})
	if err == nil || !strings.Contains(err.Error(), "db failure") {
		t.Fatalf("expected db failure, got %v", err)
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("registry should be empty after RegisterRunner failure")
	}
}

// TestRequestCancellationDuringSetup verifies that canceling the request
// context while blocked in Subscribe aborts the call and leaks no goroutines.
func TestRequestCancellationDuringSetup(t *testing.T) {
	id := sid(10)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	client.subscribeBlock = make(chan struct{}) // never closed; Subscribe blocks

	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := coord.Register(ctx, RegisterRequest{Endpoint: "ep10"})
		errCh <- err
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after context cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("Register did not return after context cancel")
	}
}

// TestDirtyPostCommitOutsideLock verifies that:
// (a) dirty sink is called after commit, not before;
// (b) a blocking dirty sink does not hold the lifecycle mutex;
// (c) a re-entrant dirty sink (calling Registry().Snapshot()) does not deadlock.
func TestDirtyPostCommitOutsideLock(t *testing.T) {
	id := sid(11)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)

	var coord *Coordinator
	reentrantSink := DirtySinkFunc(func(ctx context.Context, r centralstore.MutationResult) {
		// Must not deadlock: coordinator must not hold c.mu when calling this.
		snap := coord.Registry().Snapshot()
		_ = snap
	})
	coord = New(nil, client, dur, reentrantSink, nil)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep11"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// TestDirtyBlockingDoesNotStallCoordinator verifies that a blocking dirty
// sink does not hold the lifecycle mutex. A second concurrent registration
// on the same coordinator must proceed while the first's dirty sink blocks.
func TestDirtyBlockingDoesNotStallCoordinator(t *testing.T) {
	id1 := sid(12)
	id2 := sid(13)

	// sinkBlock controls when Committed unblocks. It is only used by the
	// first registration; close it to release.
	sinkBlock := make(chan struct{})
	var sinkCallCount atomic.Int32
	sink := DirtySinkFunc(func(ctx context.Context, r centralstore.MutationResult) {
		// Only the first call blocks; subsequent calls are the second
		// registration (or drain) and must not deadlock.
		if sinkCallCount.Add(1) == 1 {
			<-sinkBlock
		}
	})

	// Route by endpoint so both registrations share one coordinator.
	epClient := &endpointRunnerClient{
		routes: map[string]*fakeRunnerClient{
			"ep12": newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id1, Alive: true}}),
			"ep13": newFakeClient(RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id2, Alive: true}}),
		},
	}
	coord := New(nil, epClient, newFakeDurable(0), sink, nil)

	// First registration goroutine; its dirty sink will block.
	done1 := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep12"})
		done1 <- err
	}()

	// Wait until the first registration's sink is blocked.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sinkCallCount.Load() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if sinkCallCount.Load() == 0 {
		t.Fatal("timeout waiting for first registration's dirty sink")
	}

	// Second registration on the same coordinator must not be blocked by the
	// first's blocked dirty sink (mutex must not be held during publish).
	done2 := make(chan error, 1)
	go func() {
		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep13"})
		done2 <- err
	}()

	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("second registration: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second registration stalled by first's blocking dirty sink")
	}

	// Unblock the sink to let first registration finish.
	close(sinkBlock)
	if err := <-done1; err != nil {
		t.Fatalf("first registration: %v", err)
	}
}

// endpointRunnerClient routes by endpoint string.
type endpointRunnerClient struct {
	routes map[string]*fakeRunnerClient
}

func (c *endpointRunnerClient) Subscribe(ctx context.Context, ep string) (EventStream, error) {
	return c.routes[ep].Subscribe(ctx, ep)
}
func (c *endpointRunnerClient) Meta(ctx context.Context, ep string) (RunnerMeta, error) {
	return c.routes[ep].Meta(ctx, ep)
}

// TestStaleVersionRetry verifies that ErrStaleVersion causes a single retry
// with the refreshed row version, and that a permanent failure is reported.
func TestStaleVersionRetry(t *testing.T) {
	id := sid(14)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	errSink := &fakeErrorSink{}
	dur := newFakeDurable(0)

	var callCount atomic.Int32
	staleVersion := centralstore.RowVersion(99)
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		n := callCount.Add(1)
		if n == 1 {
			// Return stale with current version.
			return centralstore.MutationResult{SessionVersion: staleVersion}, centralstore.ErrStaleVersion
		}
		if n == 2 {
			// Retry with refreshed version should succeed.
			if obs.ObservedVersion != staleVersion {
				return centralstore.MutationResult{}, fmt.Errorf("expected refreshed version %d, got %d", staleVersion, obs.ObservedVersion)
			}
			return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: staleVersion + 1}, nil
		}
		return centralstore.MutationResult{}, errors.New("unexpected call")
	}

	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep14"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Send one event to trigger apply.
	t1 := ts(100)
	client.stream.send(RunnerEvent{ObservedAt: t1, Alive: aliveTrue})

	// Wait for apply to complete (retry succeeds). sink is called once for
	// the registration itself, then again for the apply result.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sink.count() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sink.count() < 2 {
		t.Fatalf("dirty sink not called after stale-version retry (sink count=%d)", sink.count())
	}
	if errSink.count() != 0 {
		t.Fatalf("unexpected error reported: %v", errSink.last())
	}
	if n := callCount.Load(); n != 2 {
		t.Fatalf("expected 2 apply calls, got %d", n)
	}
}

// TestStaleVersionRetryExhausted verifies that a permanent stale failure after
// retry is surfaced through the error sink.
func TestStaleVersionRetryExhausted(t *testing.T) {
	id := sid(15)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	errSink := &fakeErrorSink{}
	dur := newFakeDurable(0)
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		// Always stale with a current version (forcing retry to also stale).
		return centralstore.MutationResult{SessionVersion: obs.ObservedVersion + 1}, centralstore.ErrStaleVersion
	}
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep15"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	client.stream.send(RunnerEvent{ObservedAt: ts(100), Alive: aliveTrue})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errSink.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if errSink.count() == 0 {
		t.Fatal("expected error after exhausted retry")
	}
}

// TestMalformedExitHandling verifies that an Alive=false event without
// ExitedAt is reported as malformed but still removes liveness.
func TestMalformedExitHandling(t *testing.T) {
	id := sid(16)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	errSink := &fakeErrorSink{}
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep16"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Send Alive=false without ExitedAt (malformed).
	client.stream.send(RunnerEvent{ObservedAt: ts(100), Alive: aliveFalse})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errSink.count() > 0 && len(coord.Registry().Snapshot()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if errSink.count() == 0 {
		t.Fatal("expected error for malformed exit event")
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("registry entry should be removed despite malformed exit")
	}
}

// TestExitFactsCommitBeforeLivenessRemoved verifies that a well-formed exit
// event (Alive=false, ExitedAt set) commits facts to the store before removing
// the registry entry.
func TestExitFactsCommitBeforeLivenessRemoved(t *testing.T) {
	id := sid(17)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	errSink := &fakeErrorSink{}

	var applyOrder []string
	dur := newFakeDurable(0)
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		applyOrder = append(applyOrder, "apply")
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
	}

	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep17"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Well-formed exit event.
	client.stream.send(RunnerEvent{
		ObservedAt: ts(100),
		Alive:      aliveFalse,
		Facts:      centralstore.RunnerFacts{ExitedAt: exitedAt(100)},
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(coord.Registry().Snapshot()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("expected registry entry removed after exit event")
	}
	if errSink.count() != 0 {
		t.Fatalf("unexpected error: %v", errSink.last())
	}
	// apply must have been called (facts committed) before entry removed.
	if len(applyOrder) == 0 {
		t.Fatal("expected ApplyRunnerObservation to be called for exit event")
	}
}

// TestContextGoroutineCleanup verifies that canceling the registered stream
// context cleans up the drain goroutine.
func TestContextGoroutineCleanup(t *testing.T) {
	id := sid(18)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep18"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Close stream to trigger drain exit.
	client.stream.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(coord.Registry().Snapshot()) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("drain goroutine did not clean up")
}

// TestConcurrentRegistrationAndEventRace is a race-detector test that
// exercises concurrent registration, event observation, and close.
func TestConcurrentRegistrationAndEventRace(t *testing.T) {
	const goroutines = 8
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := sid(100 + n)
			meta := RunnerMeta{
				Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
			}
			client := newFakeClient(meta)
			dur := newFakeDurable(0)
			coord := newCoord(client, dur, &fakeDirtySink{}, &fakeErrorSink{})

			if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: fmt.Sprintf("ep%d", n)}); err != nil {
				t.Errorf("Register %d: %v", n, err)
				return
			}

			slug := "slug"
			for j := range 5 {
				client.stream.send(RunnerEvent{
					ObservedAt: ts(int64(j + 1)),
					Facts:      centralstore.RunnerFacts{Slug: &slug},
				})
			}
			client.stream.Close()
		}(i)
	}
	wg.Wait()
}

// TestCopyRuntimeSnapshot verifies that Snapshot returns immutable copies
// and that mutating the returned slice does not affect the registry.
func TestCopyRuntimeSnapshot(t *testing.T) {
	id := sid(200)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep200"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	snap := coord.Registry().Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}

	// Mutate the snapshot; registry must be unaffected.
	snap[0].PID = 99999
	snap[0].RunnerVersion = "mutated"

	snap2 := coord.Registry().Snapshot()
	if len(snap2) != 1 {
		t.Fatalf("expected 1 entry after mutation, got %d", len(snap2))
	}
	if snap2[0].PID == 99999 {
		t.Fatal("snapshot mutation leaked into registry")
	}
}

// TestFastDeadRegistration verifies that a fast-dead registration (Subscribe
// returns a stream that closes before Meta) produces Alive=false and no drain.
func TestFastDeadRegistration(t *testing.T) {
	id := sid(201)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, nil)

	// Close stream before Register is called.
	client.stream.Close()

	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep201"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runtime.Subscribed {
		t.Fatal("expected Subscribed=false for fast-dead registration")
	}
	if len(dur.registered) != 1 || dur.registered[0].Alive {
		t.Fatal("expected Alive=false in RegisterRunner call")
	}
	if dur.registered[0].Facts.ExitedAt.Set == nil {
		t.Fatal("expected synthesized ExitedAt for fast-dead registration")
	}
	// No drain goroutine; registry should be empty.
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("expected empty registry for fast-dead registration")
	}
}

// TestMetaDeadRegistrationNotSubscribed pins the Subscribed derivation:
// a runner whose meta reports Alive=false (open stream, no exit fact) is
// registered fast-dead — Subscribed=false, exit synthesized, no drain — even
// though the stream never closed. This is the case the removed dead
// `!closed` term could never distinguish from liveness.
func TestMetaDeadRegistrationNotSubscribed(t *testing.T) {
	id := sid(202)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: false, ObservedAt: ts(7)},
	}
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)

	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep202"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if runtime.Subscribed {
		t.Fatal("expected Subscribed=false for meta-dead registration")
	}
	if len(dur.registered) != 1 || dur.registered[0].Alive {
		t.Fatal("expected Alive=false in RegisterRunner call")
	}
	if dur.registered[0].Facts.ExitedAt.Set == nil {
		t.Fatal("expected synthesized ExitedAt for meta-dead registration")
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("expected empty registry for meta-dead registration")
	}
	if !client.stream.closed.Load() {
		t.Fatal("expected the stream to be closed")
	}
}

// TestExitApplyStaleRetryBudget verifies that an exit-carrying event gets the
// ensureDurableExit-sized stale-retry budget (up to three retries) instead of
// the ordinary single retry: three stale responses followed by success still
// land the exit durably with no error reported.
func TestExitApplyStaleRetryBudget(t *testing.T) {
	id := sid(203)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	errSink := &fakeErrorSink{}
	dur := newFakeDurable(0)

	var callCount atomic.Int32
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		n := callCount.Add(1)
		if n <= 3 {
			return centralstore.MutationResult{SessionVersion: obs.ObservedVersion + 1}, centralstore.ErrStaleVersion
		}
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: obs.ObservedVersion + 1}, nil
	}
	sink := &fakeDirtySink{}
	coord := newCoord(client, dur, sink, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep203"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	client.stream.send(RunnerEvent{ObservedAt: ts(100), Alive: aliveFalse, Facts: centralstore.RunnerFacts{ExitedAt: exitedAt(100)}})

	// The successful apply removes the entry (exit); wait for that.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if callCount.Load() >= 4 && len(coord.Registry().Snapshot()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := callCount.Load(); n != 4 {
		t.Fatalf("expected 4 apply calls (3 stale retries + success), got %d", n)
	}
	if errSink.count() != 0 {
		t.Fatalf("unexpected error reported: %v", errSink.last())
	}
	if len(coord.Registry().Snapshot()) != 0 {
		t.Fatal("expected entry removed after durable exit")
	}
}

// TestExitApplyStaleRetryExhausted verifies that a permanently stale
// exit-carrying apply gives up after three retries and reports the failure.
func TestExitApplyStaleRetryExhausted(t *testing.T) {
	id := sid(204)
	meta := RunnerMeta{
		Registration: centralstore.RunnerRegistration{ID: id, Alive: true},
	}
	client := newFakeClient(meta)
	errSink := &fakeErrorSink{}
	dur := newFakeDurable(0)
	var callCount atomic.Int32
	dur.applyResult = func(obs centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		callCount.Add(1)
		return centralstore.MutationResult{SessionVersion: obs.ObservedVersion + 1}, centralstore.ErrStaleVersion
	}
	coord := newCoord(client, dur, &fakeDirtySink{}, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep204"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	client.stream.send(RunnerEvent{ObservedAt: ts(100), Alive: aliveFalse, Facts: centralstore.RunnerFacts{ExitedAt: exitedAt(100)}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errSink.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if errSink.count() == 0 {
		t.Fatal("expected error after exhausted exit-apply retries")
	}
	if !errors.Is(errSink.last(), centralstore.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion in reported error, got %v", errSink.last())
	}
	if n := callCount.Load(); n != 4 {
		t.Fatalf("expected 4 apply calls (initial + 3 retries), got %d", n)
	}
}

// TestRegisterRejectsInvalidSessionID verifies that a runner reporting a
// malformed session ID (a potential path-traversal vector — the ID becomes a
// filesystem path segment) is rejected before any commit, fence, or registry
// change, for every Register caller.
func TestRegisterRejectsInvalidSessionID(t *testing.T) {
	for _, bad := range []string{"", "no-prefix", "sess-", "sess-../../etc", "sess-a/b", "../sess-x"} {
		meta := RunnerMeta{
			Registration: centralstore.RunnerRegistration{ID: centralstore.SessionID(bad), Alive: true},
		}
		client := newFakeClient(meta)
		dur := newFakeDurable(0)
		coord := newCoord(client, dur, &fakeDirtySink{}, nil)

		_, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep205"})
		if !errors.Is(err, ErrInvalidSessionID) {
			t.Fatalf("id %q: expected ErrInvalidSessionID, got %v", bad, err)
		}
		if len(dur.registered) != 0 {
			t.Fatalf("id %q: RegisterRunner must not be called", bad)
		}
		if len(coord.Registry().Snapshot()) != 0 {
			t.Fatalf("id %q: registry must stay empty", bad)
		}
		if !client.stream.closed.Load() {
			t.Fatalf("id %q: expected the subscribed stream to be closed", bad)
		}
	}
}

// testClaim reserves the per-session lifecycle slot exactly like
// Resume/Restart do, so tests can exercise Replace registrations under the
// checked ErrReplaceWithoutClaim invariant. It returns the claim token (to
// be threaded into RegisterRequest.Claim) and the release.
func testClaim(t *testing.T, coord *Coordinator, id centralstore.SessionID) (*LifecycleClaim, func()) {
	t.Helper()
	cl, release, err := coord.claim(id, "test")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return cl, release
}

// TestReplaceRequiresClaim verifies that Replace/ExpectedID provenance is a
// checked invariant: a registration carrying either aborts with
// ErrReplaceWithoutClaim unless it presents the token of the caller's own
// held claim — no token, a foreign token, and an unrelated operation's
// concurrent claim all fail, with no commit, fence, or registry change.
func TestReplaceRequiresClaim(t *testing.T) {
	id := sid(206)
	meta := RunnerMeta{Registration: centralstore.RunnerRegistration{ID: id, Alive: true}}

	expectRejected := func(t *testing.T, coord *Coordinator, dur *fakeDurable, req RegisterRequest) {
		t.Helper()
		if _, err := coord.Register(context.Background(), req); !errors.Is(err, ErrReplaceWithoutClaim) {
			t.Fatalf("req %+v: expected ErrReplaceWithoutClaim, got %v", req, err)
		}
		if len(dur.registered) != 0 {
			t.Fatal("RegisterRunner must not be called without the caller's claim")
		}
		if len(coord.Registry().Snapshot()) != 0 {
			t.Fatal("registry must stay empty")
		}
	}

	for _, req := range []RegisterRequest{
		{Endpoint: "ep206", Replace: true},
		{Endpoint: "ep206", ExpectedID: id},
	} {
		client := newFakeClient(meta)
		dur := newFakeDurable(0)
		coord := newCoord(client, dur, &fakeDirtySink{}, nil)
		expectRejected(t, coord, dur, req)
	}

	// A concurrent UNRELATED claim (e.g. a Stop in its long terminate/wait
	// window) must not authorize a stray tokenless Replace — the fable M-1
	// loophole.
	{
		client := newFakeClient(meta)
		dur := newFakeDurable(0)
		coord := newCoord(client, dur, &fakeDirtySink{}, nil)
		_, release, err := coord.claim(id, "stop")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		defer release()
		expectRejected(t, coord, dur, RegisterRequest{Endpoint: "ep206", Replace: true})
		// A foreign token (another session's claim) must fail identity too.
		foreign, releaseForeign, err := coord.claim(sid(299), "test")
		if err != nil {
			t.Fatalf("foreign claim: %v", err)
		}
		defer releaseForeign()
		expectRejected(t, coord, dur, RegisterRequest{Endpoint: "ep206", Replace: true, Claim: foreign})
	}

	// Under the caller's own claim token the same request commits.
	client := newFakeClient(meta)
	dur := newFakeDurable(0)
	coord := newCoord(client, dur, &fakeDirtySink{}, nil)
	cl, release := testClaim(t, coord, id)
	defer release()
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep206", Replace: true, ExpectedID: id, Claim: cl}); err != nil {
		t.Fatalf("claimed Replace registration: %v", err)
	}
	if len(dur.registered) != 1 {
		t.Fatalf("expected 1 registration, got %d", len(dur.registered))
	}
}

// TestTakeoverUnconfiguredWarning verifies the silent-loss guard: a
// registration whose merged facts carry a conversation ref while takeover is
// not configured warns through the ErrorSink exactly once per process.
func TestTakeoverUnconfiguredWarning(t *testing.T) {
	ref := "conv-1"
	mkMeta := func(n int) RunnerMeta {
		return RunnerMeta{Registration: centralstore.RunnerRegistration{
			ID: sid(207 + n), Alive: true,
			Facts: centralstore.RunnerFacts{ConversationRef: &ref},
		}}
	}
	errSink := &fakeErrorSink{}
	dur := newFakeDurable(0)
	client := newFakeClient(mkMeta(0))
	coord := newCoord(client, dur, &fakeDirtySink{}, errSink)

	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep207"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if errSink.count() != 1 {
		t.Fatalf("expected 1 warning, got %d", errSink.count())
	}
	if !strings.Contains(errSink.last().Error(), "takeover") {
		t.Fatalf("warning should mention takeover: %v", errSink.last())
	}

	// Second conversation-bearing registration: no second warning.
	coord.runners = newFakeClient(mkMeta(1))
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep208"}); err != nil {
		t.Fatalf("Register 2: %v", err)
	}
	if errSink.count() != 1 {
		t.Fatalf("warning must fire once per process, got %d", errSink.count())
	}

	// A takeover-configured coordinator never warns.
	errSink2 := &fakeErrorSink{}
	coord2 := New(nil, newFakeClient(mkMeta(2)), newFakeDurable(0), &fakeDirtySink{}, errSink2,
		WithConversationTakeover(nil))
	if _, err := coord2.Register(context.Background(), RegisterRequest{Endpoint: "ep209"}); err != nil {
		t.Fatalf("Register 3: %v", err)
	}
	if errSink2.count() != 0 {
		t.Fatalf("takeover-configured coordinator must not warn: %v", errSink2.last())
	}
}
