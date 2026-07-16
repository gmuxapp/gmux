package sessioncoord

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

type fakeReconciler struct {
	mu    sync.Mutex
	fn    func(adapter string, batch []ReconcileCandidate) ([]ReconcileDecision, error)
	calls []struct {
		adapter string
		batch   []ReconcileCandidate
	}
}

func (r *fakeReconciler) ReconcileRetained(_ context.Context, adapter string, batch []ReconcileCandidate) ([]ReconcileDecision, error) {
	r.mu.Lock()
	r.calls = append(r.calls, struct {
		adapter string
		batch   []ReconcileCandidate
	}{adapter, append([]ReconcileCandidate(nil), batch...)})
	fn := r.fn
	r.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(adapter, batch)
}

func (r *fakeReconciler) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// closeBarrier opens and immediately closes the convergence window so
// reconciliation is unblocked.
func closeBarrier(t *testing.T, coord *Coordinator) {
	t.Helper()
	ctx := context.Background()
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.FinishConvergence(ctx, 1); err != nil {
		t.Fatal(err)
	}
}

func decide(d Disposition, ids ...centralstore.SessionID) []ReconcileDecision {
	out := make([]ReconcileDecision, len(ids))
	for i, id := range ids {
		out[i] = ReconcileDecision{ID: id, Disposition: d}
	}
	return out
}

func TestReconcilePreconditions(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	coord := newCoord(newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil)
	if _, err := coord.Reconcile(ctx); !errors.Is(err, ErrNoAdapterReconciler) {
		t.Fatalf("no reconciler: %v", err)
	}

	coord = New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, WithAdapterReconciler(&fakeReconciler{}))
	if _, err := coord.Reconcile(ctx); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("open barrier: %v", err)
	}
	if err := coord.BeginConvergence(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Reconcile(ctx); !errors.Is(err, ErrConvergencePending) {
		t.Fatalf("window still open: %v", err)
	}
}

func TestReconcileAppliesDispositions(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			deadSession("keep", "pi", "c-keep", 3),
			deadSession("drop", "pi", "c-drop", 4),
			deadSession("dunno", "pi", "c-dunno", 5),
			deadSession("undecided", "pi", "c-undecided", 6),
			deadSession("live-one", "pi", "c-live", 7),
		}, nil
	}
	rec := &fakeReconciler{fn: func(adapter string, batch []ReconcileCandidate) ([]ReconcileDecision, error) {
		return []ReconcileDecision{
			{ID: "keep", Disposition: DispositionRetain},
			{ID: "drop", Disposition: DispositionRemove},
			{ID: "dunno", Disposition: DispositionUnknown},
			{ID: "not-a-candidate", Disposition: DispositionRemove}, // unrequested: ignored
		}, nil
	}}
	sink := &fakeDirtySink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil, WithAdapterReconciler(rec))
	installLive(coord, "live-one", "ep-live")
	closeBarrier(t, coord)

	removed, err := coord.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "drop" {
		t.Fatalf("removed=%v", removed)
	}
	if len(dur.removeCalls) != 1 || dur.removeCalls[0] != "drop" {
		t.Fatalf("removeCalls=%v", dur.removeCalls)
	}
	// The live row was never offered to the adapter.
	for _, call := range rec.calls {
		for _, cand := range call.batch {
			if cand.ID == "live-one" {
				t.Fatal("live row must not be a candidate")
			}
			if cand.ID == "drop" && cand.Version != 4 {
				t.Fatalf("candidate version=%d", cand.Version)
			}
		}
	}
	verdicts := coord.ResumeVerdicts()
	if verdicts["keep"] != VerdictResumable {
		t.Fatalf("keep verdict=%v", verdicts["keep"])
	}
	for _, id := range []centralstore.SessionID{"drop", "dunno", "undecided", "live-one"} {
		if v, ok := verdicts[id]; ok {
			t.Fatalf("%s should carry no verdict, got %v", id, v)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("published=%d", sink.count())
	}
}

