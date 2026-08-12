package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func budgetSession(id, adapter string, parent *centralstore.SessionID, promoted bool) centralstore.Session {
	return centralstore.Session{ID: centralstore.SessionID(id), Adapter: adapter, ParentSessionID: parent, PromotedToRoot: promoted}
}

func budgetParent(id string) *centralstore.SessionID {
	v := centralstore.SessionID(id)
	return &v
}

func TestActiveSubagentRootAndCountTable(t *testing.T) {
	rows := []centralstore.Session{
		budgetSession("root", "shell", nil, false),
		budgetSession("agent", "pi", budgetParent("root"), false),
		budgetSession("nested", "pi", budgetParent("agent"), false),
		budgetSession("process", "shell", budgetParent("root"), false),
		budgetSession("dead", "pi", budgetParent("root"), false),
		budgetSession("promoted", "pi", budgetParent("root"), true),
		budgetSession("promoted-child", "pi", budgetParent("promoted"), false),
		budgetSession("orphan", "pi", budgetParent("remote@peer"), false),
		budgetSession("orphan-child", "pi", budgetParent("orphan"), false),
		budgetSession("cycle-a", "pi", budgetParent("cycle-b"), false),
		budgetSession("cycle-b", "pi", budgetParent("cycle-a"), false),
		budgetSession("cycle-child", "pi", budgetParent("cycle-b"), false),
	}
	b := newActiveSubagentBudget(8, func(adapter string) bool { return adapter == "pi" }, rows)
	for _, id := range []centralstore.SessionID{"agent", "nested", "process", "promoted", "promoted-child", "orphan", "orphan-child", "cycle-a", "cycle-b", "cycle-child"} {
		b.setLive(id, true)
	}

	roots := map[centralstore.SessionID]centralstore.SessionID{
		"root": "root", "agent": "root", "nested": "root", "process": "root", "dead": "root",
		"promoted": "promoted", "promoted-child": "promoted",
		"orphan": "orphan", "orphan-child": "orphan",
		"cycle-a": "cycle-a", "cycle-b": "cycle-a", "cycle-child": "cycle-a",
	}
	for id, want := range roots {
		if got := b.roots[id]; got != want {
			t.Errorf("root(%s) = %s, want %s", id, got, want)
		}
	}
	wantCounts := map[centralstore.SessionID]int{"root": 2, "promoted": 1, "orphan": 1, "cycle-a": 2}
	for root, want := range wantCounts {
		if got := b.activeByRoot[root]; got != want {
			t.Errorf("active[%s] = %d, want %d", root, got, want)
		}
	}
	if _, exists := b.nodes["remote@peer"]; exists {
		t.Fatal("remote projection entered the local budget index")
	}
}

func TestActiveSubagentMutableOwnershipTransitions(t *testing.T) {
	rows := []centralstore.Session{
		budgetSession("a", "shell", nil, false),
		budgetSession("b", "shell", nil, false),
		budgetSession("child", "pi", budgetParent("a"), false),
		budgetSession("grand", "pi", budgetParent("child"), false),
	}
	b := newActiveSubagentBudget(8, func(adapter string) bool { return adapter == "pi" }, rows)
	b.setLive("child", true)
	b.setLive("grand", true)
	if b.activeByRoot["a"] != 2 {
		t.Fatalf("initial counts = %v", b.activeByRoot)
	}

	b.setParent("child", budgetParent("b"))
	if b.activeByRoot["a"] != 0 || b.activeByRoot["b"] != 2 {
		t.Fatalf("reparent counts = %v", b.activeByRoot)
	}
	b.setPromotion("child", true)
	if b.activeByRoot["b"] != 0 || b.activeByRoot["child"] != 1 {
		t.Fatalf("promotion counts = %v", b.activeByRoot)
	}
	b.setPromotion("child", false)
	if b.activeByRoot["b"] != 2 || b.activeByRoot["child"] != 0 {
		t.Fatalf("demotion counts = %v", b.activeByRoot)
	}
	b.setLive("grand", false)
	if b.activeByRoot["b"] != 1 {
		t.Fatalf("termination counts = %v", b.activeByRoot)
	}
}

