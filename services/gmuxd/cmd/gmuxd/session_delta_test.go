package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// Oracle: for every pair of epochs (i, j) in a randomized broadcast sequence,
// a subscriber that holds view i and applies the delta the server would send
// at epoch j must end up with a map deep-equal to view j — for every view
// class AND for a registered children scope. Mutations include reparenting
// and promotion, so rows move between the roots view and a scope.
func TestDeltaReplayEqualsSnapshots(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	isLocalPeer := func(name string) bool { return name == "box" }

	peers := []string{"", "", "", "box", "remote"}
	const rows = 40
	parent := make([]int, rows) // -1 = root
	for i := range parent {
		parent[i] = -1
	}
	makeRow := func(i, rev int) wire.Session {
		s := wire.Session{
			ID:            fmt.Sprintf("s%02d", i),
			Peer:          peers[i%len(peers)],
			Title:         fmt.Sprintf("title-%d", rev),
			Alive:         rev%2 == 0,
			SemanticAgent: i%3 != 2,
			Unread:        rev%3 == 0,
			CreatedAt:     fmt.Sprintf("2026-09-%02dT00:00:00Z", 1+i%28),
		}
		if parent[i] >= 0 {
			s.ParentSessionID = fmt.Sprintf("s%02d", parent[i])
		}
		return s
	}

	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(isLocalPeer)
	for c := 0; c < deltaClassCount; c++ {
		fanout.DemandClass(c)
	}
	conn, release := fanout.RegisterConn()
	defer release()
	const scope = scopeChildrenPrefix + "s00" // s00 is a same-peer agent root
	if _, ok := fanout.UpdateScopes(conn, []string{scope}, nil); !ok {
		t.Fatal("scope registration failed")
	}

	revs := make([]int, rows)
	live := map[int]bool{}
	for i := 0; i < rows; i++ {
		live[i] = true
	}
	type step struct {
		epoch uint64
		memo  *sessionEncodeMemo
		snap  map[int]map[string]string // class -> id -> encoded row
		scope map[string]string
	}
	var steps []step

	encodeAll := func(list []wire.Session) map[string]string {
		m := map[string]string{}
		for _, s := range list {
			b, _ := json.Marshal(s)
			m[s.ID] = string(b)
		}
		return m
	}

	for n := 0; n < 60; n++ {
		switch rng.Intn(6) {
		case 0:
			revs[rng.Intn(rows)]++
		case 1:
			delete(live, rng.Intn(rows))
		case 2:
			live[rng.Intn(rows)] = true
		case 3:
			revs[rng.Intn(rows)] += 2
		case 4: // reparent under s00 (same peer "" → i%5==0) or another row
			i := rng.Intn(rows)
			p := rng.Intn(rows)
			if p != i {
				parent[i] = p
			}
		case 5: // promote
			parent[rng.Intn(rows)] = -1
		}
		var list []wire.Session
		for i := 0; i < rows; i++ {
			if live[i] {
				list = append(list, makeRow(i, revs[i]))
			}
		}
		payload := &wire.SessionsPayload{Sessions: list}
		fanout.BroadcastFrames(wire.Frames{Sessions: payload})
		memo, epoch := fanout.CurrentMemo()
		snap := map[int]map[string]string{}
		for c := 0; c < deltaClassCount; c++ {
			snap[c] = encodeAll(memo.ViewRows(c))
		}
		steps = append(steps, step{epoch: epoch, memo: memo, snap: snap, scope: encodeAll(memo.ScopeRows(scope))})
	}

	check := func(label string, base map[string]string, upsert []wire.Session, remove []string, want map[string]string) {
		got := map[string]string{}
		for id, row := range base {
			got[id] = row
		}
		for _, id := range remove {
			delete(got, id)
		}
		for _, row := range upsert {
			b, _ := json.Marshal(row)
			got[row.ID] = string(b)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", label, len(got), len(want))
		}
		for id, row := range want {
			if got[id] != row {
				t.Fatalf("%s: row %s diverged\n got=%s\nwant=%s", label, id, got[id], row)
			}
		}
	}

	for i := range steps {
		for j := i + 1; j < len(steps); j++ {
			for c := 0; c < deltaClassCount; c++ {
				touched, ok := fanout.TouchedSince(steps[i].epoch, steps[j].epoch, c)
				if !ok {
					t.Fatalf("chain %d->%d class=%d not covered by the ring", i, j, c)
				}
				upsert, remove := steps[j].memo.DeltaRows(c, touched)
				check(fmt.Sprintf("chain %d->%d class=%d", i, j, c), steps[i].snap[c], upsert, remove, steps[j].snap[c])
			}
			touched, ok := fanout.ScopeTouchedSince(steps[i].epoch, steps[j].epoch, scope)
			if !ok {
				t.Fatalf("scope chain %d->%d not covered", i, j)
			}
			upsert, remove := steps[j].memo.ScopeDeltaRows(scope, touched)
			check(fmt.Sprintf("scope chain %d->%d", i, j), steps[i].scope, upsert, remove, steps[j].scope)
		}
	}
	// Sanity: the roots view is a strict subset in at least one step.
	smaller := false
	for _, s := range steps {
		if len(s.snap[deltaClassRoots]) < len(s.snap[deltaClassAll]) {
			smaller = true
		}
	}
	if !smaller {
		t.Fatal("roots view never hid a child; the fixture is not exercising families")
	}
}