func TestReconcileProbeFailureRetainsAndClearsVerdicts(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession("a", "pi", "c-a", 3)}, nil
	}
	rec := &fakeReconciler{fn: func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		return decide(DispositionRetain, "a"), nil
	}}
	errs := &fakeErrorSink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, errs, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if coord.ResumeVerdicts()["a"] != VerdictResumable {
		t.Fatal("first pass should confirm resumable")
	}

	rec.mu.Lock()
	rec.fn = func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		return nil, errors.New("storage unplugged")
	}
	rec.mu.Unlock()
	removed, err := coord.Reconcile(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if len(dur.removeCalls) != 0 {
		t.Fatal("probe failure must not remove anything")
	}
	if errs.count() != 1 {
		t.Fatalf("errors=%d", errs.count())
	}
	if _, ok := coord.ResumeVerdicts()["a"]; ok {
		t.Fatal("an unreachable probe clears the stale verdict back to unknown")
	}
}

func TestReconcileRemoveConditionalOnVersion(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession("a", "pi", "c-a", 3)}, nil
	}
	dur.removeResult = func(centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{SessionVersion: 4}, centralstore.ErrStaleVersion
	}
	rec := &fakeReconciler{fn: func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		return decide(DispositionRemove, "a"), nil
	}}
	sink := &fakeDirtySink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, sink, nil, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	removed, err := coord.Reconcile(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if sink.count() != 0 {
		t.Fatal("a skipped removal publishes nothing")
	}
	if _, ok := coord.ResumeVerdicts()["a"]; ok {
		t.Fatal("stale decision drops the verdict back to unknown")
	}
}

// TestReconcileAdapterConfirmedRemoveDBFailureKeepsGoneVerdict pins the
// semantic boundary of VerdictGone: it means ADAPTER-confirmed
// non-resumable, so an adapter Remove disposition whose conditional removal
// hits a database error keeps Gone to narrow the overlay while a later pass
// retries. (Contrast the covered-row variant below, where Gone is never
// set.)
func TestReconcileAdapterConfirmedRemoveDBFailureKeepsGoneVerdict(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession("a", "pi", "c-a", 3)}, nil
	}
	dur.removeResult = func(centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{}, errors.New("disk full")
	}
	rec := &fakeReconciler{fn: func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		return decide(DispositionRemove, "a"), nil
	}}
	errs := &fakeErrorSink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, errs, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if errs.count() != 1 {
		t.Fatalf("errors=%d", errs.count())
	}
	if coord.ResumeVerdicts()["a"] != VerdictGone {
		t.Fatal("adapter-confirmed Gone survives a failed removal to narrow the overlay")
	}
}

// TestReconcileCoveredRowRemoveDBFailureLeavesUnknown pins design M-2: a
// takeover-coverage removal is NOT adapter-confirmed — when it fails on a
// database error the row must carry no VerdictGone (the adapter said
// Retain), so the overlay keeps the conservative resumable default.
func TestReconcileCoveredRowRemoveDBFailureLeavesUnknown(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			{ID: "owner", Version: 1, Adapter: "pi", ConversationRef: "R", Command: []string{"pi"}},
			deadSession("loser", "pi", "R", 3),
		}, nil
	}
	dur.removeResult = func(centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{}, errors.New("disk full")
	}
	rec := &fakeReconciler{fn: func(_ string, batch []ReconcileCandidate) ([]ReconcileDecision, error) {
		return decide(DispositionRetain, "loser"), nil
	}}
	errs := &fakeErrorSink{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, errs,
		WithAdapterReconciler(rec), WithConversationTakeover(nil))
	installLive(coord, "owner", "ep-owner")
	closeBarrier(t, coord)

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if errs.count() != 1 {
		t.Fatalf("errors=%d", errs.count())
	}
	if v, ok := coord.ResumeVerdicts()["loser"]; ok {
		t.Fatalf("covered-row failure must not record a verdict, got %v", v)
	}
}