func coordinatorAtSeven(t *testing.T) *Coordinator {
	t.Helper()
	rows := []centralstore.Session{budgetSession("root", "shell", nil, false)}
	for i := 0; i < 7; i++ {
		rows = append(rows, budgetSession(fmt.Sprintf("agent-%d", i), "pi", budgetParent("root"), false))
	}
	c := New(nil, nil, nil, nil, nil, WithActiveSubagentBudget(8, func(adapter string) bool { return adapter == "pi" }, rows))
	c.mu.Lock()
	for i := 0; i < 7; i++ {
		c.activeSubagents.setLive(centralstore.SessionID(fmt.Sprintf("agent-%d", i)), true)
	}
	c.mu.Unlock()
	return c
}

func TestConcurrentActiveSubagentAdmissionAtSevenOfEight(t *testing.T) {
	c := coordinatorAtSeven(t)
	parent := centralstore.SessionID("root")
	const attempts = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes []ActiveSubagentReservation
	failures := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reservation, err := c.ReserveActiveSubagent(context.Background(), &parent)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes = append(successes, reservation)
				return
			}
			if !errors.Is(err, ErrSubagentLimitReached) {
				t.Errorf("admission error = %v", err)
			}
			failures++
		}()
	}
	close(start)
	wg.Wait()
	if len(successes) != 1 || failures != attempts-1 {
		t.Fatalf("successes=%d failures=%d", len(successes), failures)
	}
	c.mu.Lock()
	if got := len(c.activeSubagents.launches); got != 1 {
		t.Fatalf("reservation state = %d, want one successful launch only", got)
	}
	// Convert the sole receipt into the eighth live descendant, as Register
	// does under this same mutex, and prove the root never exceeded eight.
	child := budgetSession("agent-7", "pi", &parent, false)
	c.activeSubagents.upsert(child, true)
	c.activeSubagents.release(successes[0].Token, false)
	if got := c.activeSubagents.activeByRoot[parent]; got != 8 {
		t.Fatalf("live descendants = %d, want 8", got)
	}
	if got := len(c.activeSubagents.launches); got != 0 {
		t.Fatalf("leaked launch state = %d", got)
	}
	c.mu.Unlock()
}

func TestConcurrentActiveSubagentAdmissionAtEightOfEight(t *testing.T) {
	c := coordinatorAtSeven(t)
	parent := centralstore.SessionID("root")
	c.mu.Lock()
	c.activeSubagents.upsert(budgetSession("agent-7", "pi", &parent, false), true)
	c.mu.Unlock()

	const attempts = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := c.ReserveActiveSubagent(context.Background(), &parent); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if successes.Load() != 0 {
		t.Fatalf("successes = %d, want 0", successes.Load())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.activeSubagents.launches) != 0 {
		t.Fatalf("failed admissions leaked: %d", len(c.activeSubagents.launches))
	}
}

func TestReservedLaunchFollowsReparentAndBlocksDismiss(t *testing.T) {
	rootA, rootB, caller := centralstore.SessionID("root-a"), centralstore.SessionID("root-b"), centralstore.SessionID("caller")
	rows := []centralstore.Session{
		{ID: rootA, Adapter: "shell"}, {ID: rootB, Adapter: "shell"},
		{ID: caller, Adapter: "shell", ParentSessionID: &rootA},
	}
	durable := newFakeDurable(0)
	durable.listSessions = func() ([]centralstore.Session, error) { return rows, nil }
	coord := New(nil, nil, durable, nil, nil, WithActiveSubagentBudget(1, func(a string) bool { return a == "pi" }, rows))
	first, err := coord.ReserveActiveSubagent(context.Background(), &caller)
	if err != nil {
		t.Fatal(err)
	}
	coord.mu.Lock()
	coord.activeSubagents.setParent(caller, &rootB)
	coord.mu.Unlock()
	if _, err := coord.ReserveActiveSubagent(context.Background(), &rootB); !errors.Is(err, ErrSubagentLimitReached) {
		t.Fatalf("reservation did not move with caller: %v", err)
	}
	if _, err := coord.ReserveActiveSubagent(context.Background(), &rootA); err != nil {
		t.Fatalf("old root did not regain slot: %v", err)
	}
	if _, err := coord.Dismiss(context.Background(), caller); !errors.Is(err, ErrSubtreeBusy) {
		t.Fatalf("dismiss during launch = %v, want ErrSubtreeBusy", err)
	}
	coord.ReleaseActiveSubagentReservation(first.Token)
}

