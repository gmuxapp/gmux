package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// The tests in this file cover the two bounds that keep the startup converge
// pass survivable on a long-lived install: the takeover lineage warm is capped
// in time (lineageWarmBudget) and deduplicated across concurrent
// registrations (lineageCache single-flight). Without either, N concurrent
// registrations perform O(N × same-adapter refs) identical conversation
// describes and every one of them burns its whole runner budget warming
// instead of installing.

// warmFleet is a runner transport whose /meta identity is derived from the
// endpoint, so a test can register many distinct sessions concurrently.
type warmFleet struct{ adapter string }

func (warmFleet) Subscribe(context.Context, string) (EventStream, error) {
	return newFakeStream(), nil
}

func (f warmFleet) Meta(_ context.Context, ep string) (RunnerMeta, error) {
	id := centralstore.SessionID(ep)
	return RunnerMeta{PID: 1, Registration: centralstore.RunnerRegistration{ID: id, Adapter: f.adapter, Alive: true, CreatedAt: 1, ObservedAt: 1}}, nil
}

// countingResolver answers describes after a per-ref delay and records how
// many times each ref was described. delay honors ctx so it models real
// adapter I/O under a deadline; hang blocks until ctx expires.
type countingResolver struct {
	delay time.Duration
	hang  string

	mu    sync.Mutex
	calls map[string]int
}

func (r *countingResolver) DescribeConversation(ctx context.Context, adapter, ref string) (ConversationInfo, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = make(map[string]int)
	}
	r.calls[ref]++
	r.mu.Unlock()
	wait := r.delay
	if ref == r.hang {
		wait = time.Hour
	}
	select {
	case <-ctx.Done():
		return ConversationInfo{}, ctx.Err()
	case <-time.After(wait):
	}
	return ConversationInfo{ID: "conv-" + ref}, nil
}

func (r *countingResolver) maxCalls() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	worstRef, worst := "", 0
	for ref, n := range r.calls {
		if n > worst {
			worstRef, worst = ref, n
		}
	}
	return worstRef, worst
}

// warmCoord builds a coordinator whose durable list contains refs
// same-adapter dead rows, i.e. the warm universe of every registration.
func warmCoord(t *testing.T, refs int, resolver ConversationResolver) *Coordinator {
	t.Helper()
	rows := make([]centralstore.Session, 0, refs)
	for i := 0; i < refs; i++ {
		rows = append(rows, centralstore.Session{
			ID:              centralstore.SessionID(fmt.Sprintf("d%07d", i)),
			Adapter:         "pi",
			ConversationRef: fmt.Sprintf("/conversations/%07d.jsonl", i),
		})
	}
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) { return rows, nil }
	durable.registerResult = func(reg centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return centralstore.Session{ID: reg.ID, Adapter: reg.Adapter, Version: 1}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
	}
	c := New(nil, warmFleet{adapter: "pi"}, durable, &fakeDirtySink{}, nil, WithConversationTakeover(resolver))
	t.Cleanup(c.Close)
	return c
}

// registerAll registers n endpoints concurrently under one per-registration
// budget, mirroring the startup converge pass, and returns their errors.
func registerAll(c *Coordinator, n int, budget time.Duration) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			_, errs[i] = c.Register(ctx, RegisterRequest{Endpoint: fmt.Sprintf("r%07d", i)})
		}(i)
	}
	wg.Wait()
	return errs
}

// TestRegisterInstallsDespiteLargeSameAdapterRefUniverse is the at-scale
// regression: 400 same-adapter refs at 20ms per describe is 8s of takeover
// bookkeeping, far more than any sane per-runner budget, yet every
// registration must still install well inside that budget. On the unbounded
// warm every one of them failed with a deadline instead, which is exactly the
// "converge pass abandoned all N runners in the same second" defect.
func TestRegisterInstallsDespiteLargeSameAdapterRefUniverse(t *testing.T) {
	const runners, refs = 8, 400
	resolver := &countingResolver{delay: 20 * time.Millisecond}
	c := warmCoord(t, refs, resolver)

	budget := 4 * time.Second // a generous runner budget; the warm cap is 1.5s
	start := time.Now()
	errs := registerAll(c, runners, budget)
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("registration %d abandoned after %s: %v", i, elapsed, err)
		}
	}
	if got := len(c.registry.Snapshot()); got != runners {
		t.Fatalf("installed generations = %d, want %d", got, runners)
	}
	// The warm cap, not the runner budget, must be what ends the bookkeeping.
	if elapsed >= budget {
		t.Fatalf("registrations took %s, i.e. they spent the whole runner budget warming", elapsed)
	}
	if elapsed > lineageWarmBudget+2*time.Second {
		t.Fatalf("registrations took %s, warm budget is %s", elapsed, lineageWarmBudget)
	}
}

// TestLineageWarmDescribesEachRefOnceUnderConcurrentRegistrations pins the
// single-flight: concurrent registrations share one describe per ref instead
// of each repeating the whole universe (the O(N × refs) hazard).
func TestLineageWarmDescribesEachRefOnceUnderConcurrentRegistrations(t *testing.T) {
	const runners, refs = 8, 40
	resolver := &countingResolver{delay: 5 * time.Millisecond}
	c := warmCoord(t, refs, resolver)

	for i, err := range registerAll(c, runners, 4*time.Second) {
		if err != nil {
			t.Fatalf("registration %d failed: %v", i, err)
		}
	}
	if ref, n := resolver.maxCalls(); n > 1 {
		t.Fatalf("ref %s described %d times by %d concurrent registrations; single-flight lost", ref, n, runners)
	}
	// The shared work must still have happened exactly once and be cached.
	if _, ok := c.lineage.get("pi", "/conversations/0000000.jsonl"); !ok {
		t.Fatal("single-flight left the shared describe uncached")
	}
}

