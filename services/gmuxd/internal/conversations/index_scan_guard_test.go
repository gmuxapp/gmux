package conversations

import (
	"errors"
	"strconv"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// errAdapter describes nothing: the guard must be retired on the failure path
// too, or a transiently unreadable ref would leak an entry per attempt.
type errAdapter struct{ *fakeConvAdapter }

func (errAdapter) DescribeConversation(string) (*adapter.ConversationInfo, error) {
	return nil, errors.New("boom")
}

// The scan guard is a *window*, not a ledger: it exists while a Scan of that
// ref is in flight and disappears once the last one settles. Before this, the
// per-ref generation map only ever grew — every removal event (manual rm, log
// rotation, a conversation deleted by its tool) left an entry behind for the
// daemon's lifetime.
func TestScanGuardIsPrunedWhenWorkSettles(t *testing.T) {
	const ref = "conv-1|Some Title|/tmp/x"
	idx := New()
	a := &fakeConvAdapter{name: "probe"}

	if key := idx.Scan(a, ref); key == "" {
		t.Fatal("scan failed")
	}
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("guard entry survived a settled scan: %d entr(ies)", n)
	}

	// Removals with no scan in flight record nothing: a scan starting after a
	// removal re-reads the world, so there is nothing to invalidate.
	for i := 0; i < 100; i++ {
		idx.RemoveByRef("probe", ref)
		idx.Remove("probe", "conv-1")
		if key := idx.Scan(a, ref); key == "" {
			t.Fatalf("rescan %d refused to index a re-created conversation", i)
		}
	}
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("guard grew across %d remove/scan cycles: %d entr(ies)", 100, n)
	}

	// Describe failures keep stale-good state and must not leak a guard entry.
	failing := errAdapter{&fakeConvAdapter{name: "probe"}}
	for i := 0; i < 10; i++ {
		if key := idx.Scan(failing, "missing|x|/tmp/x"); key != "" {
			t.Fatalf("failed describe indexed key %q", key)
		}
	}
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("guard grew across failed describes: %d entr(ies)", n)
	}
}

// The property the change exists for, isolated: removals *alone* — the actual
// unbounded-growth workload (log rotation, a tool pruning old conversations,
// `rm` in a watched directory), where no scan of the ref follows — must leave
// nothing behind. The settle-based test above cannot see this: every removal
// there is followed by a Scan whose commit deletes the entry anyway.
func TestRemovalsWithoutScansRecordNothing(t *testing.T) {
	idx := New()
	a := &fakeConvAdapter{name: "probe"}

	// Index one conversation, then delete it by ID and by ref, repeatedly, plus
	// removals of refs that were never indexed at all.
	if key := idx.Scan(a, "conv-1|Some Title|/tmp/x"); key == "" {
		t.Fatal("scan failed")
	}
	idx.Remove("probe", "conv-1")
	for i := 0; i < 500; i++ {
		ref := "rotated-" + strconv.Itoa(i) + "|Title|/tmp/x"
		idx.RemoveByRef("probe", ref)
		idx.Remove("probe", "rotated-"+strconv.Itoa(i))
	}
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("removal-only workload left %d guard entr(ies)", n)
	}
}

// A removal landing mid-scan must still invalidate that scan (the #517 zombie
// guarantee), and the entry must be gone once the scan settles.
func TestScanGuardPrunedAfterInvalidatingAnInFlightScan(t *testing.T) {
	const ref = "conv-1|Some Title|/tmp/x"
	idx := New()
	gated := &fakeConvAdapter{
		name:            "probe",
		describeStarted: make(chan string, 1),
		describeGate:    make(chan struct{}),
	}
	done := make(chan string)
	go func() { done <- idx.Scan(gated, ref) }()
	<-gated.describeStarted

	idx.mu.RLock()
	pending := len(idx.scanning)
	idx.mu.RUnlock()
	if pending != 1 {
		t.Fatalf("in-flight scan is not tracked: %d entr(ies)", pending)
	}

	idx.RemoveByRef("probe", ref)
	close(gated.describeGate)
	if key := <-done; key != "" {
		t.Fatalf("zombie: scan committed key %q after the ref was removed", key)
	}
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("guard entry survived the invalidated scan: %d entr(ies)", n)
	}

	// The generation restarts from zero for this ref, which is only sound
	// because the entry is created fresh per scan window: a later scan snapshots
	// and re-checks the *same* entry it created.
	if key := idx.Scan(&fakeConvAdapter{name: "probe"}, ref); key == "" {
		t.Fatal("post-removal rescan refused to index a re-created conversation")
	}
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("guard entry survived the rescan: %d entr(ies)", n)
	}
}

// Two concurrent scans of the same ref share one entry; it is dropped only
// after the second one settles.
func TestScanGuardCountsConcurrentScans(t *testing.T) {
	const ref = "conv-1|Some Title|/tmp/x"
	idx := New()
	gates := []*fakeConvAdapter{
		{name: "probe", describeStarted: make(chan string, 1), describeGate: make(chan struct{})},
		{name: "probe", describeStarted: make(chan string, 1), describeGate: make(chan struct{})},
	}
	done := make(chan string, 2)
	for _, g := range gates {
		go func() { done <- idx.Scan(g, ref) }()
		<-g.describeStarted
	}

	idx.mu.RLock()
	state := idx.scanning[refKey("probe", ref)]
	idx.mu.RUnlock()
	if state.pending != 2 {
		t.Fatalf("pending scans = %d, want 2", state.pending)
	}

	close(gates[0].describeGate)
	<-done
	idx.mu.RLock()
	state = idx.scanning[refKey("probe", ref)]
	idx.mu.RUnlock()
	if state.pending != 1 {
		t.Fatalf("guard retired too early: pending = %d, want 1", state.pending)
	}

	close(gates[1].describeGate)
	<-done
	if n := len(idx.scanning); n != 0 {
		t.Fatalf("guard entry survived both scans: %d entr(ies)", n)
	}
}
