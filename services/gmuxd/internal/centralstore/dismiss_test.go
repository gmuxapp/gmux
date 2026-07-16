package centralstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func regWithParent(id, parent, cwd string, at UnixMillis) RunnerRegistration {
	r := registration(id, "shell", cwd, true, at)
	if parent != "" {
		p := SessionID(parent)
		r.LaunchParentID = &p
	}
	return r
}

func mustRegister(t *testing.T, s *Store, r RunnerRegistration) Session {
	t.Helper()
	out, _, err := s.RegisterRunner(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// treeFixture: parent p with child c and grandchild g, plus an unrelated
// root sibling x, all placed in project "one".
func treeFixture(t *testing.T, s *Store) {
	t.Helper()
	registrationCatalog(t, s)
	mustRegister(t, s, regWithParent("p", "", "/one", 10))
	mustRegister(t, s, regWithParent("c", "p", "/one", 20))
	mustRegister(t, s, regWithParent("g", "c", "/one", 30))
	mustRegister(t, s, regWithParent("x", "", "/one", 40))
}

func TestDismissSessionTreeRecursive(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	before := mustSession(t, s, "c")

	dismissed, result, err := s.DismissSessionTree(ctx, "p", 500)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dismissed, []SessionID{"p", "c", "g"}) {
		t.Fatalf("dismissed=%v", dismissed)
	}
	if !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("result=%#v", result)
	}
	for _, id := range []SessionID{"p", "c", "g"} {
		v := mustSession(t, s, id)
		if v.DismissedAt == nil || *v.DismissedAt != 500 {
			t.Fatalf("%s not dismissed: %#v", id, v.DismissedAt)
		}
		if p := localPlacement(t, s, id); p != nil {
			t.Fatalf("%s placement must be removed: %#v", id, p)
		}
	}
	// Hidden, not forgotten: row, provenance, and history retained.
	after := mustSession(t, s, "c")
	if after.LaunchParentID == nil || *after.LaunchParentID != "p" {
		t.Fatalf("launch provenance lost: %#v", after.LaunchParentID)
	}
	if after.CreatedAt != before.CreatedAt || after.Version != before.Version+1 {
		t.Fatalf("history lost: before=%#v after=%#v", before, after)
	}
	// The surviving sibling's root scope is repaired densely.
	if p := localPlacement(t, s, "x"); p == nil || p.scope != "r" || p.pos != 0 {
		t.Fatalf("sibling scope not repaired: %#v", p)
	}
	if v := mustSession(t, s, "x"); v.DismissedAt != nil {
		t.Fatalf("unrelated sibling dismissed: %#v", v)
	}
}

func TestDismissSessionTreeRepairsChildSiblingScope(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	mustRegister(t, s, regWithParent("p", "", "/one", 10))
	for i, id := range []string{"c1", "c2", "c3"} {
		mustRegister(t, s, regWithParent(id, "p", "/one", UnixMillis(20+i)))
	}

	if _, _, err := s.DismissSessionTree(ctx, "c2", 500); err != nil {
		t.Fatal(err)
	}
	p1 := localPlacement(t, s, "c1")
	p3 := localPlacement(t, s, "c3")
	if p1 == nil || p3 == nil || p1.scope != "c:l:p" || p3.scope != "c:l:p" || p1.pos != 0 || p3.pos != 1 {
		t.Fatalf("child scope not dense: c1=%#v c3=%#v", p1, p3)
	}
	if pp := localPlacement(t, s, "p"); pp == nil || pp.scope != "r" || pp.pos != 0 {
		t.Fatalf("parent scope disturbed: %#v", pp)
	}
}

func TestDismissSessionTreeSkipsAlreadyDismissedMembers(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)

	if _, _, err := s.DismissSessionTree(ctx, "g", 100); err != nil {
		t.Fatal(err)
	}
	dismissed, _, err := s.DismissSessionTree(ctx, "p", 500)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dismissed, []SessionID{"p", "c"}) {
		t.Fatalf("dismissed=%v", dismissed)
	}
	if v := mustSession(t, s, "g"); v.DismissedAt == nil || *v.DismissedAt != 100 {
		t.Fatalf("earlier dismissal timestamp overwritten: %#v", v.DismissedAt)
	}
}