func TestClaimedLaunchRechecksBudgetAfterReparent(t *testing.T) {
	rootA, rootB, caller := centralstore.SessionID("root-a"), centralstore.SessionID("root-b"), centralstore.SessionID("caller")
	rows := []centralstore.Session{{ID: rootA, Adapter: "shell"}, {ID: rootB, Adapter: "shell"}, {ID: caller, Adapter: "shell", ParentSessionID: &rootA}, budgetSession("full", "pi", &rootB, false)}
	b := newActiveSubagentBudget(1, func(a string) bool { return a == "pi" }, rows)
	b.setLive("full", true)
	reservation, err := b.reserve(&caller)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := b.claim(reservation.Token, "child")
	if err != nil {
		t.Fatal(err)
	}
	b.setParent(caller, &rootB)
	if err := b.validateClaimedBudget(launch); !errors.Is(err, ErrSubagentLimitReached) {
		t.Fatalf("reparented claim validation = %v, want limit", err)
	}
}

func TestMissingNodeMutationsDoNotCorruptDescendantCounts(t *testing.T) {
	root, missing, child := centralstore.SessionID("root"), centralstore.SessionID("missing"), centralstore.SessionID("child")
	b := newActiveSubagentBudget(1, func(a string) bool { return a == "pi" }, []centralstore.Session{{ID: root, Adapter: "shell"}, budgetSession(string(child), "pi", &missing, false)})
	b.setLive(child, true)
	before := b.activeByRoot[child]
	b.setParent(missing, &root)
	b.setPromotion(missing, true)
	b.remove(missing)
	if got := b.activeByRoot[child]; got != before {
		t.Fatalf("count changed from %d to %d", before, got)
	}
}

func TestActiveSubagentLaunchFailureReleasesSlot(t *testing.T) {
	c := coordinatorAtSeven(t)
	parent := centralstore.SessionID("root")
	reservation, err := c.ReserveActiveSubagent(context.Background(), &parent)
	if err != nil {
		t.Fatal(err)
	}
	c.ReleaseActiveSubagentReservation(reservation.Token)
	if _, err := c.ReserveActiveSubagent(context.Background(), &parent); err != nil {
		t.Fatalf("slot not reusable after pre-start failure: %v", err)
	}
}

func TestActiveSubagentRegistrationConsumesAndTerminationReleasesSlot(t *testing.T) {
	parent := centralstore.SessionID("root0000")
	childID := centralstore.SessionID("child000")
	rows := []centralstore.Session{{ID: parent, Adapter: "shell"}}
	for i := 0; i < 7; i++ {
		rows = append(rows, budgetSession(fmt.Sprintf("live%04d", i), "pi", &parent, false))
	}
	meta := liveMeta(childID, "pi", "")
	meta.Registration.ParentSessionID = &parent
	client := newFakeClient(meta)
	durable := newFakeDurable(0)
	durable.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return centralstore.Session{ID: childID, Version: 1, Adapter: "pi", ParentSessionID: &parent}, centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 1}, nil
	}
	durable.applyResult = func(centralstore.RunnerObservation) (centralstore.MutationResult, error) {
		return centralstore.MutationResult{Changed: true, SessionsDirty: true, SessionVersion: 2}, nil
	}
	coord := New(nil, client, durable, nil, nil, WithActiveSubagentBudget(8, func(a string) bool { return a == "pi" }, rows))
	coord.mu.Lock()
	for i := 0; i < 7; i++ {
		coord.activeSubagents.setLive(centralstore.SessionID(fmt.Sprintf("live%04d", i)), true)
	}
	coord.mu.Unlock()
	reservation, err := coord.ReserveActiveSubagent(context.Background(), &parent)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep", AssertedID: childID, ActiveSubagentReservation: reservation.Token})
	if err != nil {
		t.Fatal(err)
	}
	coord.mu.Lock()
	if got := coord.activeSubagents.activeByRoot[parent]; got != 8 {
		t.Fatalf("after register active=%d", got)
	}
	if len(coord.activeSubagents.launches) != 0 {
		t.Fatalf("receipt not consumed: %v", coord.activeSubagents.launches)
	}
	coord.mu.Unlock()

	exit := centralstore.UnixMillis(10)
	alive := false
	coord.apply(context.Background(), childID, runtime.Generation, RunnerEvent{ObservedAt: exit, Alive: &alive, Facts: centralstore.RunnerFacts{ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exit}}})
	coord.mu.Lock()
	if got := coord.activeSubagents.activeByRoot[parent]; got != 7 {
		t.Fatalf("after termination active=%d", got)
	}
	coord.mu.Unlock()
}

