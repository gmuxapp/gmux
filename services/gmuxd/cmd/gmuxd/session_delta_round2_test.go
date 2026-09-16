package main

import (
	"fmt"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// Round-2 review findings, pinned.

func bigFamily(n int) []wire.Session {
	rows := []wire.Session{{ID: "root", SemanticAgent: true, Alive: true}}
	for i := 0; i < n; i++ {
		rows = append(rows, wire.Session{
			ID: fmt.Sprintf("k%04d", i), ParentSessionID: "root", Alive: true, SemanticAgent: true,
			CreatedAt: fmt.Sprintf("2026-01-01T%02d:%02d:%02dZ", i/3600, (i/60)%60, i%60),
			Title:     "a subagent with a title of realistic length for the wire",
			Cwd:       "/home/user/dev/project/subdir", Command: []string{"pi", "--session", "x"},
		})
	}
	return rows
}

func clone(in []wire.Session) []wire.Session {
	out := make([]wire.Session, len(in))
	copy(out, in)
	return out
}

// H2/D2: a scope is the client's viewport. A burst over a 690-child family
// with page 1 held produces a payload bounded by the page, which fits next to
// the world delta; the same burst over an uncapped subtree would not.
func TestScopeIsBoundedByTheViewport(t *testing.T) {
	rows := bigFamily(690)
	f := newSSEFanout()
	f.SetOwnershipFilter(func(string) bool { return false })
	f.DemandClass(deltaClassRoots)
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: rows}})
	memo0, e0 := f.CurrentMemo()
	page, _ := memo0.ChildrenPage("root", false, "", 50)
	if page.NextCursor == "" || len(page.Rows) != 50 {
		t.Fatalf("page 1: %d rows cursor=%q", len(page.Rows), page.NextCursor)
	}
	conn, release := f.RegisterConn()
	defer release()
	reg, windows, err := f.UpdateScopes(conn, []ScopeAdd{{Scope: "children:root", Cursor: page.NextCursor}}, nil)
	if err != nil || reg != e0 {
		t.Fatalf("register: epoch %d err %v", reg, err)
	}
	if w := windows["children:root"]; w.Rows != 50 || w.Total != 690 {
		t.Fatalf("window = %+v", w)
	}
	// Flip every child's status at once.
	next := clone(rows)
	for i := 1; i < len(next); i++ {
		next[i].Status = &wire.Status{Active: true}
	}
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: next}})
	memo1, e1 := f.CurrentMemo()
	ringKey := scopeRingKey("children:root", page.NextCursor)
	touched, ok := f.ScopeTouchedSince(e0, e1, ringKey)
	if !ok || len(touched) != 50 {
		t.Fatalf("viewport touched = %d ok=%v (want 50)", len(touched), ok)
	}
	up, rm := memo1.ScopeDeltaRows(ringKey, touched)
	wup, wrm := memo1.DeltaRows(deltaClassRoots, []string{"root"})
	scopes := map[string]sessionstream.ScopePayload[wire.Session]{"children:root": {FromEpoch: e0, Total: 690, Upsert: up, Remove: rm}}
	ev, fits, resets, err := sessionstream.EncodeDeltaWithScopes(f.BootID(), e0, e1, wup, wrm, scopes)
	if err != nil || !fits || len(resets) != 0 {
		t.Fatalf("folded viewport delta: fits=%v resets=%v err=%v (%d B)", fits, resets, err, len(ev.Data))
	}
	t.Logf("burst over 690 children, page-1 viewport: folded delta %d B, %d rows", len(ev.Data), len(up))

	// The uncapped subtree (a client that paged everything, window cap 200)
	// is still bounded by scopeWindowMax; beyond that it resets, not loops.
	_, _, _ = f.UpdateScopes(conn, []ScopeAdd{{Scope: "children:root"}}, nil)
	if n := len(memo1.ScopeRows(scopeRingKey("children:root", ""))); n != scopeWindowMax {
		t.Fatalf("uncapped window = %d rows, want cap %d", n, scopeWindowMax)
	}
}

// M2: a row REPARENTED into a held window is delivered by the scope (it is in
// the viewport); paging alone would never return it.
func TestReparentIntoHeldWindowIsDeliveredByScope(t *testing.T) {
	rows := bigFamily(10)
	rows = append(rows, wire.Session{ID: "other", SemanticAgent: true, Alive: true},
		wire.Session{ID: "moved", ParentSessionID: "other", Alive: true, CreatedAt: rows[9].CreatedAt, Title: "moved"})
	f := newSSEFanout()
	f.SetOwnershipFilter(func(string) bool { return false })
	f.DemandClass(deltaClassRoots)
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: rows}})
	memo0, e0 := f.CurrentMemo()
	page, _ := memo0.ChildrenPage("root", false, "", 3) // holds k0009,k0008,k0007
	conn, release := f.RegisterConn()
	defer release()
	_, _, _ = f.UpdateScopes(conn, []ScopeAdd{{Scope: "children:root", Cursor: page.NextCursor}}, nil)
	next := clone(rows)
	next[len(next)-1].ParentSessionID = "root" // created_at == k0008's: lands inside the held page
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: next}})
	memo1, e1 := f.CurrentMemo()
	ringKey := scopeRingKey("children:root", page.NextCursor)
	touched, ok := f.ScopeTouchedSince(e0, e1, ringKey)
	if !ok {
		t.Fatal("chain")
	}
	up, _ := memo1.ScopeDeltaRows(ringKey, touched)
	found := false
	for _, s := range up {
		if s.ID == "moved" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reparented row not in the window delta: %v", touched)
	}
	// Paging from the held cursor does not return it (documented limit).
	rest, _ := memo1.ChildrenPage("root", false, page.NextCursor, 100)
	for _, s := range rest.Rows {
		if s.ID == "moved" {
			t.Fatal("paging returned a row that landed inside a held page; the contract says it cannot")
		}
	}
	// A tail insert (older than the bound) is NOT in the window but the
	// total reflects it.
	next2 := clone(next)
	next2 = append(next2, wire.Session{ID: "old", ParentSessionID: "root", Alive: true, CreatedAt: "2025-01-01T00:00:00Z"})
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: next2}})
	memo2, e2 := f.CurrentMemo()
	touched, _ = f.ScopeTouchedSince(e1, e2, ringKey)
	if len(touched) != 0 {
		t.Fatalf("tail insert touched the window: %v", touched)
	}
	if memo2.ScopeTotal(ringKey) != 12 {
		t.Fatalf("total = %d, want 12", memo2.ScopeTotal(ringKey))
	}
}

