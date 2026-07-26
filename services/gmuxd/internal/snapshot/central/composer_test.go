package central

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// fakeReader records queries and returns canned snapshots. If gate is
// non-nil, ReadSnapshot blocks until the test releases it.
type fakeReader struct {
	mu      sync.Mutex
	queries []centralstore.SnapshotQuery
	result  func(centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error)
	gate    chan struct{}
}

func (r *fakeReader) ReadSnapshot(ctx context.Context, q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
	r.mu.Lock()
	r.queries = append(r.queries, q)
	result, gate := r.result, r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return centralstore.StoreSnapshot{}, ctx.Err()
		}
	}
	if result == nil {
		return centralstore.StoreSnapshot{}, nil
	}
	return result(q)
}

func (r *fakeReader) calls() []centralstore.SnapshotQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]centralstore.SnapshotQuery(nil), r.queries...)
}

// blockingSink delivers each batch to out and, if gate is non-nil, blocks
// inside Emit until the gate is released.
type blockingSink struct {
	out  chan Batch
	gate chan struct{}
}

func (s *blockingSink) Emit(ctx context.Context, b Batch) {
	s.out <- b
	if s.gate != nil {
		<-s.gate
	}
}

type errCollector struct {
	mu   sync.Mutex
	errs []error
}

func (e *errCollector) Error(_ context.Context, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errs = append(e.errs, err)
}

func recvBatch(t *testing.T, ch <-chan Batch) Batch {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a composed batch")
		return Batch{}
	}
}

func expectNoBatch(t *testing.T, ch <-chan Batch, quiet time.Duration) {
	t.Helper()
	select {
	case b := <-ch:
		t.Fatalf("unexpected batch %#v", b)
	case <-time.After(quiet):
	}
}

func startComposer(t *testing.T, c *Composer) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	t.Cleanup(func() {
		c.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("composer did not stop")
		}
	})
}

func TestCrossKindInvalidationComposesMatchedPairFromOneRead(t *testing.T) {
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{
			Sessions: []centralstore.SessionView{{Session: centralstore.Session{ID: "a"}}},
			Projects: centralstore.ProjectCatalog{{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "one"}},
		}, nil
	}}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink)
	startComposer(t, c)

	c.Invalidate(centralstore.MutationResult{Changed: true, SessionsDirty: true, WorldDirty: true})
	b := recvBatch(t, sink.out)
	if b.Sessions == nil || b.Projects == nil {
		t.Fatalf("cross-kind batch must carry both payloads: %#v", b)
	}
	if len(b.Sessions.Sessions) != 1 || len(b.Projects.Projects) != 1 {
		t.Fatalf("batch=%#v", b)
	}
	calls := reader.calls()
	if len(calls) != 1 || !calls[0].IncludeSessions || !calls[0].IncludeProjects {
		t.Fatalf("matched pair must come from ONE read transaction, got %#v", calls)
	}
}

func TestSingleKindInvalidationUsesNarrowRead(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink)
	startComposer(t, c)

	c.Invalidate(centralstore.MutationResult{Changed: true, SessionsDirty: true})
	b := recvBatch(t, sink.out)
	if b.Sessions == nil || b.Projects != nil {
		t.Fatalf("sessions-only batch=%#v", b)
	}
	c.Invalidate(centralstore.MutationResult{Changed: true, WorldDirty: true})
	b = recvBatch(t, sink.out)
	if b.Sessions != nil || b.Projects == nil {
		t.Fatalf("projects-only batch=%#v", b)
	}
	calls := reader.calls()
	if len(calls) != 2 || calls[0].IncludeProjects || calls[1].IncludeSessions {
		t.Fatalf("narrow reads expected, got %#v", calls)
	}
}