// TestReconcileCoverageInvalidatedWhenOwnerDiesMidProbe pins fable M1:
// takeover coverage computed at gather is re-validated at apply — an owner
// whose live generation vanished during the probe phase must not cause
// deletion of a row the adapter explicitly retained.
func TestReconcileCoverageInvalidatedWhenOwnerDiesMidProbe(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			{ID: "owner", Version: 1, Adapter: "pi", ConversationRef: "R", Command: []string{"pi"}},
			deadSession("loser", "pi", "R", 3),
		}, nil
	}
	var coord *Coordinator
	rec := &fakeReconciler{}
	rec.fn = func(_ string, batch []ReconcileCandidate) ([]ReconcileDecision, error) {
		// The covering owner dies (leaves the registry) mid-probe.
		coord.registry.remove("owner", 999)
		return decide(DispositionRetain, "loser"), nil
	}
	coord = New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil,
		WithAdapterReconciler(rec), WithConversationTakeover(nil))
	installLive(coord, "owner", "ep-owner")
	closeBarrier(t, coord)

	removed, err := coord.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 || len(dur.removeCalls) != 0 {
		t.Fatalf("stale coverage must not remove a retained row: removed=%v calls=%v", removed, dur.removeCalls)
	}
	// Coverage degraded to the adapter's disposition.
	if coord.ResumeVerdicts()["loser"] != VerdictResumable {
		t.Fatalf("verdicts=%v", coord.ResumeVerdicts())
	}
}

// TestReconcileSkipsVerdictWritesInvalidatedMidPass pins fable L1: a
// registration during the probe phase invalidates the verdict; the in-flight
// pass must not re-set a stale one afterwards — even on the removal-DB-error
// path that would otherwise keep VerdictGone.
func TestReconcileSkipsVerdictWritesInvalidatedMidPass(t *testing.T) {
	ctx := context.Background()
	id := sid(11)
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession(id, "pi", "c", 3)}, nil
	}
	dur.removeResult = func(centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{}, errors.New("disk full")
	}
	// The runner re-registers fast-dead during the probe phase.
	meta := liveMeta(id, "pi", "c")
	meta.Registration.Alive = false
	meta.Registration.Facts.ExitedAt = exitedAt(9)
	client := newFakeClient(meta)
	var coord *Coordinator
	rec := &fakeReconciler{}
	rec.fn = func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		if _, err := coord.Register(ctx, RegisterRequest{Endpoint: "ep"}); err != nil {
			t.Errorf("mid-probe register: %v", err)
		}
		return decide(DispositionRemove, id), nil
	}
	coord = New(nil, client, dur, &fakeDirtySink{}, &fakeErrorSink{}, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if v, ok := coord.ResumeVerdicts()[id]; ok {
		t.Fatalf("invalidated verdict re-set by the in-flight pass: %v", v)
	}
}

func TestReconcileSkipsRowThatWentLiveOrClaimedDuringProbe(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			deadSession("went-live", "pi", "c-a", 3),
			deadSession("got-claimed", "pi", "c-b", 4),
		}, nil
	}
	var coord *Coordinator
	rec := &fakeReconciler{}
	rec.fn = func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		// Between gather and apply: one row goes live, one gains a claim.
		installLive(coord, "went-live", "ep-x")
		coord.mu.Lock()
		coord.ops["got-claimed"] = &LifecycleClaim{op: "resume"}
		coord.mu.Unlock()
		return decide(DispositionRemove, "went-live", "got-claimed"), nil
	}
	coord = New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	removed, err := coord.Reconcile(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if len(dur.removeCalls) != 0 {
		t.Fatalf("removeCalls=%v", dur.removeCalls)
	}
	if len(coord.ResumeVerdicts()) != 0 {
		t.Fatalf("verdicts=%v", coord.ResumeVerdicts())
	}
}

func TestReconcileClaimedRowIsNotACandidate(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession("claimed", "pi", "c-a", 3)}, nil
	}
	rec := &fakeReconciler{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, WithAdapterReconciler(rec))
	closeBarrier(t, coord)
	coord.mu.Lock()
	coord.ops["claimed"] = &LifecycleClaim{op: "resume"}
	coord.mu.Unlock()

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.callCount() != 0 {
		t.Fatal("a claimed row must never reach the adapter")
	}
}