// A root's counts are part of its roots-view value: a mutation deep in the
// subtree touches the root (and only the root) in the roots class, while
// the children scope of the intermediate parent sees the child itself.
func TestRootsDeltaCarriesCountsForSubtreeChange(t *testing.T) {
	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(func(string) bool { return false })
	fanout.DemandClass(deltaClassRoots)
	rows := func(childActive bool) *wire.SessionsPayload {
		return &wire.SessionsPayload{Sessions: []wire.Session{
			{ID: "a", SemanticAgent: true, Alive: true},
			{ID: "b", SemanticAgent: true, Alive: true, ParentSessionID: "a"},
			{ID: "c", SemanticAgent: false, Alive: true, ParentSessionID: "b", Status: &wire.Status{Active: childActive}},
			{ID: "shell", SemanticAgent: false, Alive: true},
			{ID: "shell-editor", SemanticAgent: false, Alive: true, ParentSessionID: "shell"},
		}}
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: rows(false)})
	_, e1 := fanout.CurrentMemo()
	fanout.BroadcastFrames(wire.Frames{Sessions: rows(true)})
	memo, e2 := fanout.CurrentMemo()

	roots := memo.ViewRows(deltaClassRoots)
	ids := []string{}
	for _, r := range roots {
		ids = append(ids, r.ID)
	}
	// shell-editor's parent is not an agent: it stays a root of its own.
	if len(roots) != 3 || ids[0] != "a" || ids[1] != "shell" || ids[2] != "shell-editor" {
		t.Fatalf("roots = %v", ids)
	}
	if c := roots[0].DescendantCounts; c == nil || c.Total != 2 || c.Alive != 2 || c.Running != 1 || c.Children != 1 {
		t.Fatalf("root counts = %+v", c)
	}
	touched, ok := fanout.TouchedSince(e1, e2, deltaClassRoots)
	if !ok || len(touched) != 1 || touched[0] != "a" {
		t.Fatalf("roots touched = %v ok=%v, want [a]", touched, ok)
	}
	up, rm := memo.DeltaRows(deltaClassRoots, touched)
	if len(up) != 1 || len(rm) != 0 || up[0].DescendantCounts.Running != 1 {
		t.Fatalf("roots delta = %+v / %v", up, rm)
	}
}

// Promotion while a drawer is open: the child leaves the scope (removal in
// the scoped delta) and arrives in the roots view (upsert), in one epoch.
func TestPromotionMovesRowBetweenScopeAndRoots(t *testing.T) {
	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(func(string) bool { return false })
	fanout.DemandClass(deltaClassRoots)
	conn, release := fanout.RegisterConn()
	defer release()
	rows := func(promoted bool) *wire.SessionsPayload {
		child := wire.Session{ID: "kid", SemanticAgent: true, Alive: true, ParentSessionID: "root"}
		if promoted {
			child.ParentSessionID = ""
		}
		return &wire.SessionsPayload{Sessions: []wire.Session{{ID: "root", SemanticAgent: true, Alive: true}, child}}
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: rows(false)})
	e1, ok := fanout.UpdateScopes(conn, []string{"children:root"}, nil)
	if !ok {
		t.Fatal("scope")
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: rows(true)})
	memo, e2 := fanout.CurrentMemo()

	touched, ok := fanout.ScopeTouchedSince(e1, e2, "children:root")
	if !ok {
		t.Fatal("scope chain not covered")
	}
	up, rm := memo.ScopeDeltaRows("children:root", touched)
	if len(up) != 0 || len(rm) != 1 || rm[0] != "kid" {
		t.Fatalf("scope delta up=%v rm=%v", up, rm)
	}
	rt, ok := fanout.TouchedSince(e1, e2, deltaClassRoots)
	if !ok {
		t.Fatal("roots chain")
	}
	rup, rrm := memo.DeltaRows(deltaClassRoots, rt)
	seen := map[string]bool{}
	for _, s := range rup {
		seen[s.ID] = true
	}
	if !seen["kid"] || !seen["root"] || len(rrm) != 0 {
		t.Fatalf("roots delta up=%v rm=%v; want kid promoted + root's counts", rup, rrm)
	}
	if rup[0].ID == "root" && rup[0].DescendantCounts != nil {
		t.Fatalf("root still carries counts after promotion: %+v", rup[0].DescendantCounts)
	}
}