func TestBurstCoalescesToOnePendingComposition(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8), gate: make(chan struct{})}
	c := New(reader, nil, sink)
	startComposer(t, c)

	c.MarkDirty(true, false)
	recvBatch(t, sink.out) // first composition is now blocked inside Emit

	for i := 0; i < 50; i++ {
		c.MarkDirty(true, false)
	}
	close(sink.gate) // release Emit; the burst must collapse into one pass
	recvBatch(t, sink.out)
	expectNoBatch(t, sink.out, 100*time.Millisecond)
	if calls := reader.calls(); len(calls) != 2 {
		t.Fatalf("read passes=%d, want 2 (burst coalesced)", len(calls))
	}
}

func TestDirtDuringCompositionTriggersAnotherPass(t *testing.T) {
	reader := &fakeReader{gate: make(chan struct{}, 8)}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink)
	startComposer(t, c)

	c.MarkDirty(true, false)
	// Wait until the composer is inside ReadSnapshot, then dirty the other
	// kind before releasing the read.
	deadline := time.Now().Add(5 * time.Second)
	for len(reader.calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("composer never started reading")
		}
		time.Sleep(time.Millisecond)
	}
	c.MarkDirty(false, true)
	reader.gate <- struct{}{}
	first := recvBatch(t, sink.out)
	if first.Sessions == nil || first.Projects != nil {
		t.Fatalf("first batch=%#v", first)
	}
	reader.gate <- struct{}{}
	second := recvBatch(t, sink.out)
	if second.Projects == nil || second.Sessions != nil {
		t.Fatalf("dirt during composition lost: %#v", second)
	}
}

func TestCompositionFailureRetainsDirtAndRetries(t *testing.T) {
	boom := errors.New("read failed")
	fail := true
	var mu sync.Mutex
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			fail = false
			return centralstore.StoreSnapshot{}, boom
		}
		return centralstore.StoreSnapshot{}, nil
	}}
	errs := &errCollector{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithErrorSink(errs), WithRetryDelay(time.Millisecond))
	startComposer(t, c)

	c.MarkDirty(true, true)
	b := recvBatch(t, sink.out)
	if b.Sessions == nil || b.Projects == nil {
		t.Fatalf("retried batch must keep both dirty kinds: %#v", b)
	}
	errs.mu.Lock()
	defer errs.mu.Unlock()
	if len(errs.errs) != 1 || !errors.Is(errs.errs[0], boom) {
		t.Fatalf("errors=%v", errs.errs)
	}
}

func TestRuntimeOverlayDerivesAliveAndResumable(t *testing.T) {
	reader := &fakeReader{result: func(centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{Sessions: []centralstore.SessionView{
			{Session: centralstore.Session{ID: "live", Command: []string{"sh"}}},
			{Session: centralstore.Session{ID: "dead", Command: []string{"sh"}, StartedAt: ptrMillis(1)}},
		}}, nil
	}}
	runtime := RuntimeSourceFunc(func() map[centralstore.SessionID]RuntimeFacts {
		return map[centralstore.SessionID]RuntimeFacts{
			"live": {PID: 42, Endpoint: "sock", RunnerVersion: "v", BinaryHash: "h"},
		}
	})
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, runtime, sink)
	startComposer(t, c)

	c.MarkDirty(true, false)
	b := recvBatch(t, sink.out)
	rows := b.Sessions.Sessions
	if len(rows) != 2 {
		t.Fatalf("rows=%#v", rows)
	}
	live, dead := rows[0], rows[1]
	if !live.Alive || live.Resumable || live.Runtime == nil || live.Runtime.PID != 42 {
		t.Fatalf("live overlay=%#v", live)
	}
	if dead.Alive || !dead.Resumable || dead.Runtime != nil {
		t.Fatalf("dead overlay=%#v", dead)
	}
}