// M5: scope churn has its own budget and cannot shorten the world chain.
func TestScopeChurnDoesNotEvictTheWorldChain(t *testing.T) {
	rows := bigFamily(200)
	f := newSSEFanout()
	f.SetOwnershipFilter(func(string) bool { return false })
	f.DemandClass(deltaClassRoots)
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: rows}})
	_, first := f.CurrentMemo()
	conn, release := f.RegisterConn()
	defer release()
	_, _, _ = f.UpdateScopes(conn, []ScopeAdd{{Scope: "children:root"}}, nil) // 200-row window
	cur := rows
	var last uint64
	for i := 0; i < 400; i++ { // 400 × 200 touched ids = 80K > scope budget
		cur = clone(cur)
		for j := 1; j < len(cur); j++ {
			cur[j].Title = fmt.Sprintf("t%d", i)
		}
		f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: cur}})
		_, last = f.CurrentMemo()
	}
	if _, ok := f.TouchedSince(first, last, deltaClassRoots); !ok {
		t.Fatal("world chain evicted by scope churn")
	}
	if _, ok := f.ScopeTouchedSince(first, last, scopeRingKey("children:root", "")); ok {
		t.Fatal("scope chain should have been trimmed (its own budget)")
	}
	f.mu.Lock()
	base, scopeBase, ids, sids := f.ring.base, f.ring.scopeBase, f.ring.ids, f.ring.scopeIDs
	f.mu.Unlock()
	if base != 0 || scopeBase == 0 || sids > defaultMaxScopeIDs {
		t.Fatalf("base=%d scopeBase=%d ids=%d scopeIDs=%d", base, scopeBase, ids, sids)
	}
}

// M6: the owned audience's roots are computed against the rows it sees.
func TestOwnedRootsViewUsesTheAudienceFamilyIndex(t *testing.T) {
	rows := []wire.Session{
		{ID: "p@far", Peer: "far", SemanticAgent: true},
		{ID: "mine", ParentSessionID: "p@far"},
	}
	m := newSessionEncodeMemo(1, &wire.SessionsPayload{Sessions: rows})
	m.SetLocalPeer(func(string) bool { return false })
	owned := m.ViewRows(deltaClassRootsOwned)
	if len(owned) != 1 || owned[0].ID != "mine" {
		t.Fatalf("owned roots = %v, want [mine]", owned)
	}
	if roots := m.ViewRows(deltaClassRoots); len(roots) != 1 || roots[0].ID != "p@far" {
		t.Fatalf("browser roots = %v", roots)
	}
}

// D3/M3: a connection cannot register more than maxScopesPerConn; re-adding
// replaces the window instead of counting again.
func TestScopesPerConnectionAreCapped(t *testing.T) {
	f := newSSEFanout()
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: bigFamily(3)}})
	conn, release := f.RegisterConn()
	defer release()
	for i := 0; i < maxScopesPerConn; i++ {
		if _, _, err := f.UpdateScopes(conn, []ScopeAdd{{Scope: fmt.Sprintf("children:r%d", i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.UpdateScopes(conn, []ScopeAdd{{Scope: "children:one-more"}}, nil); err != errTooManyScopes {
		t.Fatalf("err = %v, want cap", err)
	}
	if _, _, err := f.UpdateScopes(conn, []ScopeAdd{{Scope: "children:r0", Cursor: "abc"}}, nil); err != nil {
		t.Fatalf("replacing a held scope's window must not count: %v", err)
	}
	if n := f.ScopesTracked(); n != maxScopesPerConn {
		t.Fatalf("tracked %d", n)
	}
	release()
	if n := f.ScopesTracked(); n != 0 {
		t.Fatalf("release leaked %d scopes", n)
	}
}

// L1: annotating twice is idempotent.
func TestAnnotateDescendantCountsIsIdempotent(t *testing.T) {
	rows := bigFamily(3)
	wire.AnnotateDescendantCounts(rows)
	wire.AnnotateDescendantCounts(rows)
	if rows[0].DescendantCounts == nil || rows[0].DescendantCounts.Total != 3 {
		t.Fatalf("counts = %+v", rows[0].DescendantCounts)
	}
}