func TestActiveSubagentRegistrationPanicUnlocksAndPreservesReceipt(t *testing.T) {
	parent, id := centralstore.SessionID("root0000"), centralstore.SessionID("child000")
	meta := liveMeta(id, "pi", "")
	meta.Registration.ParentSessionID = &parent
	client := newFakeClient(meta)
	durable := newFakeDurable(0)
	durable.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		panic("boom")
	}
	coord := New(nil, client, durable, nil, nil, WithActiveSubagentBudget(1, func(a string) bool { return a == "pi" }, []centralstore.Session{{ID: parent, Adapter: "shell"}}))
	reservation, err := coord.ReserveActiveSubagent(context.Background(), &parent)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Register did not panic")
			}
		}()
		_, _ = coord.Register(context.Background(), RegisterRequest{Endpoint: "ep", AssertedID: id, ActiveSubagentReservation: reservation.Token})
	}()
	// A post-recovery mutex operation must complete, and the original receipt
	// remains the sole counted reservation rather than leaking a claimed state.
	coord.mu.Lock()
	launch := coord.activeSubagents.launches[reservation.Token]
	coord.mu.Unlock()
	if launch.claimedBy != "" {
		t.Fatalf("receipt remained claimed by %s", launch.claimedBy)
	}
	coord.ReleaseActiveSubagentReservation(reservation.Token)
	if _, err := coord.ReserveActiveSubagent(context.Background(), &parent); err != nil {
		t.Fatalf("coordinator wedged after panic: %v", err)
	}
}

func TestActiveSubagentRegistrationFailureReleasesClaimedReceipt(t *testing.T) {
	parent := centralstore.SessionID("root0000")
	id := centralstore.SessionID("child000")
	meta := liveMeta(id, "pi", "")
	meta.Registration.ParentSessionID = &parent
	client := newFakeClient(meta)
	client.metaErr = errors.New("startup meta failed")
	coord := New(nil, client, newFakeDurable(0), nil, nil,
		WithActiveSubagentBudget(1, func(a string) bool { return a == "pi" }, []centralstore.Session{{ID: parent, Adapter: "shell"}}))
	reservation, err := coord.ReserveActiveSubagent(context.Background(), &parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep", AssertedID: id, ActiveSubagentReservation: reservation.Token}); err == nil {
		t.Fatal("registration unexpectedly succeeded")
	}
	// The same receipt may retry after a transient pre-commit failure.
	client.metaErr = nil
	client.stream = newFakeStream()
	client.stream.incarnation = client.meta.Incarnation
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "ep", AssertedID: id, ActiveSubagentReservation: reservation.Token}); err != nil {
		t.Fatalf("receipt was not retryable: %v", err)
	}
}

func TestActiveSubagentRestartReconstructsOwnershipFromDurableRows(t *testing.T) {
	parent := centralstore.SessionID("root0000")
	child := centralstore.SessionID("child000")
	rows := []centralstore.Session{
		{ID: parent, Adapter: "shell"},
		{ID: child, Adapter: "pi", ParentSessionID: &parent},
	}
	meta := liveMeta(child, "pi", "")
	meta.Registration.ParentSessionID = &parent
	client := newFakeClient(meta)
	durable := newFakeDurable(0)
	durable.registerResult = func(centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error) {
		return centralstore.Session{ID: child, Version: 1, Adapter: "pi", ParentSessionID: &parent}, centralstore.MutationResult{Changed: true, SessionVersion: 1}, nil
	}
	coord := New(nil, client, durable, nil, nil, WithActiveSubagentBudget(1, func(a string) bool { return a == "pi" }, rows))
	if _, err := coord.Register(context.Background(), RegisterRequest{Endpoint: "surviving-runner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ReserveActiveSubagent(context.Background(), &parent); !errors.Is(err, ErrSubagentLimitReached) {
		t.Fatalf("post-restart admission error=%v, want limit", err)
	}
}

func BenchmarkActiveSubagentAdmission1000Sessions(b *testing.B) {
	rows := make([]centralstore.Session, 0, 1000)
	rows = append(rows, budgetSession("root", "shell", nil, false))
	parent := centralstore.SessionID("root")
	for i := 1; i < 1000; i++ {
		id := fmt.Sprintf("s-%04d", i)
		rows = append(rows, budgetSession(id, "shell", &parent, false))
		parent = centralstore.SessionID(id)
	}
	budget := newActiveSubagentBudget(8, func(string) bool { return false }, rows)
	launchParent := parent
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reservation, err := budget.reserve(&launchParent)
		if err != nil {
			b.Fatal(err)
		}
		budget.release(reservation.Token, false)
	}
}