// TestCloseBeforePendingWakeNeverEmits pins conc LOW-01: Close raced by a
// pending wake-up must win. Both the done and wake channels are ready before
// Run starts, so whichever case the first select picks, the post-wake done
// re-check must return without composing or emitting.
func TestCloseBeforePendingWakeNeverEmits(t *testing.T) {
	for i := 0; i < 50; i++ {
		reader := &fakeReader{}
		sink := &blockingSink{out: make(chan Batch, 8)}
		c := New(reader, nil, sink)
		c.MarkDirty(true, true) // wake-up now pending
		c.Close()
		done := make(chan struct{})
		go func() {
			c.Run(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop")
		}
		if len(reader.calls()) != 0 {
			t.Fatal("composition ran after Close")
		}
		expectNoBatch(t, sink.out, time.Millisecond)
	}
}

func TestSingleKindCompositionFailureRestoresOnlyThatKind(t *testing.T) {
	boom := errors.New("read failed")
	fail := true
	var mu sync.Mutex
	reader := &fakeReader{result: func(q centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			fail = false
			return centralstore.StoreSnapshot{}, boom
		}
		return centralstore.StoreSnapshot{}, nil
	}}
	errs := &errCollector{}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink, WithErrorSink(errs), WithRetryDelay(time.Millisecond))
	startComposer(t, c)

	c.MarkDirty(true, false)
	b := recvBatch(t, sink.out)
	if b.Sessions == nil || b.Projects != nil {
		t.Fatalf("retry must recompose exactly the failed kind: %#v", b)
	}
	calls := reader.calls()
	if len(calls) != 2 || calls[1].IncludeProjects || !calls[1].IncludeSessions {
		t.Fatalf("retry query widened: %#v", calls)
	}
}

func TestEmptySnapshotComposesEmptyNonNilPayloads(t *testing.T) {
	reader := &fakeReader{} // zero StoreSnapshot
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink)
	startComposer(t, c)

	c.MarkDirty(true, true)
	b := recvBatch(t, sink.out)
	if b.Sessions == nil || b.Projects == nil {
		t.Fatalf("empty state must still produce both payloads: %#v", b)
	}
	if b.Sessions.Sessions == nil || len(b.Sessions.Sessions) != 0 {
		t.Fatalf("sessions must be an empty non-nil slice: %#v", b.Sessions.Sessions)
	}
}

func TestCloseStopsRunAndContextCancelStopsRun(t *testing.T) {
	reader := &fakeReader{}
	sink := &blockingSink{out: make(chan Batch, 8)}

	c := New(reader, nil, sink)
	done := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(done)
	}()
	c.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop Run")
	}
	// MarkDirty after Close must not panic or emit.
	c.MarkDirty(true, true)
	expectNoBatch(t, sink.out, 50*time.Millisecond)

	c2 := New(reader, nil, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		c2.Run(ctx)
		close(done2)
	}()
	cancel()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("context cancel did not stop Run")
	}
}

// TestVerdictOverlayNarrowsResumability pins the adapter-reconciliation
// verdict overlay: VerdictGone narrows a dead row to non-resumable; unknown
// and VerdictResumable keep the conservative default; a row without a
// recorded command is never resumable (production parity: it cannot be
// respawned); verdicts never affect live rows.
func TestVerdictOverlayNarrowsResumability(t *testing.T) {
	reader := &fakeReader{result: func(centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{Sessions: []centralstore.SessionView{
			{Session: centralstore.Session{ID: "confirmed", Command: []string{"sh"}, StartedAt: ptrMillis(1)}},
			{Session: centralstore.Session{ID: "gone", Command: []string{"sh"}, StartedAt: ptrMillis(1)}},
			{Session: centralstore.Session{ID: "live", Command: []string{"sh"}}},
			{Session: centralstore.Session{ID: "no-command", Command: []string{}, StartedAt: ptrMillis(1)}},
			{Session: centralstore.Session{ID: "unprobed", Command: []string{"sh"}, StartedAt: ptrMillis(1)}},
		}}, nil
	}}
	runtime := RuntimeSourceFunc(func() map[centralstore.SessionID]RuntimeFacts {
		return map[centralstore.SessionID]RuntimeFacts{"live": {PID: 1}}
	})
	verdicts := VerdictSourceFunc(func() map[centralstore.SessionID]ResumeVerdict {
		return map[centralstore.SessionID]ResumeVerdict{
			"confirmed": VerdictResumable,
			"gone":      VerdictGone,
			"live":      VerdictGone, // stale verdict must not narrow a live row
		}
	})
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, runtime, sink, WithVerdictSource(verdicts))
	startComposer(t, c)

	c.MarkDirty(true, false)
	b := recvBatch(t, sink.out)
	got := map[centralstore.SessionID][2]bool{} // alive, resumable
	for _, row := range b.Sessions.Sessions {
		got[row.ID] = [2]bool{row.Alive, row.Resumable}
	}
	want := map[centralstore.SessionID][2]bool{
		"confirmed":  {false, true},
		"gone":       {false, false},
		"live":       {true, false},
		"no-command": {false, false},
		"unprobed":   {false, true},
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s: got alive/resumable=%v want %v", id, got[id], w)
		}
	}
}

