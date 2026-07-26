package centralstore

import (
	"context"
	"errors"
	"testing"
)

// evictFixture registers a dead loser with a child and a sibling, all placed
// in project one, and returns the loser's committed row.
func evictFixture(t *testing.T, s *Store) Session {
	t.Helper()
	ctx := context.Background()
	loser := registration("loser", "pi", "/one", false, 10)
	loser.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(11))}
	ref := "conv-a"
	loser.Facts.ConversationRef = &ref
	row, _, err := s.RegisterRunner(ctx, loser)
	if err != nil {
		t.Fatal(err)
	}

	parent := SessionID("loser")
	child := registration("child", "pi", "/one", false, 12)
	child.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(13))}
	child.LaunchParentID = &parent
	if _, _, err = s.RegisterRunner(ctx, child); err != nil {
		t.Fatal(err)
	}

	sibling := registration("sibling", "pi", "/one", false, 14)
	sibling.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(15))}
	if _, _, err = s.RegisterRunner(ctx, sibling); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestRegisterRunnerEvictsTakeoverLosers(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	loser := evictFixture(t, s)

	winner := registration("winner", "pi", "/one", true, 20)
	ref := "conv-a"
	winner.Facts.ConversationRef = &ref
	winner.Evict = []TakeoverEviction{{ID: "loser", Version: loser.Version}}
	got, result, err := s.RegisterRunner(ctx, winner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty || !result.WorldDirty {
		t.Fatalf("eviction result=%#v", result)
	}
	if _, ok, _ := s.Session(ctx, "loser"); ok {
		t.Fatal("loser row survived takeover")
	}
	// Winner is intact and placed.
	if got.ID != "winner" || got.ExitedAt != nil {
		t.Fatalf("winner=%#v", got)
	}
	// The loser's child was promoted to a genuine root: parent cleared,
	// sticky promotion untouched, its own row retained.
	child := mustSession(t, s, "child")
	if child.LaunchParentID != nil || child.PromotedToRoot {
		t.Fatalf("child not promoted to genuine root: %#v", child)
	}
	// Placement scope is densely renormalized: loser's placement is gone,
	// survivors occupy 0..n-1 in the root scope.
	if p := localPlacement(t, s, "loser"); p != nil {
		t.Fatalf("loser placement survived: %#v", p)
	}
	positions := map[int64]bool{}
	for _, id := range []SessionID{"child", "sibling", "winner"} {
		p := localPlacement(t, s, id)
		if p == nil {
			t.Fatalf("placement missing for %s", id)
		}
		positions[p.pos] = true
	}
	if !positions[0] || !positions[1] || !positions[2] {
		t.Fatalf("scope not dense: %v", positions)
	}
}

func TestRegisterRunnerEvictionSkipsStaleAndMissingLosers(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	loser := evictFixture(t, s)

	// Bump the loser's version after the observed decision (any conditional
	// mutation between decision and apply).
	if _, err := s.ApplyCommonFacts(ctx, "loser", loser.Version, CommonFactsPatch{Unread: ptr(true)}); err != nil {
		t.Fatal(err)
	}

	winner := registration("winner", "pi", "/one", true, 20)
	ref := "conv-a"
	winner.Facts.ConversationRef = &ref
	winner.Evict = []TakeoverEviction{
		{ID: "loser", Version: loser.Version}, // stale
		{ID: "never-existed", Version: 1},     // missing
	}
	_, result, err := s.RegisterRunner(ctx, winner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatalf("registration itself must commit: %#v", result)
	}
	if _, ok, _ := s.Session(ctx, "loser"); !ok {
		t.Fatal("stale eviction must be skipped, not applied")
	}
	child := mustSession(t, s, "child")
	if child.LaunchParentID == nil || *child.LaunchParentID != "loser" {
		t.Fatalf("skipped eviction must not clear child parents: %#v", child)
	}
}

func TestRegisterRunnerEvictionValidation(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)

	dead := registration("dead-binder", "pi", "/one", false, 10)
	dead.Facts.ExitedAt = NullablePatch[UnixMillis]{Set: ptr(UnixMillis(11))}
	dead.Evict = []TakeoverEviction{{ID: "x", Version: 1}}
	if _, _, err := s.RegisterRunner(ctx, dead); !errors.Is(err, ErrInvalidEviction) {
		t.Fatalf("dead registration with evictions: %v", err)
	}

	self := registration("selfish", "pi", "/one", true, 10)
	self.Evict = []TakeoverEviction{{ID: "selfish", Version: 1}}
	if _, _, err := s.RegisterRunner(ctx, self); !errors.Is(err, ErrInvalidEviction) {
		t.Fatalf("self-eviction: %v", err)
	}

	empty := registration("emptyish", "pi", "/one", true, 10)
	empty.Evict = []TakeoverEviction{{ID: "", Version: 1}}
	if _, _, err := s.RegisterRunner(ctx, empty); !errors.Is(err, ErrInvalidEviction) {
		t.Fatalf("empty loser id: %v", err)
	}

	if _, ok, _ := s.Session(ctx, "dead-binder"); ok {
		t.Fatal("failed validation must not commit any row")
	}
}

func TestRegisterRunnerEvictionRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	loser := evictFixture(t, s)

	s.beforePlacementFinalize = func() error { return errors.New("injected") }
	winner := registration("winner", "pi", "/one", true, 20)
	winner.Evict = []TakeoverEviction{{ID: "loser", Version: loser.Version}}
	if _, _, err := s.RegisterRunner(ctx, winner); err == nil {
		t.Fatal("injected fault must fail the transaction")
	}
	s.beforePlacementFinalize = nil

	if _, ok, _ := s.Session(ctx, "loser"); !ok {
		t.Fatal("rollback must restore the loser row")
	}
	if _, ok, _ := s.Session(ctx, "winner"); ok {
		t.Fatal("rollback must not leave the winner row")
	}
	child := mustSession(t, s, "child")
	if child.LaunchParentID == nil {
		t.Fatal("rollback must restore child provenance")
	}
	if p := localPlacement(t, s, "loser"); p == nil {
		t.Fatal("rollback must restore the loser's placement row")
	}
}

// TestRegisterRunnerEvictingOwnLaunchParent pins the returned-row contract
// when the winner is a direct child of its evicted loser: eviction's
// ClearDirectChildParents nulls the winner's launch parent and bumps its
// row_version inside the same transaction, and the returned Session /
// MutationResult must mirror the committed row — no dangling parent, no
// stale version token for the registry.
func TestRegisterRunnerEvictingOwnLaunchParent(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	loser := evictFixture(t, s)

	parent := SessionID("loser")
	winner := registration("winner", "pi", "/one", true, 20)
	winner.LaunchParentID = &parent
	ref := "conv-a"
	winner.Facts.ConversationRef = &ref
	winner.Evict = []TakeoverEviction{{ID: "loser", Version: loser.Version}}
	got, result, err := s.RegisterRunner(ctx, winner)
	if err != nil {
		t.Fatal(err)
	}
	if got.LaunchParentID != nil {
		t.Fatalf("returned session must not name the deleted loser as parent: %#v", got.LaunchParentID)
	}
	committed := mustSession(t, s, "winner")
	if committed.LaunchParentID != nil {
		t.Fatalf("committed parent=%#v", committed.LaunchParentID)
	}
	if got.Version != committed.Version || result.SessionVersion != committed.Version {
		t.Fatalf("returned=%d result=%d committed=%d", got.Version, result.SessionVersion, committed.Version)
	}
	if _, ok, _ := s.Session(ctx, "loser"); ok {
		t.Fatal("loser must be evicted")
	}
}

func TestRegisterRunnerEvictionOfDismissedLoser(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	registrationCatalog(t, s)
	loser := evictFixture(t, s)

	// Dismiss the loser subtree; its rows are hidden but retained.
	dismissed, _, err := s.DismissSessionTree(ctx, "loser", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(dismissed) != 2 {
		t.Fatalf("dismissed=%v", dismissed)
	}
	current := mustSession(t, s, "loser")
	if current.Version == loser.Version {
		t.Fatal("dismissal should bump the version")
	}

	winner := registration("winner", "pi", "/one", true, 40)
	winner.Evict = []TakeoverEviction{{ID: "loser", Version: current.Version}}
	if _, _, err = s.RegisterRunner(ctx, winner); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Session(ctx, "loser"); ok {
		t.Fatal("dismissed loser must still be evictable")
	}
	// The dismissed child stays dismissed but becomes a genuine root.
	child := mustSession(t, s, "child")
	if child.LaunchParentID != nil || child.DismissedAt == nil {
		t.Fatalf("child=%#v", child)
	}
}
