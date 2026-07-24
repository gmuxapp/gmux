package centralstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func addSessionCwd(t *testing.T, s *Store, id, parent, cwd string, created UnixMillis) Session {
	t.Helper()
	v := NewSession{ID: SessionID(id), Adapter: "shell", Command: []string{"sh"}, CWD: cwd, Remotes: map[string]string{}, CreatedAt: created}
	if parent != "" {
		p := SessionID(parent)
		v.LaunchParentID = &p
	}
	out, _, err := s.InsertSession(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func placeLocal(t *testing.T, s *Store, id string, project ProjectEntryID) {
	t.Helper()
	if _, err := s.PlaceLocalSession(context.Background(), SessionID(id), project); err != nil {
		t.Fatal(err)
	}
}

func specsFromCatalog(cat ProjectCatalog) []ProjectEntrySpec {
	out := make([]ProjectEntrySpec, len(cat))
	for i, e := range cat {
		if e.Kind == ProjectEntryOwned {
			rules := append([]MatchRule(nil), e.Rules...)
			out[i] = ProjectEntrySpec{ID: e.ID, Owned: &OwnedProjectSpec{Slug: e.Slug, Rules: rules}}
		} else {
			out[i] = ProjectEntrySpec{ID: e.ID, Reference: &ProjectReference{PeerKey: e.PeerKey, Slug: e.Slug}}
		}
	}
	return out
}

func localPlacementOf(t *testing.T, s *Store, id string) *placementRec {
	t.Helper()
	all, err := placements(context.Background(), s.queries)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.local == id {
			return r
		}
	}
	return nil
}

// TestCatalogRematchMovesRetargetedSessionsAndPreservesSurvivorOrder pins the
// core preserve/move/append contract: a rules change that retargets one
// placed session appends it at the bottom of the new project's root scope,
// while sessions whose derived project is unchanged keep their exact durable
// order, and the vacated scope densifies.
func TestCatalogRematchMovesRetargetedSessionsAndPreservesSurvivorOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := cat[0].ID, cat[1].ID
	addSessionCwd(t, s, "s1", "", "/a/x", 1)
	addSessionCwd(t, s, "s2", "", "/a/y", 2)
	addSessionCwd(t, s, "s3", "", "/a/z", 3)
	addSessionCwd(t, s, "old-b", "", "/b/q", 4)
	placeLocal(t, s, "s1", aID)
	placeLocal(t, s, "s2", aID)
	placeLocal(t, s, "s3", aID)
	placeLocal(t, s, "old-b", bID)

	// New rules: /a/y now belongs to project b; a and b otherwise unchanged.
	specs := []ProjectEntrySpec{
		{ID: aID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/a"}}}},
		{ID: bID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}, {Path: "/a/y"}}}},
	}
	out, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10)
	if err != nil || !r.Changed || !r.SessionsDirty || !r.WorldDirty {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if len(out) != 2 || out[1].Rules[1].Path != "/a/y" {
		t.Fatalf("catalog=%#v", out)
	}
	if got := rootOrder(t, s, aID); !reflect.DeepEqual(got, []string{"l:s1", "l:s3"}) {
		t.Fatalf("project a order=%v", got) // survivor order preserved, dense
	}
	if got := rootOrder(t, s, bID); !reflect.DeepEqual(got, []string{"l:old-b", "l:s2"}) {
		t.Fatalf("project b order=%v", got) // mover appended at the bottom
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchPreservesDurableOrderNotCreationOrder: survivors keep
// their user-authored (reordered) positions, which deliberately differ from
// creation order here so a regression to created-at sorting fails loudly
// (tests-review HIGH-1: the plain survivor test cannot tell the two apart).
func TestCatalogRematchPreservesDurableOrderNotCreationOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := cat[0].ID, cat[1].ID
	addSessionCwd(t, s, "s1", "", "/a/x", 1)
	addSessionCwd(t, s, "s2", "", "/a/y", 2)
	addSessionCwd(t, s, "s3", "", "/a/z", 3)
	placeLocal(t, s, "s1", aID)
	placeLocal(t, s, "s2", aID)
	placeLocal(t, s, "s3", aID)
	// User-authored durable order: s3 before s1 before s2 — the reverse of
	// creation order for the surviving pair.
	if _, err := s.ReorderSiblings(ctx, aID, ParentRef{}, []SubjectRef{{LocalSessionID: "s3"}, {LocalSessionID: "s1"}, {LocalSessionID: "s2"}}); err != nil {
		t.Fatal(err)
	}

	specs := []ProjectEntrySpec{
		{ID: aID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/a"}}}},
		{ID: bID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}, {Path: "/a/y"}}}},
	}
	if _, _, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, aID); !reflect.DeepEqual(got, []string{"l:s3", "l:s1"}) {
		t.Fatalf("durable order lost, project a order=%v (creation order would be [l:s1 l:s3])", got)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchMultiMoverAppendOrder pins the documented mover sort
// when several subjects land in the same scope in one replacement: movers
// append after stayers; local movers order by created-at with session-ID
// tiebreak; mixed local/Local-peer movers order by subject key (locals
// before peers) — tests-review HIGH-2.
func TestCatalogRematchMultiMoverAppendOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := cat[0].ID, cat[1].ID
	// Created order deliberately not the placement order: m2 and m1 tie on
	// created-at (ID breaks the tie), m3 is oldest despite being placed last.
	addSessionCwd(t, s, "m2", "", "/a/x", 5)
	addSessionCwd(t, s, "m1", "", "/a/y", 5)
	addSessionCwd(t, s, "m3", "", "/a/z", 2)
	addSessionCwd(t, s, "old-b", "", "/b/q", 1)
	placeLocal(t, s, "m2", aID)
	placeLocal(t, s, "m1", aID)
	placeLocal(t, s, "m3", aID)
	placeLocal(t, s, "old-b", bID)
	pm := LocalPeerSubject{PeerKey: "box", SessionID: "pm"}
	if _, err := s.UpsertLocalPeerPlacement(ctx, pm, aID); err != nil {
		t.Fatal(err)
	}

	// Everything in project a moves to b in one replacement.
	specs := []ProjectEntrySpec{
		{ID: aID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/elsewhere"}}}},
		{ID: bID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}, {Path: "/a"}}}},
	}
	inputs := []LocalPeerMatchInput{{Subject: pm, CWD: "/a/peer"}}
	_, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, inputs, 10)
	if err != nil || !r.Changed {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	// Stayer first, then local movers by (created, id): m3(2), m1(5), m2(5);
	// the Local-peer mover sorts after locals by subject key ("l:" < "p:").
	want := []string{"l:old-b", "l:m3", "l:m1", "l:m2", "p:box:pm"}
	if got := rootOrder(t, s, bID); !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-mover order=%v want %v", got, want)
	}
	if got := rootOrder(t, s, aID); len(got) != 0 {
		t.Fatalf("vacated project a=%v", got)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchRulePrecedenceThroughRematch: the rematch inherits the
// full matcher policy — any path match beats a remote match, and the longest
// normalized path wins across entries.
func TestCatalogRematchRulePrecedenceThroughRematch(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
		owned("start", "/start"),
		{Owned: &OwnedProjectSpec{Slug: "remote", Rules: []MatchRule{{Remote: "github.com/org/repo"}}}},
		owned("shallow", "/work"),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	startID := cat[0].ID
	v := NewSession{ID: "s", Adapter: "shell", Command: []string{"sh"}, CWD: "/work/repo/sub",
		Remotes: map[string]string{"origin": "git@github.com:org/repo.git"}, CreatedAt: 1}
	if _, _, err := s.InsertSession(ctx, v); err != nil {
		t.Fatal(err)
	}
	placeLocal(t, s, "s", startID)

	// New catalog: remote entry first (would win on entry order), a shallow
	// path, and a deeper path added last — deepest path must win.
	specs := append(specsFromCatalog(cat), owned("deep", "/work/repo"))
	specs[0].Owned.Rules = []MatchRule{{Path: "/gone"}} // evict from start
	out, _, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	deepID := out[3].ID
	rec := localPlacementOf(t, s, "s")
	if rec == nil || rec.project != int64(deepID) {
		t.Fatalf("placement=%#v want deep project %d (path beats remote, longest path wins)", rec, deepID)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchUnmatchedBecomesUnplacedNotDismissed: a placed session
// matching no project under the new rules loses its placement but stays a
// visible retained row.
func TestCatalogRematchUnmatchedBecomesUnplacedNotDismissed(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	addSessionCwd(t, s, "s1", "", "/a/x", 1)
	placeLocal(t, s, "s1", cat[0].ID)

	specs := []ProjectEntrySpec{{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/elsewhere"}}}}}
	_, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10)
	if err != nil || !r.Changed {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if localPlacementOf(t, s, "s1") != nil {
		t.Fatal("unmatched session still placed")
	}
	sess, ok, err := s.Session(ctx, "s1")
	if err != nil || !ok || sess.DismissedAt != nil {
		t.Fatalf("session=%#v ok=%v err=%v", sess, ok, err)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchDeletedEntryReplacesOrUnplaces: sessions whose project
// entry was deleted are re-placed into another matching owned entry
// (appended) or become unplaced when nothing matches.
func TestCatalogRematchDeletedEntryReplacesOrUnplaces(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("wide", "/")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, wideID := cat[0].ID, cat[1].ID
	addSessionCwd(t, s, "covered", "", "/a/x", 1)
	addSessionCwd(t, s, "orphan", "", "/a/y", 2)
	addSessionCwd(t, s, "wide-old", "", "/w", 3)
	placeLocal(t, s, "covered", aID)
	placeLocal(t, s, "orphan", aID)
	placeLocal(t, s, "wide-old", wideID)

	// Delete entry a; keep wide but narrow its rule so "orphan" matches nothing.
	specs := []ProjectEntrySpec{{ID: wideID, Owned: &OwnedProjectSpec{Slug: "wide", Rules: []MatchRule{{Path: "/w"}, {Path: "/a/x"}}}}}
	_, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10)
	if err != nil || !r.Changed {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, wideID); !reflect.DeepEqual(got, []string{"l:wide-old", "l:covered"}) {
		t.Fatalf("wide order=%v", got)
	}
	if localPlacementOf(t, s, "orphan") != nil {
		t.Fatal("orphan re-placed despite matching nothing")
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchDoesNotPlaceUnplacedSessions pins the flagged
// conservative decision: an unplaced visible session that newly matches is
// NOT placed by catalog replacement (liveness is unknown to SQLite; the
// coordinator owns auto-assignment).
func TestCatalogRematchDoesNotPlaceUnplacedSessions(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	addSessionCwd(t, s, "floating", "", "/new/x", 1)
	specs := append(specsFromCatalog(cat), owned("new", "/new"))
	out, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10)
	if err != nil || !r.Changed || len(out) != 2 {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if localPlacementOf(t, s, "floating") != nil {
		t.Fatal("catalog replacement placed an unplaced session")
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchScopeAndPromotionRules: an unpromoted child moving with
// its parent regroups under the parent's child scope in the new project; a
// promoted child stays a root in its new project.
func TestCatalogRematchScopeAndPromotionRules(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := cat[0].ID, cat[1].ID
	addSessionCwd(t, s, "parent", "", "/a/repo", 1)
	addSessionCwd(t, s, "child", "parent", "/a/repo", 2)
	addSessionCwd(t, s, "promoted", "parent", "/a/repo", 3)
	placeLocal(t, s, "parent", aID)
	placeLocal(t, s, "child", aID)
	placeLocal(t, s, "promoted", aID)
	if _, err := s.SetPromotion(ctx, "promoted", true, nil); err != nil {
		t.Fatal(err)
	}

	// Move /a/repo wholesale into project b.
	specs := []ProjectEntrySpec{
		{ID: aID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/a", Exact: true}}}},
		{ID: bID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}, {Path: "/a/repo"}}}},
	}
	_, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10)
	if err != nil || !r.Changed {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, bID); !reflect.DeepEqual(got, []string{"l:parent", "l:promoted"}) {
		t.Fatalf("b roots=%v", got)
	}
	if got := scopeOrder(t, s, bID, "c:l:parent"); !reflect.DeepEqual(got, []string{"l:child"}) {
		t.Fatalf("b child scope=%v", got)
	}
	if got := rootOrder(t, s, aID); len(got) != 0 {
		t.Fatalf("a roots=%v", got)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchLocalPeerInputs: supplied Local-peer inputs are
// re-derived (move/remove); placed subjects without inputs keep their
// placement when the entry survives; validation rejects duplicates and
// malformed subjects.
func TestCatalogRematchLocalPeerInputs(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := cat[0].ID, cat[1].ID
	mover := LocalPeerSubject{PeerKey: "box", SessionID: "m"}
	gone := LocalPeerSubject{PeerKey: "box", SessionID: "g"}
	keeper := LocalPeerSubject{PeerKey: "box", SessionID: "k"}
	for _, sub := range []LocalPeerSubject{mover, gone, keeper} {
		if _, err := s.UpsertLocalPeerPlacement(ctx, sub, aID); err != nil {
			t.Fatal(err)
		}
	}
	specs := specsFromCatalog(cat)
	inputs := []LocalPeerMatchInput{
		{Subject: mover, CWD: "/b/x"},
		{Subject: gone, CWD: "/nowhere"},
		// keeper has no input: placement kept because entry a survives.
	}
	_, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, inputs, 10)
	if err != nil || !r.Changed {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, bID); !reflect.DeepEqual(got, []string{"p:box:m"}) {
		t.Fatalf("b roots=%v", got)
	}
	if got := rootOrder(t, s, aID); !reflect.DeepEqual(got, []string{"p:box:k"}) {
		t.Fatalf("a roots=%v", got)
	}

	// An input for a subject that was never placed is ignored.
	unplaced := []LocalPeerMatchInput{{Subject: LocalPeerSubject{PeerKey: "box", SessionID: "new"}, CWD: "/a/x"}}
	_, r, err = s.ReplaceProjectCatalogAndRematch(ctx, specs, unplaced, 11)
	if err != nil || r.Changed {
		t.Fatalf("unplaced input result=%#v err=%v", r, err)
	}

	if _, _, err = s.ReplaceProjectCatalogAndRematch(ctx, specs, []LocalPeerMatchInput{{Subject: mover}, {Subject: mover}}, 12); err == nil {
		t.Fatal("duplicate Local-peer input accepted")
	}
	if _, _, err = s.ReplaceProjectCatalogAndRematch(ctx, specs, []LocalPeerMatchInput{{Subject: LocalPeerSubject{PeerKey: "", SessionID: "x"}}}, 13); err == nil {
		t.Fatal("invalid Local-peer subject accepted")
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchLocalPeerParentChangeRegroups: a survivor whose supplied
// input carries a new parent regroups under that parent's child scope when
// the parent is placed in the same project (valid parent-change success
// path for the catalog op).
func TestCatalogRematchLocalPeerParentChangeRegroups(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID := cat[0].ID
	parent := LocalPeerSubject{PeerKey: "box", SessionID: "parent"}
	child := LocalPeerSubject{PeerKey: "box", SessionID: "child"}
	for _, sub := range []LocalPeerSubject{parent, child} {
		if _, err := s.UpsertLocalPeerPlacement(ctx, sub, aID); err != nil {
			t.Fatal(err)
		}
	}
	inputs := []LocalPeerMatchInput{
		{Subject: parent, CWD: "/a/x"},
		{Subject: LocalPeerSubject{PeerKey: "box", SessionID: "child", ParentSessionID: "parent"}, CWD: "/a/x"},
	}
	_, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specsFromCatalog(cat), inputs, 10)
	if err != nil || !r.Changed {
		t.Fatalf("result=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, aID); !reflect.DeepEqual(got, []string{"p:box:parent"}) {
		t.Fatalf("roots=%v", got)
	}
	if got := scopeOrder(t, s, aID, "c:p:box:parent"); !reflect.DeepEqual(got, []string{"p:box:child"}) {
		t.Fatalf("child scope=%v", got)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchRejectsCycleFromReplacedLocalPeerSubjects is the fable
// MEDIUM-1 regression: when every parent change arrives via cascade-deleted
// re-placed subjects (entry deleted, both subjects re-placed into a survivor
// with mutually-cyclic parents), the cycle guard must still run — otherwise
// the committed cycle permanently bricks every later Local-peer placement
// op behind validateLocalPeerParentGraph.
func TestCatalogRematchRejectsCycleFromReplacedLocalPeerSubjects(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	aID, bID := cat[0].ID, cat[1].ID
	m := LocalPeerSubject{PeerKey: "box", SessionID: "m"}
	n := LocalPeerSubject{PeerKey: "box", SessionID: "n"}
	for _, sub := range []LocalPeerSubject{m, n} {
		if _, err := s.UpsertLocalPeerPlacement(ctx, sub, aID); err != nil {
			t.Fatal(err)
		}
	}
	beforeCat, err := s.ListProjectCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Delete entry a (cascade-deletes both placements); both inputs match
	// surviving entry b and carry mutually-cyclic parents, so every parent
	// change flows through the re-place branch.
	specs := []ProjectEntrySpec{{ID: bID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}}}}}
	inputs := []LocalPeerMatchInput{
		{Subject: LocalPeerSubject{PeerKey: "box", SessionID: "m", ParentSessionID: "n"}, CWD: "/b/x"},
		{Subject: LocalPeerSubject{PeerKey: "box", SessionID: "n", ParentSessionID: "m"}, CWD: "/b/y"},
	}
	if _, _, err := s.ReplaceProjectCatalogAndRematch(ctx, specs, inputs, 10); !errors.Is(err, ErrLocalPeerParentCycle) {
		t.Fatalf("cyclic re-placed parents err=%v, want ErrLocalPeerParentCycle", err)
	}

	// Whole transaction rolled back: catalog unchanged, both placements
	// still in entry a, and later Local-peer placement ops still work.
	afterCat, err := s.ListProjectCatalog(ctx)
	if err != nil || !reflect.DeepEqual(afterCat, beforeCat) {
		t.Fatalf("catalog mutated by rejected rematch: %#v err=%v", afterCat, err)
	}
	if got := rootOrder(t, s, aID); !reflect.DeepEqual(got, []string{"p:box:m", "p:box:n"}) {
		t.Fatalf("placements after rollback=%v", got)
	}
	if _, err := s.UpsertLocalPeerPlacement(ctx, LocalPeerSubject{PeerKey: "box", SessionID: "later"}, bID); err != nil {
		t.Fatalf("later Local-peer placement bricked: %v", err)
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchNoOpAndBootstrapGuardUnchanged: an identical catalog
// with no placement effect commits as an unchanged no-op, and the bootstrap
// primitive still refuses to run once placements exist.
func TestCatalogRematchNoOpAndBootstrapGuardUnchanged(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	addSessionCwd(t, s, "s1", "", "/a/x", 1)
	placeLocal(t, s, "s1", cat[0].ID)

	if _, _, err := s.ReplaceProjectCatalog(ctx, specsFromCatalog(cat), 5); !errors.Is(err, ErrCatalogHasPlacements) {
		t.Fatalf("bootstrap guard err=%v", err)
	}
	out, r, err := s.ReplaceProjectCatalogAndRematch(ctx, specsFromCatalog(cat), nil, 5)
	if err != nil || r.Changed || r.SessionsDirty || r.WorldDirty {
		t.Fatalf("noop result=%#v err=%v", r, err)
	}
	if !reflect.DeepEqual(out, cat) {
		t.Fatalf("noop catalog=%#v", out)
	}
	if _, _, err = s.ReplaceProjectCatalogAndRematch(ctx, specsFromCatalog(cat), nil, -1); err == nil {
		t.Fatal("negative timestamp accepted")
	}
	assertKernelInvariants(t, s)
}

// TestCatalogRematchRollsBackAtomically: an injected fault during placement
// finalization rolls back the catalog swap and every placement mutation.
func TestCatalogRematchRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	addSessionCwd(t, s, "s1", "", "/a/x", 1)
	placeLocal(t, s, "s1", cat[0].ID)
	beforeCat, err := s.ListProjectCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}

	s.beforePlacementFinalize = func() error { return errors.New("boom") }
	specs := []ProjectEntrySpec{
		{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/other"}}}},
		{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}, {Path: "/a/x"}}}},
	}
	if _, _, err = s.ReplaceProjectCatalogAndRematch(ctx, specs, nil, 10); err == nil {
		t.Fatal("fault swallowed")
	}
	s.beforePlacementFinalize = nil

	afterCat, err := s.ListProjectCatalog(ctx)
	if err != nil || !reflect.DeepEqual(afterCat, beforeCat) {
		t.Fatalf("catalog mutated by failed rematch: %#v err=%v", afterCat, err)
	}
	rec := localPlacementOf(t, s, "s1")
	if rec == nil || rec.project != int64(cat[0].ID) {
		t.Fatalf("placement mutated by failed rematch: %#v", rec)
	}
	assertKernelInvariants(t, s)
}