func TestReconcileBatchesPerAdapterInOrder(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			deadSession("p1", "pi", "", 1),
			deadSession("p2", "pi", "", 1),
			deadSession("p3", "pi", "", 1),
			deadSession("s1", "shell", "", 1),
		}, nil
	}
	rec := &fakeReconciler{}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil,
		WithAdapterReconciler(rec), WithReconcileBatchSize(2))
	closeBarrier(t, coord)

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 3 {
		t.Fatalf("calls=%d", len(rec.calls))
	}
	if rec.calls[0].adapter != "pi" || len(rec.calls[0].batch) != 2 ||
		rec.calls[1].adapter != "pi" || len(rec.calls[1].batch) != 1 ||
		rec.calls[2].adapter != "shell" || len(rec.calls[2].batch) != 1 {
		t.Fatalf("batching=%#v", rec.calls)
	}
}

func TestReconcileSingleFlight(t *testing.T) {
	ctx := context.Background()
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession("a", "pi", "", 1)}, nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	rec := &fakeReconciler{fn: func(string, []ReconcileCandidate) ([]ReconcileDecision, error) {
		close(entered)
		<-release
		return nil, nil
	}}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	done := make(chan error, 1)
	go func() {
		_, err := coord.Reconcile(ctx)
		done <- err
	}()
	<-entered
	if _, err := coord.Reconcile(ctx); !errors.Is(err, ErrReconcileInFlight) {
		t.Fatalf("second pass: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The flag is released: a later pass runs.
	rec.mu.Lock()
	rec.fn = nil
	rec.mu.Unlock()
	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatalf("third pass: %v", err)
	}
}

func TestReconcileTakeoverConvergenceEvictsCoveredDeadRows(t *testing.T) {
	ctx := context.Background()
	resolver := &fakeResolver{infos: map[string]ConversationInfo{
		lineageKey("pi", "R"):  {ID: "cR", AncestorIDs: []string{"c2"}},
		lineageKey("pi", "R2"): {ID: "c2"},
	}}
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{
			{ID: "owner", Version: 1, Adapter: "pi", ConversationRef: "R", Command: []string{"pi"}},
			deadSession("equal-ref", "pi", "R", 3),
			deadSession("ancestor", "pi", "R2", 4),
			deadSession("unrelated", "pi", "other", 5),
		}, nil
	}
	// The adapter retains everything — coverage by a live binder wins anyway.
	rec := &fakeReconciler{fn: func(_ string, batch []ReconcileCandidate) ([]ReconcileDecision, error) {
		out := make([]ReconcileDecision, len(batch))
		for i, cand := range batch {
			out[i] = ReconcileDecision{ID: cand.ID, Disposition: DispositionRetain}
		}
		return out, nil
	}}
	coord := New(nil, newFakeClient(RunnerMeta{}), dur, &fakeDirtySink{}, nil,
		WithAdapterReconciler(rec), WithConversationTakeover(resolver))
	installLive(coord, "owner", "ep-owner")
	closeBarrier(t, coord)

	removed, err := coord.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[centralstore.SessionID]bool{}
	for _, id := range removed {
		got[id] = true
	}
	if !got["equal-ref"] || !got["ancestor"] || got["unrelated"] || len(removed) != 2 {
		t.Fatalf("removed=%v", removed)
	}
	if coord.ResumeVerdicts()["unrelated"] != VerdictResumable {
		t.Fatal("uncovered retained row keeps its Resumable verdict")
	}
}

func TestRegistrationInvalidatesVerdict(t *testing.T) {
	ctx := context.Background()
	id := sid(9)
	dur := newFakeDurable(0)
	dur.listSessions = func() ([]centralstore.Session, error) {
		return []centralstore.Session{deadSession(id, "pi", "c", 3)}, nil
	}
	rec := &fakeReconciler{fn: func(_ string, batch []ReconcileCandidate) ([]ReconcileDecision, error) {
		return decide(DispositionRetain, id), nil
	}}
	client := newFakeClient(liveMeta(id, "pi", "c"))
	coord := New(nil, client, dur, &fakeDirtySink{}, nil, WithAdapterReconciler(rec))
	closeBarrier(t, coord)

	if _, err := coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if coord.ResumeVerdicts()[id] != VerdictResumable {
		t.Fatal("verdict not recorded")
	}
	if _, err := coord.Register(ctx, RegisterRequest{Endpoint: "ep"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := coord.ResumeVerdicts()[id]; ok {
		t.Fatal("registration must invalidate the verdict")
	}
}