// TestLineageWarmHungResolverDoesNotWedgeRegistrations proves the waiting
// introduced by single-flight is bounded: one ref whose describe never returns
// must not hold a registration past the warm budget, and must not hold the
// other registrations at all.
func TestLineageWarmHungResolverDoesNotWedgeRegistrations(t *testing.T) {
	const runners, refs = 4, 6
	resolver := &countingResolver{hang: "/conversations/0000000.jsonl"}
	c := warmCoord(t, refs, resolver)

	budget := 10 * time.Second
	start := time.Now()
	errs := registerAll(c, runners, budget)
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("registration %d wedged behind a hung describe: %v", i, err)
		}
	}
	if elapsed > lineageWarmBudget+2*time.Second {
		t.Fatalf("hung describe held registrations for %s (warm budget %s)", elapsed, lineageWarmBudget)
	}
	if got := len(c.registry.Snapshot()); got != runners {
		t.Fatalf("installed generations = %d, want %d", got, runners)
	}
}

// panickingResolver panics on one ref, the way a malformed transcript can
// take out an adapter parser.
type panickingResolver struct {
	panicOn string
	calls   atomic.Int64
}

func (r *panickingResolver) DescribeConversation(_ context.Context, _, ref string) (ConversationInfo, error) {
	r.calls.Add(1)
	if ref == r.panicOn {
		panic("resolver blew up on " + ref)
	}
	return ConversationInfo{ID: "conv-" + ref}, nil
}

// TestLineageWarmPanickingResolverDoesNotPoisonTheCache pins that a panicking
// describe releases its single-flight claim. A retained claim whose channel
// never closes would make every later warm block on that key until its own
// deadline — and reconcile's warm, which runs under the daemon context, would
// block on it forever, silently stopping the periodic reap/retention pass for
// the life of the daemon. net/http recovers per connection, so the daemon
// survives such a panic; it must not survive it in a poisoned state.
func TestLineageWarmPanickingResolverDoesNotPoisonTheCache(t *testing.T) {
	const refs = 6
	poison := "/conversations/0000000.jsonl"
	resolver := &panickingResolver{panicOn: poison}
	c := warmCoord(t, refs, resolver)

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("resolver did not panic; test no longer exercises the panic path")
			}
		}()
		c.lineage.warm(context.Background(), resolver, "pi", []string{poison})
	}()

	c.lineage.mu.Lock()
	leaked := len(c.lineage.inflight)
	c.lineage.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("panicking describe leaked %d single-flight claim(s)", leaked)
	}

	// The cache is usable again: a later registration warms every remaining
	// ref instead of blocking on the poisoned key. (The poisoned ref itself
	// keeps panicking, which is the resolver's problem, not the cache's.)
	resolver.panicOn = ""
	start := time.Now()
	if err := errors.Join(registerAll(c, 1, 4*time.Second)...); err != nil {
		t.Fatalf("registration after a panicking describe: %v", err)
	}
	if elapsed := time.Since(start); elapsed > lineageWarmBudget {
		t.Fatalf("registration after a panicking describe took %s: it waited on a poisoned claim", elapsed)
	}
	for i := 0; i < refs; i++ {
		ref := fmt.Sprintf("/conversations/%07d.jsonl", i)
		if _, ok := c.lineage.get("pi", ref); !ok {
			t.Fatalf("ref %s stayed uncached after the poisoned key was released", ref)
		}
	}
}

// TestReconcileWarmIsBounded pins the second warm site. Reconcile runs on the
// startup path (before the TCP/tailnet listeners bind and before conversation
// indexing) and on every periodic tick, under the daemon context — i.e. with
// no deadline of its own. Left unbounded it re-walks the whole O(history) ref
// universe there, which is how the startup cost simply moved from convergence
// to post-convergence.
func TestReconcileWarmIsBounded(t *testing.T) {
	ctx := context.Background()
	const refs = 400
	rows := make([]centralstore.Session, 0, refs)
	for i := 0; i < refs; i++ {
		rows = append(rows, deadSession(centralstore.SessionID(fmt.Sprintf("d%07d", i)), "pi", fmt.Sprintf("/conversations/%07d.jsonl", i), centralstore.RowVersion(i+1)))
	}
	durable := newFakeDurable(1)
	durable.listSessions = func() ([]centralstore.Session, error) { return rows, nil }
	resolver := &countingResolver{delay: 20 * time.Millisecond} // 8s for the universe
	rec := &fakeReconciler{}
	c := New(nil, warmFleet{adapter: "pi"}, durable, &fakeDirtySink{}, nil, WithConversationTakeover(resolver), WithAdapterReconciler(rec))
	defer c.Close()
	closeBarrier(t, c)

	start := time.Now()
	if _, _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed > lineageWarmBudget+2*time.Second {
		t.Fatalf("reconcile spent %s warming an unbounded ref universe (budget %s)", elapsed, lineageWarmBudget)
	}
	if rec.callCount() == 0 {
		t.Fatal("reconcile never reached the adapter probe")
	}
}