func TestDismissDismissedRootConvergesResurfacedDescendants(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	if _, _, err := s.DismissSessionTree(ctx, "p", 100); err != nil {
		t.Fatal(err)
	}
	// The child re-registers: dismissal cleared, visible again.
	mustRegister(t, s, regWithParent("c", "p", "/one", 200))
	if v := mustSession(t, s, "c"); v.DismissedAt != nil {
		t.Fatalf("re-registration must clear dismissal: %#v", v)
	}

	dismissed, result, err := s.DismissSessionTree(ctx, "p", 500)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dismissed, []SessionID{"c"}) || !result.Changed {
		t.Fatalf("dismissed=%v result=%#v", dismissed, result)
	}
	if v := mustSession(t, s, "p"); *v.DismissedAt != 100 {
		t.Fatalf("already-dismissed root re-stamped: %#v", v.DismissedAt)
	}
	if v := mustSession(t, s, "c"); v.DismissedAt == nil || *v.DismissedAt != 500 {
		t.Fatalf("resurfaced child not converged: %#v", v.DismissedAt)
	}
}

func TestDismissSessionTreeNoOpAndErrors(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)

	if _, _, err := s.DismissSessionTree(ctx, "missing", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err=%v", err)
	}
	if _, _, err := s.DismissSessionTree(ctx, "", 1); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if _, _, err := s.DismissSessionTree(ctx, "p", -1); err == nil {
		t.Fatal("negative timestamp must be rejected")
	}

	if _, _, err := s.DismissSessionTree(ctx, "p", 100); err != nil {
		t.Fatal(err)
	}
	dismissed, result, err := s.DismissSessionTree(ctx, "p", 200)
	if err != nil || dismissed != nil || result.Changed || result.SessionsDirty || result.WorldDirty {
		t.Fatalf("fully dismissed subtree must be a silent no-op: %v %#v err=%v", dismissed, result, err)
	}
}

func TestDismissSessionTreeRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	s.beforePlacementFinalize = func() error { return errors.New("injected") }
	if _, _, err := s.DismissSessionTree(ctx, "p", 500); err == nil {
		t.Fatal("injected fault must fail the transaction")
	}
	s.beforePlacementFinalize = nil
	for _, id := range []SessionID{"p", "c", "g"} {
		if v := mustSession(t, s, id); v.DismissedAt != nil {
			t.Fatalf("partial dismissal leaked for %s", id)
		}
		if p := localPlacement(t, s, id); p == nil {
			t.Fatalf("placement lost for %s despite rollback", id)
		}
	}
}

func TestDismissedSessionHiddenFromSnapshotButRetained(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	if _, _, err := s.DismissSessionTree(ctx, "p", 500); err != nil {
		t.Fatal(err)
	}
	snap, err := s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].ID != "x" {
		t.Fatalf("dismissed rows must be hidden: %#v", snap.Sessions)
	}
	if _, ok, err := s.Session(ctx, "g"); err != nil || !ok {
		t.Fatalf("dismissed row must be retained: ok=%v err=%v", ok, err)
	}
}

// Undismissal placement is emergent: a re-registering dismissed child is
// rematched and appended, and groups under its launch parent only when the
// parent is visible (placed) in the same project — a dismissed parent has no
// placement, so the child resurfaces as a root.
func TestUndismissalResurfacesAsRootWhenParentDismissed(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	if _, _, err := s.DismissSessionTree(ctx, "p", 500); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, regWithParent("c", "p", "/one", 600))
	p := localPlacement(t, s, "c")
	if p == nil || p.scope != "r" || p.pos != 1 { // appended after surviving root "x"
		t.Fatalf("child must resurface as appended root: %#v", p)
	}
	if v := mustSession(t, s, "c"); v.LaunchParentID == nil || *v.LaunchParentID != "p" {
		t.Fatalf("provenance must survive undismissal: %#v", v)
	}
}

func TestUndismissalResurfacesUnderVisibleParent(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	if _, _, err := s.DismissSessionTree(ctx, "c", 500); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, regWithParent("c", "p", "/one", 600))
	p := localPlacement(t, s, "c")
	if p == nil || p.scope != "c:l:p" || p.pos != 0 {
		t.Fatalf("child must regroup under its visible parent: %#v", p)
	}
	// Undismissal is per-registered-runner, never recursive: the grandchild
	// (dismissed as part of c's subtree) stays hidden and unplaced.
	if v := mustSession(t, s, "g"); v.DismissedAt == nil {
		t.Fatalf("grandchild must stay dismissed: %#v", v)
	}
	if g := localPlacement(t, s, "g"); g != nil {
		t.Fatalf("dismissed grandchild must stay unplaced: %#v", g)
	}
}