// TestNilVerdictSourceKeepsDefault: without a VerdictSource every dead row
// with a command stays a resume candidate.
func TestNilVerdictSourceKeepsDefault(t *testing.T) {
	reader := &fakeReader{result: func(centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{Sessions: []centralstore.SessionView{
			{Session: centralstore.Session{ID: "dead", Command: []string{"sh"}, StartedAt: ptrMillis(1)}},
		}}, nil
	}}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink)
	startComposer(t, c)
	c.MarkDirty(true, false)
	b := recvBatch(t, sink.out)
	if row := b.Sessions.Sessions[0]; row.Alive || !row.Resumable {
		t.Fatalf("row=%#v", row)
	}
}

func ptrMillis(v centralstore.UnixMillis) *centralstore.UnixMillis { return &v }

// TestNeverAliveDeadRowIsNotResumableAndDropsUnread pins the ever-alive
// gate: a dead row whose runner never reported running (started_at null —
// failed spawn / instant exec error) must not surface as a resume candidate
// and must not carry an unread badge, even with a command and no verdict.
func TestNeverAliveDeadRowIsNotResumableAndDropsUnread(t *testing.T) {
	reader := &fakeReader{result: func(centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error) {
		return centralstore.StoreSnapshot{Sessions: []centralstore.SessionView{
			{Session: centralstore.Session{ID: "never-ran", Command: []string{"sh"}, Unread: true}},
			{Session: centralstore.Session{ID: "ran", Command: []string{"sh"}, Unread: true, StartedAt: ptrMillis(1)}},
		}}, nil
	}}
	sink := &blockingSink{out: make(chan Batch, 8)}
	c := New(reader, nil, sink)
	startComposer(t, c)
	c.MarkDirty(true, false)
	b := recvBatch(t, sink.out)
	rows := b.Sessions.Sessions
	if len(rows) != 2 {
		t.Fatalf("rows=%#v", rows)
	}
	if never := rows[0]; never.Resumable || never.Unread {
		t.Fatalf("never-ran row must be gated: %#v", never)
	}
	if ran := rows[1]; !ran.Resumable || !ran.Unread {
		t.Fatalf("ever-ran row must keep resumable+unread: %#v", ran)
	}
}

func TestCloseJoinsInFlightComposeAndRejectsLaterInvalidation(t *testing.T) {
	gate := make(chan struct{})
	reader := &fakeReader{gate: gate}
	c := New(reader, RuntimeSourceFunc(func() map[centralstore.SessionID]RuntimeFacts { return nil }), SinkFunc(func(context.Context, Batch) {}))
	go c.Run(context.Background())
	c.MarkDirty(true, false)
	deadline := time.Now().Add(time.Second)
	for len(reader.calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while store read was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join composer")
	}
	before := len(reader.calls())
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); c.MarkDirty(true, true) }()
	}
	wg.Wait()
	time.Sleep(20 * time.Millisecond)
	if got := len(reader.calls()); got != before {
		t.Fatalf("post-Close reads=%d, want %d", got, before)
	}
}

func TestCloseBeforeRunIsJoinedAndIdempotent(t *testing.T) {
	c := New(&fakeReader{}, RuntimeSourceFunc(func() map[centralstore.SessionID]RuntimeFacts { return nil }), SinkFunc(func(context.Context, Batch) {}))
	c.Close()
	c.Close()
	done := make(chan struct{})
	go func() { c.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run after Close did not return")
	}
}