// A row that leaves the ?as=peer audience because its owner stopped being a
// Local peer must reach that audience as a removal.
func TestDeltaOwnershipFlipLooksLikeRemoval(t *testing.T) {
	localPeers := map[string]bool{"box": true}
	fanout := newSSEFanout()
	fanout.SetOwnershipFilter(func(name string) bool { return localPeers[name] })
	fanout.DemandClass(deltaClassOwned)
	fanout.DemandClass(deltaClassAll)

	rowsOf := func() *wire.SessionsPayload {
		return &wire.SessionsPayload{Sessions: []wire.Session{
			{ID: "local-1"},
			{ID: "box/remote-1", Peer: "box"},
		}}
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: rowsOf()})
	_, first := fanout.CurrentMemo()

	localPeers["box"] = false // peer reclassified; rows unchanged byte-wise
	fanout.BroadcastFrames(wire.Frames{Sessions: rowsOf()})
	memo, second := fanout.CurrentMemo()

	touched, ok := fanout.TouchedSince(first, second, deltaClassOwned)
	if !ok {
		t.Fatal("ring did not cover the flip")
	}
	upsert, remove := memo.DeltaRows(deltaClassOwned, touched)
	if len(upsert) != 0 || len(remove) != 1 || remove[0] != "box/remote-1" {
		t.Fatalf("ownership flip: upsert=%v remove=%v, want a single removal of box/remote-1", upsert, remove)
	}
	touchedAll, _ := fanout.TouchedSince(first, second, deltaClassAll)
	if len(touchedAll) != 0 {
		t.Fatalf("browser audience saw %v changed, want nothing", touchedAll)
	}
}

// Eviction is the honest failure mode: a subscriber older than the ring base
// is told "no chain", and the caller falls back to today's full bootstrap.
func TestDeltaRingEvictionRefusesStaleResume(t *testing.T) {
	fanout := newSSEFanout()
	fanout.ring.maxEntries = 4
	fanout.DemandClass(deltaClassAll)
	for i := 0; i < 10; i++ {
		fanout.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "a", Title: fmt.Sprint(i)}}}})
	}
	_, latest := fanout.CurrentMemo()
	if _, ok := fanout.TouchedSince(1, latest, deltaClassAll); ok {
		t.Fatal("resume from an evicted epoch must be refused")
	}
	if _, ok := fanout.TouchedSince(latest-1, latest, deltaClassAll); !ok {
		t.Fatal("resume from the newest epoch must be honored")
	}
	// An undemanded class has no chain at all.
	if _, ok := fanout.TouchedSince(latest-1, latest, deltaClassRootsOwned); ok {
		t.Fatal("an undemanded class must not offer a chain")
	}
}

// A scope registered after some epochs cannot chain across its registration.
func TestScopeChainStartsAtRegistration(t *testing.T) {
	fanout := newSSEFanout()
	conn, release := fanout.RegisterConn()
	defer release()
	payload := func(i int) *wire.SessionsPayload {
		return &wire.SessionsPayload{Sessions: []wire.Session{{ID: "r", SemanticAgent: true}, {ID: "k", ParentSessionID: "r", Title: fmt.Sprint(i)}}}
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: payload(0)})
	_, before := fanout.CurrentMemo()
	fanout.BroadcastFrames(wire.Frames{Sessions: payload(1)})
	// A client connecting between the broadcast and the registration bumps
	// the fanout epoch without changing state. The chain must start at the
	// epoch a GET /children page cut from the current memo carries, or every
	// drawer open would refetch page 1 to "catch up" to nothing.
	_, _, cancel := fanout.Subscribe()
	defer cancel()
	pageMemo, _ := fanout.CurrentMemo()
	reg, _ := fanout.UpdateScopes(conn, []string{"children:r"}, nil)
	if reg != pageMemo.epoch {
		t.Fatalf("scope chains from %d, but a page cut now carries epoch %d", reg, pageMemo.epoch)
	}
	fanout.BroadcastFrames(wire.Frames{Sessions: payload(2)})
	_, after := fanout.CurrentMemo()
	if _, ok := fanout.ScopeTouchedSince(before, after, "children:r"); ok {
		t.Fatal("a scope must not chain across its own registration")
	}
	touched, ok := fanout.ScopeTouchedSince(reg, after, "children:r")
	if !ok || len(touched) != 1 || touched[0] != "k" {
		t.Fatalf("touched=%v ok=%v", touched, ok)
	}
	// Releasing the connection drops the scope entirely.
	release()
	if _, ok := fanout.ScopeTouchedSince(reg, after, "children:r"); ok {
		t.Fatal("released scope still tracked")
	}
}

// Oversized deltas fall back rather than splitting: the delta event is the
// unit of atomicity, so it must fit one SSE payload.
func TestDeltaOversizedFallsBack(t *testing.T) {
	big := make([]wire.Session, 200)
	for i := range big {
		big[i] = wire.Session{ID: fmt.Sprintf("s%03d", i), Title: string(make([]byte, 400))}
	}
	_, fits, err := sessionstream.EncodeDelta("boot", 1, 2, big, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fits {
		t.Fatal("a delta above MaxEventPayload must report !fits so the sender falls back")
	}
	_, fits, err = sessionstream.EncodeDelta("boot", 1, 2, big[:2], []string{"x"})
	if err != nil || !fits {
		t.Fatalf("small delta: fits=%v err=%v", fits, err)
	}
	if _, fits, _ := sessionstream.EncodeScopeDelta("boot", "children:x", 1, 2, big, nil); fits {
		t.Fatal("oversized scope delta must report !fits so the sender resets")
	}
}