// Parent deletion promotes surviving direct children to genuine roots by
// clearing launch_parent_id only. The sticky promoted_to_root bit is
// user-authored presentation state and is neither set nor cleared by
// deletion; grandchildren keep their own provenance.
func TestParentDeletionPromotesChildrenAsGenuineRoots(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	mustRegister(t, s, regWithParent("p", "", "/one", 10))
	mustRegister(t, s, regWithParent("c1", "p", "/one", 20))
	mustRegister(t, s, regWithParent("c2", "p", "/one", 30))
	mustRegister(t, s, regWithParent("g", "c2", "/one", 40))
	mustRegister(t, s, regWithParent("x", "", "/one", 50))
	if _, err := s.SetPromotion(ctx, "c1", true, nil); err != nil {
		t.Fatal(err)
	}
	parent := mustSession(t, s, "p")

	result, err := s.RemoveSessionAtVersion(ctx, "p", parent.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("result=%#v", result)
	}
	if _, ok, err := s.Session(ctx, "p"); err != nil || ok {
		t.Fatalf("parent must be deleted: ok=%v err=%v", ok, err)
	}
	c1 := mustSession(t, s, "c1")
	c2 := mustSession(t, s, "c2")
	g := mustSession(t, s, "g")
	if c1.LaunchParentID != nil || c2.LaunchParentID != nil {
		t.Fatalf("children parents not cleared: %#v %#v", c1.LaunchParentID, c2.LaunchParentID)
	}
	if !c1.PromotedToRoot {
		t.Fatal("existing sticky promotion must survive parent deletion")
	}
	if c2.PromotedToRoot {
		t.Fatal("deletion must not fabricate a sticky promotion")
	}
	if g.LaunchParentID == nil || *g.LaunchParentID != "c2" {
		t.Fatalf("grandchild provenance must survive: %#v", g.LaunchParentID)
	}
	// Root scope repaired densely across all affected scopes; grandchild
	// stays grouped under its (now root) parent.
	positions := map[SessionID]int64{}
	for _, id := range []SessionID{"c1", "c2", "x"} {
		p := localPlacement(t, s, id)
		if p == nil || p.scope != "r" {
			t.Fatalf("%s must be a root: %#v", id, p)
		}
		positions[id] = p.pos
	}
	seen := map[int64]bool{}
	for id, pos := range positions {
		if pos < 0 || pos > 2 || seen[pos] {
			t.Fatalf("root scope not dense: %s at %d (%v)", id, pos, positions)
		}
		seen[pos] = true
	}
	if p := localPlacement(t, s, "g"); p == nil || p.scope != "c:l:c2" || p.pos != 0 {
		t.Fatalf("grandchild scope: %#v", p)
	}
}

// promoted_to_root is sticky presentation state: registration merges never
// touch it, including re-registration after dismissal — a promoted child
// resurfaces as a root even when its parent is visible.
func TestPromotionSurvivesReRegistrationAndDismissal(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	treeFixture(t, s)
	if _, err := s.SetPromotion(ctx, "c", true, nil); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, regWithParent("c", "p", "/one", 100))
	if v := mustSession(t, s, "c"); !v.PromotedToRoot {
		t.Fatal("promotion lost on re-registration")
	}
	if _, _, err := s.DismissSessionTree(ctx, "c", 200); err != nil {
		t.Fatal(err)
	}
	if v := mustSession(t, s, "c"); !v.PromotedToRoot {
		t.Fatal("promotion lost on dismissal")
	}
	mustRegister(t, s, regWithParent("c", "p", "/one", 300))
	v := mustSession(t, s, "c")
	if !v.PromotedToRoot || v.DismissedAt != nil {
		t.Fatalf("undismissed promoted child: %#v", v)
	}
	if p := localPlacement(t, s, "c"); p == nil || p.scope != "r" {
		t.Fatalf("promoted child must resurface as root: %#v", p)
	}
}
