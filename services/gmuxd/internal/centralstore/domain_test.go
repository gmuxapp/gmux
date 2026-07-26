package centralstore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func openKernelStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func addSessionAt(t *testing.T, s *Store, id, parent string, created UnixMillis) Session {
	t.Helper()
	v := NewSession{ID: SessionID(id), Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{}, CreatedAt: created, ShellTitle: "shell title"}
	if parent != "" {
		p := SessionID(parent)
		v.LaunchParentID = &p
	}
	out, result, err := s.InsertSession(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.SessionsDirty || !result.WorldDirty || result.SessionVersion != 1 {
		t.Fatalf("insert result=%#v", result)
	}
	return out
}
func addSession(t *testing.T, s *Store, id, parent string) Session {
	return addSessionAt(t, s, id, parent, 1)
}
func owned(slug, path string) ProjectEntrySpec {
	return ProjectEntrySpec{Owned: &OwnedProjectSpec{Slug: slug, Rules: []MatchRule{{Path: path}}}}
}
func addProject(t *testing.T, s *Store) ProjectEntryID {
	t.Helper()
	cat, r, err := s.ReplaceProjectCatalog(context.Background(), []ProjectEntrySpec{owned("p", "/")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !r.WorldDirty {
		return 0
	}
	return cat[0].ID
}

func rootOrder(t *testing.T, s *Store, p ProjectEntryID) []string {
	t.Helper()
	all, err := placements(context.Background(), s.queries)
	if err != nil {
		t.Fatal(err)
	}
	var rows []*placementRec
	for _, r := range all {
		if r.project == int64(p) && r.scope == "r" {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pos < rows[j].pos })
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = recKey(r)
	}
	return out
}
func scopeOrder(t *testing.T, s *Store, p ProjectEntryID, scope string) []string {
	t.Helper()
	all, err := placements(context.Background(), s.queries)
	if err != nil {
		t.Fatal(err)
	}
	var rows []*placementRec
	for _, r := range all {
		if r.project == int64(p) && r.scope == scope {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pos < rows[j].pos })
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = recKey(r)
	}
	return out
}

func TestChildFirstAndLaterParentCycle(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	child := addSession(t, s, "child", "parent")
	if child.LaunchParentID == nil || *child.LaunchParentID != "parent" {
		t.Fatalf("parent not preserved: %#v", child)
	}
	addSession(t, s, "parent", "")
	s2 := openKernelStore(t)
	addSession(t, s2, "a", "b")
	p := SessionID("a")
	_, _, err := s2.InsertSession(ctx, NewSession{ID: "b", Adapter: "shell", Command: []string{}, CWD: "/", Remotes: map[string]string{}, CreatedAt: 1, LaunchParentID: &p})
	if err == nil {
		t.Fatal("cycle insertion succeeded")
	}
}

func TestDedicatedInsertAndTriStatePatch(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	epoch := UnixMillis(0)
	cols, rows := uint16(80), uint16(24)
	v, _, err := s.InsertSession(ctx, NewSession{ID: "s", Adapter: "shell", Command: nil, CWD: "/", Remotes: nil, CreatedAt: 0, StartedAt: &epoch, TerminalCols: &cols, TerminalRows: &rows})
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != 1 || v.DismissedAt != nil || v.PromotedToRoot || v.Command == nil || v.Remotes == nil || v.StartedAt == nil || *v.StartedAt != 0 {
		t.Fatalf("round trip=%#v", v)
	}
	clear := CommonFactsPatch{StartedAt: NullablePatch[UnixMillis]{Clear: true}, TerminalSize: NullablePatch[TerminalSize]{Clear: true}}
	r, err := s.ApplyCommonFacts(ctx, "s", 1, clear)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || !r.SessionsDirty || r.WorldDirty || r.SessionVersion != 2 {
		t.Fatalf("patch result=%#v", r)
	}
	got, _, _ := s.Session(ctx, "s")
	if got.StartedAt != nil || got.TerminalCols != nil || got.TerminalRows != nil {
		t.Fatalf("clear failed: %#v", got)
	}
	x := UnixMillis(1)
	if _, err = s.ApplyCommonFacts(ctx, "s", 2, CommonFactsPatch{ExitedAt: NullablePatch[UnixMillis]{Set: &x, Clear: true}}); err == nil {
		t.Fatal("set+clear accepted")
	}
	bad := TerminalSize{Cols: 80}
	if _, err = s.ApplyCommonFacts(ctx, "s", 2, CommonFactsPatch{TerminalSize: NullablePatch[TerminalSize]{Set: &bad}}); err == nil {
		t.Fatal("one-sided terminal size accepted")
	}
}

func TestTitleNoopObservedVersionAndCorruptJSON(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	v := addSession(t, s, "s", "")
	adapter := "adapter title"
	got, err := s.ApplyCommonFacts(ctx, "s", v.Version, CommonFactsPatch{AdapterTitle: &adapter})
	if err != nil {
		t.Fatal(err)
	}
	noop, err := s.ApplyCommonFacts(ctx, "s", got.SessionVersion, CommonFactsPatch{AdapterTitle: &adapter})
	if err != nil || noop.Changed || noop.SessionVersion != 2 {
		t.Fatalf("noop=%#v err=%v", noop, err)
	}
	if _, err = s.ApplyCommonFacts(ctx, "s", v.Version, CommonFactsPatch{AdapterTitle: &adapter}); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("stale err=%v", err)
	}
	out, ok, err := s.Session(ctx, "s")
	if err != nil || !ok || out.Title != "adapter title" || out.ShellTitle != "shell title" {
		t.Fatalf("out=%#v err=%v", out, err)
	}
	if _, err = s.database.ExecContext(ctx, "PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.database.ExecContext(ctx, `UPDATE local_sessions SET command_json='not-json' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Session(ctx, "s"); err == nil {
		t.Fatal("malformed durable JSON was hidden")
	}
}

func TestCatalogIdentityValidationDenseAndRollback(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	initial := []ProjectEntrySpec{owned("gmux", "/work"), {Reference: &ProjectReference{PeerKey: "container", Slug: "gmux"}}}
	cat, r, err := s.ReplaceProjectCatalog(ctx, initial, 10)
	if err != nil || len(cat) != 2 || !r.Changed {
		t.Fatalf("cat=%#v result=%#v err=%v", cat, r, err)
	}
	unchanged := []ProjectEntrySpec{{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "gmux", Rules: []MatchRule{{Path: "/work"}}}}, {ID: cat[1].ID, Reference: &ProjectReference{PeerKey: "container", Slug: "gmux"}}}
	_, r, err = s.ReplaceProjectCatalog(ctx, unchanged, 20)
	if err != nil || r.Changed {
		t.Fatalf("catalog noop=%#v err=%v", r, err)
	}
	cases := []struct {
		name string
		in   []ProjectEntrySpec
	}{
		{"duplicate id", []ProjectEntrySpec{
			{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/a"}}}},
			{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}}}},
		}},
		{"unknown id", []ProjectEntrySpec{{ID: 999, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/a"}}}}}},
		{"kind mismatch", []ProjectEntrySpec{{ID: cat[0].ID, Reference: &ProjectReference{PeerKey: "p", Slug: "gmux"}}}},
		{"peer mismatch", []ProjectEntrySpec{{ID: cat[1].ID, Reference: &ProjectReference{PeerKey: "other", Slug: "gmux"}}}},
		{"empty rules", []ProjectEntrySpec{{Owned: &OwnedProjectSpec{Slug: "x"}}}},
		{"rule xor", []ProjectEntrySpec{{Owned: &OwnedProjectSpec{Slug: "x", Rules: []MatchRule{{Path: "/x", Remote: "r"}}}}}},
		{"remote exact", []ProjectEntrySpec{{Owned: &OwnedProjectSpec{Slug: "x", Rules: []MatchRule{{Remote: "r", Exact: true}}}}}},
		{"normalized duplicate", []ProjectEntrySpec{
			{Owned: &OwnedProjectSpec{Slug: "x", Rules: []MatchRule{{Path: "/a/../b"}}}},
			{Owned: &OwnedProjectSpec{Slug: "y", Rules: []MatchRule{{Path: "/b"}}}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.ReplaceProjectCatalog(ctx, tc.in, 30); err == nil {
				t.Fatal("invalid catalog accepted")
			}
			after, e := s.ListProjectCatalog(ctx)
			if e != nil || !reflect.DeepEqual(after, cat) {
				t.Fatalf("mutation leaked: %#v err=%v", after, e)
			}
		})
	}
	reordered := []ProjectEntrySpec{{ID: cat[1].ID, Reference: &ProjectReference{PeerKey: "container", Slug: "gmux"}}, {ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "gmux", Rules: []MatchRule{{Path: "/new"}}}}}
	after, _, err := s.ReplaceProjectCatalog(ctx, reordered, 40)
	if err != nil || after[0].ID != cat[1].ID || after[1].Rules[0].Path != "/new" {
		t.Fatalf("reorder=%#v err=%v", after, err)
	}
	after, _, err = s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "gmux", Rules: []MatchRule{{Path: "/new"}}}}}, 50)
	if err != nil || len(after) != 1 || after[0].ID != cat[0].ID {
		t.Fatalf("dense deletion=%#v err=%v", after, err)
	}
	assertKernelInvariants(t, s)
}

func TestCatalogTimestampContractAndBootstrapRestriction(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cat[0].CreatedAt != 0 || cat[0].UpdatedAt != 0 || cat[1].CreatedAt != 0 || cat[1].UpdatedAt != 0 {
		t.Fatalf("epoch timestamps=%#v", cat)
	}
	unchanged := []ProjectEntrySpec{
		{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/a"}}}},
		{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}}}},
	}
	cat, result, err := s.ReplaceProjectCatalog(ctx, unchanged, 7)
	if err != nil || result.Changed || cat[0].UpdatedAt != 0 || cat[1].UpdatedAt != 0 {
		t.Fatalf("timestamp noop=%#v result=%#v err=%v", cat, result, err)
	}
	withNew := append(append([]ProjectEntrySpec{}, unchanged...), owned("c", "/c"))
	cat, _, err = s.ReplaceProjectCatalog(ctx, withNew, 8)
	if err != nil || cat[0].UpdatedAt != 0 || cat[1].UpdatedAt != 0 || cat[2].CreatedAt != 8 || cat[2].UpdatedAt != 8 {
		t.Fatalf("change-only append=%#v err=%v", cat, err)
	}
	changed := []ProjectEntrySpec{
		{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "renamed", Rules: []MatchRule{{Path: "/a"}}}},
		{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/b"}}}},
		{ID: cat[2].ID, Owned: &OwnedProjectSpec{Slug: "c", Rules: []MatchRule{{Path: "/c"}}}},
	}
	cat, _, err = s.ReplaceProjectCatalog(ctx, changed, 9)
	if err != nil || cat[0].CreatedAt != 0 || cat[0].UpdatedAt != 9 || cat[1].UpdatedAt != 0 || cat[2].UpdatedAt != 8 {
		t.Fatalf("change-only update=%#v err=%v", cat, err)
	}
	before := append(ProjectCatalog(nil), cat...)
	if _, _, err = s.ReplaceProjectCatalog(ctx, changed, -1); err == nil {
		t.Fatal("negative catalog timestamp accepted")
	}
	after, err := s.ListProjectCatalog(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("negative timestamp mutated catalog: %#v err=%v", after, err)
	}
	conflict := []ProjectEntrySpec{
		{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "same", Rules: []MatchRule{{Path: "/a"}}}},
		{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "same", Rules: []MatchRule{{Path: "/b"}}}},
		{ID: cat[2].ID, Owned: &OwnedProjectSpec{Slug: "c", Rules: []MatchRule{{Path: "/c"}}}},
	}
	if _, _, err = s.ReplaceProjectCatalog(ctx, conflict, 11); err == nil {
		t.Fatal("conflicting timestamped update succeeded")
	}
	after, err = s.ListProjectCatalog(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed update changed timestamps/catalog: %#v err=%v", after, err)
	}
	addSession(t, s, "placed", "")
	if _, err = s.PlaceLocalSession(ctx, "placed", cat[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReplaceProjectCatalog(ctx, changed, 10); !errors.Is(err, ErrCatalogHasPlacements) {
		t.Fatalf("catalog with placements err=%v", err)
	}
}

func TestCatalogRulePathSwap(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
		{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/b"}}}},
		{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/a"}}}},
	}, 2)
	if err != nil || got[0].Rules[0].Path != "/b" || got[1].Rules[0].Path != "/a" {
		t.Fatalf("path swap=%#v err=%v", got, err)
	}
}

func TestCatalogSlugSwapAndReuse(t *testing.T) {
	ctx := context.Background()

	t.Run("owned slug swap", func(t *testing.T) {
		s := openKernelStore(t)
		cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("a", "/a"), owned("b", "/b")}, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
			{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "b", Rules: []MatchRule{{Path: "/a"}}}},
			{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "a", Rules: []MatchRule{{Path: "/b"}}}},
		}, 2)
		if err != nil || got[0].ID != cat[0].ID || got[0].Slug != "b" || got[1].ID != cat[1].ID || got[1].Slug != "a" {
			t.Fatalf("slug swap=%#v err=%v", got, err)
		}
		assertKernelInvariants(t, s)
	})

	t.Run("owned delete and re-add same slug", func(t *testing.T) {
		s := openKernelStore(t)
		cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("x", "/x"), owned("keep", "/keep")}, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
			{ID: cat[1].ID, Owned: &OwnedProjectSpec{Slug: "keep", Rules: []MatchRule{{Path: "/keep"}}}},
			owned("x", "/renewed"),
		}, 2)
		if err != nil || len(got) != 2 || got[1].Slug != "x" || got[1].ID == cat[0].ID || got[1].CreatedAt != 2 {
			t.Fatalf("delete+re-add=%#v err=%v (old id=%d)", got, err, cat[0].ID)
		}
		assertKernelInvariants(t, s)
	})

	t.Run("reference delete and re-add swapped slugs", func(t *testing.T) {
		s := openKernelStore(t)
		_, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
			{Reference: &ProjectReference{PeerKey: "peer", Slug: "one"}},
			{Reference: &ProjectReference{PeerKey: "peer", Slug: "two"}},
		}, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
			{Reference: &ProjectReference{PeerKey: "peer", Slug: "two"}},
			{Reference: &ProjectReference{PeerKey: "peer", Slug: "one"}},
		}, 2)
		if err != nil || len(got) != 2 || got[0].Slug != "two" || got[1].Slug != "one" {
			t.Fatalf("reference swap=%#v err=%v", got, err)
		}
		assertKernelInvariants(t, s)
	})

	t.Run("reference slug is identity and immutable in place", func(t *testing.T) {
		s := openKernelStore(t)
		cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{{Reference: &ProjectReference{PeerKey: "peer", Slug: "one"}}}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{{ID: cat[0].ID, Reference: &ProjectReference{PeerKey: "peer", Slug: "renamed"}}}, 2); err == nil {
			t.Fatal("in-place reference slug rebind accepted")
		}
		after, err := s.ListProjectCatalog(ctx)
		if err != nil || !reflect.DeepEqual(after, cat) {
			t.Fatalf("rejected rebind mutated catalog: %#v err=%v", after, err)
		}
	})
}

func TestSessionMutationsReturnErrSessionNotFound(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	if _, err := s.ApplyCommonFacts(ctx, "missing", 1, CommonFactsPatch{}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ApplyCommonFacts err=%v", err)
	}
	if _, err := s.RemoveSessionAtVersion(ctx, "missing", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("RemoveSessionAtVersion err=%v", err)
	}
	if _, err := s.SetPromotion(ctx, "missing", true, nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SetPromotion err=%v", err)
	}
}

func TestLaunchParentUpdateTriggerRejectsRebind(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	addSession(t, s, "parent", "")
	addSession(t, s, "child", "parent")
	if _, err := s.database.ExecContext(ctx, `UPDATE local_sessions SET launch_parent_id='child' WHERE id='parent'`); err == nil {
		t.Fatal("non-NULL launch parent update accepted")
	}
	if _, err := s.database.ExecContext(ctx, `UPDATE local_sessions SET launch_parent_id=NULL WHERE id='child'`); err != nil {
		t.Fatalf("NULL-ing launch parent rejected: %v", err)
	}
}

func TestRepeatedLocalAndPeerPlacementPreservesOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	for _, id := range []string{"a", "b", "c"} {
		addSession(t, s, id, "")
		if _, err := s.PlaceLocalSession(ctx, SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	before := rootOrder(t, s, p)
	r, err := s.PlaceLocalSession(ctx, "b", p)
	if err != nil || r.Changed {
		t.Fatalf("repeat local=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, before) {
		t.Fatalf("local moved: %v -> %v", before, got)
	}
	peers := []LocalPeerSubject{{PeerKey: "dev", SessionID: "x"}, {PeerKey: "dev", SessionID: "y"}, {PeerKey: "dev", SessionID: "z"}}
	for _, peer := range peers {
		if _, err = s.UpsertLocalPeerPlacement(ctx, peer, p); err != nil {
			t.Fatal(err)
		}
	}
	before = rootOrder(t, s, p)
	r, err = s.UpsertLocalPeerPlacement(ctx, peers[1], p)
	if err != nil || r.Changed {
		t.Fatalf("repeat peer=%#v err=%v", r, err)
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, before) {
		t.Fatalf("peer moved: %v -> %v", before, got)
	}
	assertKernelInvariants(t, s)
}

func TestLocalPeerParentScopeTransitionsAppend(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	child := LocalPeerSubject{PeerKey: "dev", SessionID: "child", ParentSessionID: "parent"}
	if _, err := s.UpsertLocalPeerPlacement(ctx, child, p); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, []string{"p:dev:child"}) {
		t.Fatalf("unresolved child=%v", got)
	}
	parent := LocalPeerSubject{PeerKey: "dev", SessionID: "parent"}
	if _, err := s.UpsertLocalPeerPlacement(ctx, parent, p); err != nil {
		t.Fatal(err)
	}
	if got := scopeOrder(t, s, p, "c:p:dev:parent"); !reflect.DeepEqual(got, []string{"p:dev:child"}) {
		t.Fatalf("resolved child=%v", got)
	}
	child.ParentSessionID = "missing"
	if _, err := s.UpsertLocalPeerPlacement(ctx, child, p); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, []string{"p:dev:parent", "p:dev:child"}) {
		t.Fatalf("re-root append=%v", got)
	}
	assertKernelInvariants(t, s)
}

func TestLocalPeerParentCyclesRollback(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup []LocalPeerSubject
		bad   LocalPeerSubject
	}{
		{name: "self", bad: LocalPeerSubject{PeerKey: "dev", SessionID: "a", ParentSessionID: "a"}},
		{name: "two node", setup: []LocalPeerSubject{{PeerKey: "dev", SessionID: "a", ParentSessionID: "b"}}, bad: LocalPeerSubject{PeerKey: "dev", SessionID: "b", ParentSessionID: "a"}},
		{name: "deeper", setup: []LocalPeerSubject{{PeerKey: "dev", SessionID: "a", ParentSessionID: "b"}, {PeerKey: "dev", SessionID: "b", ParentSessionID: "c"}}, bad: LocalPeerSubject{PeerKey: "dev", SessionID: "c", ParentSessionID: "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openKernelStore(t)
			p := addProject(t, s)
			for _, peer := range tc.setup {
				if _, err := s.UpsertLocalPeerPlacement(ctx, peer, p); err != nil {
					t.Fatal(err)
				}
			}
			before, err := placements(ctx, s.queries)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.UpsertLocalPeerPlacement(ctx, tc.bad, p); !errors.Is(err, ErrLocalPeerParentCycle) {
				t.Fatalf("cycle err=%v", err)
			}
			after, err := placements(ctx, s.queries)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("cycle mutation leaked: before=%#v after=%#v", before, after)
			}
			assertKernelInvariants(t, s)
		})
	}
	t.Run("existing row mutation", func(t *testing.T) {
		s := openKernelStore(t)
		p := addProject(t, s)
		a := LocalPeerSubject{PeerKey: "dev", SessionID: "a"}
		b := LocalPeerSubject{PeerKey: "dev", SessionID: "b", ParentSessionID: "a"}
		if _, err := s.UpsertLocalPeerPlacement(ctx, a, p); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertLocalPeerPlacement(ctx, b, p); err != nil {
			t.Fatal(err)
		}
		before, err := placements(ctx, s.queries)
		if err != nil {
			t.Fatal(err)
		}
		a.ParentSessionID = "b"
		if _, err = s.UpsertLocalPeerPlacement(ctx, a, p); !errors.Is(err, ErrLocalPeerParentCycle) {
			t.Fatalf("cycle err=%v", err)
		}
		after, err := placements(ctx, s.queries)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("existing row cycle mutation leaked")
		}
	})
}

func TestLaterParentRegroupUsesCreationOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	addSessionAt(t, s, "z", "parent", 1)
	addSessionAt(t, s, "a", "parent", 2)
	for _, id := range []SessionID{"z", "a"} {
		if _, err := s.PlaceLocalSession(ctx, id, p); err != nil {
			t.Fatal(err)
		}
	}
	addSessionAt(t, s, "parent", "", 3)
	if _, err := s.PlaceLocalSession(ctx, "parent", p); err != nil {
		t.Fatal(err)
	}
	got := scopeOrder(t, s, p, "c:l:parent")
	want := []string{"l:z", "l:a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("regroup order=%v want=%v", got, want)
	}
}

func TestPromotionNoopVersionAndExplicitOrder(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	for _, id := range []string{"a", "b"} {
		addSession(t, s, id, "")
		if _, err := s.PlaceLocalSession(ctx, SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.SetPromotion(ctx, "a", false, nil)
	if err != nil || r.Changed || r.SessionVersion != 1 {
		t.Fatalf("promotion noop=%#v err=%v", r, err)
	}
	idx := 0
	r, err = s.SetPromotion(ctx, "a", false, &idx)
	if err != nil || r.Changed || r.SessionVersion != 1 {
		t.Fatalf("order noop=%#v err=%v", r, err)
	}
	idx = 1
	r, err = s.SetPromotion(ctx, "a", false, &idx)
	if err != nil || !r.Changed || r.SessionsDirty || !r.WorldDirty || r.SessionVersion != 1 {
		t.Fatalf("order-only=%#v err=%v", r, err)
	}
	r, err = s.SetPromotion(ctx, "a", true, nil)
	if err != nil || !r.Changed || !r.SessionsDirty || r.SessionVersion != 2 {
		t.Fatalf("promotion=%#v err=%v", r, err)
	}
	r, err = s.SetPromotion(ctx, "a", true, nil)
	if err != nil || r.Changed || r.SessionVersion != 2 {
		t.Fatalf("second noop=%#v err=%v", r, err)
	}
	bad := 9
	if _, err = s.SetPromotion(ctx, "a", true, &bad); err == nil {
		t.Fatal("invalid index accepted")
	}
	got, _, _ := s.Session(ctx, "a")
	if got.Version != 2 {
		t.Fatalf("rollback changed version: %d", got.Version)
	}
}

func TestReorderValidatesProjectParentAndDuplicates(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
		owned("one", "/one"), owned("two", "/two"),
		{Reference: &ProjectReference{PeerKey: "dev", Slug: "ref"}},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	p1, p2, ref := cat[0].ID, cat[1].ID, cat[2].ID
	addSession(t, s, "parent", "")
	addSession(t, s, "other", "")
	if _, err = s.PlaceLocalSession(ctx, "parent", p1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlaceLocalSession(ctx, "other", p2); err != nil {
		t.Fatal(err)
	}
	peer := LocalPeerSubject{PeerKey: "dev", SessionID: "peer"}
	if _, err = s.UpsertLocalPeerPlacement(ctx, peer, p1); err != nil {
		t.Fatal(err)
	}
	mixed := []SubjectRef{{LocalPeer: &peer}, {LocalSessionID: "parent"}}
	if _, err = s.ReorderSiblings(ctx, p1, ParentRef{}, mixed); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, p1); !reflect.DeepEqual(got, []string{"p:dev:peer", "l:parent"}) {
		t.Fatalf("mixed sibling reorder=%v", got)
	}
	missing := SubjectRef{LocalSessionID: "missing"}
	cross := SubjectRef{LocalSessionID: "other"}
	for name, target := range map[string]struct {
		project ProjectEntryID
		parent  ParentRef
	}{
		"missing parent":    {p1, ParentRef{Subject: &missing}},
		"cross project":     {p1, ParentRef{Subject: &cross}},
		"reference project": {ref, ParentRef{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ReorderSiblings(ctx, target.project, target.parent, nil); err == nil {
				t.Fatal("invalid reorder target accepted")
			}
		})
	}
	before := rootOrder(t, s, p1)
	dup := []SubjectRef{{LocalSessionID: "parent"}, {LocalSessionID: "parent"}}
	if _, err = s.ReorderSiblings(ctx, p1, ParentRef{}, dup); err == nil {
		t.Fatal("duplicate reorder accepted")
	}
	if got := rootOrder(t, s, p1); !reflect.DeepEqual(got, before) {
		t.Fatalf("invalid reorder mutated order: %v", got)
	}
	parent, _, _ := s.Session(ctx, "parent")
	if _, err = s.RemoveSessionAtVersion(ctx, "parent", parent.Version); err != nil {
		t.Fatal(err)
	}
	deleted := SubjectRef{LocalSessionID: "parent"}
	if _, err = s.ReorderSiblings(ctx, p1, ParentRef{Subject: &deleted}, nil); err == nil {
		t.Fatal("deleted reorder parent accepted")
	}
}

func TestReorderSiblingScopesRollsBackAllScopes(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("one", "/one")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := cat[0].ID
	addSession(t, s, "a", "")
	addSession(t, s, "b", "")
	if _, err = s.PlaceLocalSession(ctx, "a", p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlaceLocalSession(ctx, "b", p); err != nil {
		t.Fatal(err)
	}
	before := rootOrder(t, s, p)
	_, err = s.ReorderSiblingScopes(ctx, []SiblingReorder{
		{Project: p, Order: []SubjectRef{{LocalSessionID: "b"}, {LocalSessionID: "a"}}},
		{Project: p, Order: []SubjectRef{{LocalSessionID: "missing"}}},
	})
	if err == nil {
		t.Fatal("invalid second scope accepted")
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, before) {
		t.Fatalf("first scope committed despite rollback: got %v want %v", got, before)
	}
}

func TestPromotionAndParentProjectTransitions(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{owned("one", "/one"), owned("two", "/two")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	p1, p2 := cat[0].ID, cat[1].ID
	addSession(t, s, "root", "")
	addSession(t, s, "parent", "")
	addSession(t, s, "child", "parent")
	addSession(t, s, "sibling", "parent")
	for _, id := range []SessionID{"root", "parent", "child", "sibling"} {
		if _, err = s.PlaceLocalSession(ctx, id, p1); err != nil {
			t.Fatal(err)
		}
	}
	idx := 1
	result, err := s.SetPromotion(ctx, "child", true, &idx)
	if err != nil || result.SessionVersion != 2 {
		t.Fatalf("promote result=%#v err=%v", result, err)
	}
	if got := rootOrder(t, s, p1); !reflect.DeepEqual(got, []string{"l:root", "l:child", "l:parent"}) {
		t.Fatalf("promoted roots=%v", got)
	}
	idx = 0
	result, err = s.SetPromotion(ctx, "child", false, &idx)
	if err != nil || result.SessionVersion != 3 {
		t.Fatalf("unpromote result=%#v err=%v", result, err)
	}
	if got := scopeOrder(t, s, p1, "c:l:parent"); !reflect.DeepEqual(got, []string{"l:child", "l:sibling"}) {
		t.Fatalf("unpromoted children=%v", got)
	}
	addSession(t, s, "destination", "")
	if _, err = s.PlaceLocalSession(ctx, "destination", p2); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlaceLocalSession(ctx, "parent", p2); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, p2); !reflect.DeepEqual(got, []string{"l:destination", "l:parent"}) {
		t.Fatalf("parent destination append=%v", got)
	}
	if got := rootOrder(t, s, p1); !reflect.DeepEqual(got, []string{"l:root", "l:child", "l:sibling"}) {
		t.Fatalf("children reroot append=%v", got)
	}
	assertKernelInvariants(t, s)
}

func TestPlacedParentRemovalPreservesGrandchildAndOrdering(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	addSession(t, s, "root", "")
	parent := addSession(t, s, "parent", "")
	addSession(t, s, "child", "parent")
	addSession(t, s, "grand", "child")
	for _, id := range []SessionID{"root", "parent", "child", "grand"} {
		if _, err := s.PlaceLocalSession(ctx, id, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RemoveSessionAtVersion(ctx, "parent", parent.Version); err != nil {
		t.Fatal(err)
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, []string{"l:root", "l:child"}) {
		t.Fatalf("removed-parent roots=%v", got)
	}
	if got := scopeOrder(t, s, p, "c:l:child"); !reflect.DeepEqual(got, []string{"l:grand"}) {
		t.Fatalf("grandchild scope=%v", got)
	}
	child, ok, err := s.Session(ctx, "child")
	if err != nil || !ok || child.Version != 2 || child.LaunchParentID != nil {
		t.Fatalf("child=%#v ok=%v err=%v", child, ok, err)
	}
	grand, ok, err := s.Session(ctx, "grand")
	if err != nil || !ok || grand.Version != 1 || grand.LaunchParentID == nil || *grand.LaunchParentID != "child" {
		t.Fatalf("grand=%#v ok=%v err=%v", grand, ok, err)
	}
	assertKernelInvariants(t, s)
}

func TestPlacementFaultRollsBackWithoutTemporaryScopes(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	for _, id := range []string{"a", "b", "c"} {
		addSession(t, s, id, "")
		if _, err := s.PlaceLocalSession(ctx, SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	before := rootOrder(t, s, p)
	s.beforePlacementFinalize = func() error { return errors.New("injected") }
	_, err := s.ReorderSiblings(ctx, p, ParentRef{}, []SubjectRef{{LocalSessionID: "c"}, {LocalSessionID: "b"}, {LocalSessionID: "a"}})
	s.beforePlacementFinalize = nil
	if err == nil {
		t.Fatal("injected failure succeeded")
	}
	if got := rootOrder(t, s, p); !reflect.DeepEqual(got, before) {
		t.Fatalf("rollback order=%v want=%v", got, before)
	}
	assertKernelInvariants(t, s)
}

func TestParentRemovalAndConcurrentObservedVersionWinner(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addSession(t, s, "p", "")
	addSession(t, s, "c", "p")
	if _, err := s.RemoveSessionAtVersion(ctx, "p", p.Version+1); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("stale remove=%v", err)
	}
	if _, err := s.RemoveSessionAtVersion(ctx, "p", p.Version); err != nil {
		t.Fatal(err)
	}
	child, ok, err := s.Session(ctx, "c")
	if err != nil || !ok || child.LaunchParentID != nil || child.Version != 2 {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	addSession(t, s, "race", "")
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			<-start
			_, e := s.ApplyCommonFacts(ctx, "race", 1, CommonFactsPatch{Subtitle: &value})
			results <- e
		}(fmt.Sprint(i))
	}
	close(start)
	wg.Wait()
	close(results)
	wins, stale := 0, 0
	for e := range results {
		if e == nil {
			wins++
		} else if errors.Is(e, ErrStaleVersion) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatalf("wins=%d stale=%d", wins, stale)
	}
}

func TestRandomizedPlacementOrderModel(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	p := addProject(t, s)
	model := []SubjectRef{}
	for i := 0; i < 6; i++ {
		id := SessionID(fmt.Sprintf("s%d", i))
		addSessionAt(t, s, string(id), "", UnixMillis(i))
		if _, err := s.PlaceLocalSession(ctx, id, p); err != nil {
			t.Fatal(err)
		}
		model = append(model, SubjectRef{LocalSessionID: id})
	}
	rng := rand.New(rand.NewSource(42))
	for step := 0; step < 150; step++ {
		switch rng.Intn(3) {
		case 0:
			i := rng.Intn(len(model))
			sub := model[i]
			var r MutationResult
			var err error
			if sub.LocalPeer != nil {
				r, err = s.UpsertLocalPeerPlacement(ctx, *sub.LocalPeer, p)
			} else {
				r, err = s.PlaceLocalSession(ctx, sub.LocalSessionID, p)
			}
			if err != nil || r.Changed {
				t.Fatalf("step %d repeat=%#v err=%v", step, r, err)
			}
		case 1:
			peer := LocalPeerSubject{PeerKey: "dev", SessionID: fmt.Sprintf("p%d", step)}
			if _, err := s.UpsertLocalPeerPlacement(ctx, peer, p); err != nil {
				t.Fatal(err)
			}
			model = append(model, SubjectRef{LocalPeer: &peer})
		case 2:
			rng.Shuffle(len(model), func(i, j int) { model[i], model[j] = model[j], model[i] })
			if _, err := s.ReorderSiblings(ctx, p, ParentRef{}, model); err != nil {
				t.Fatalf("step %d reorder: %v", step, err)
			}
		}
		want := make([]string, len(model))
		for i, x := range model {
			want[i] = subjectKey(x)
		}
		if got := rootOrder(t, s, p); !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d got=%v want=%v", step, got, want)
		}
		assertKernelInvariants(t, s)
	}
}

func assertKernelInvariants(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	all, err := placements(ctx, s.queries)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateLocalPeerParentGraph(all); err != nil {
		t.Fatalf("Local-peer graph invariant: %v", err)
	}
	groups := map[scopeKey][]int64{}
	for _, r := range all {
		if stringsHasPrefixTemp(r.scope) {
			t.Fatalf("temporary scope %q", r.scope)
		}
		groups[scopeKey{r.project, r.scope}] = append(groups[scopeKey{r.project, r.scope}], r.pos)
	}
	for k, pos := range groups {
		sort.Slice(pos, func(i, j int) bool { return pos[i] < pos[j] })
		for i, p := range pos {
			if p != int64(i) {
				t.Fatalf("non-dense %#v: %v", k, pos)
			}
		}
	}
	var bad int
	if err = s.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_placements p LEFT JOIN project_entries e ON e.id=p.project_entry_id WHERE e.id IS NULL OR e.entry_kind<>'owned' OR ((p.local_session_id IS NOT NULL) = (p.local_peer_key IS NOT NULL))`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("placement ownership/xor violations=%d", bad)
	}
	if err = s.database.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_match_rules r LEFT JOIN project_entries e ON e.id=r.project_entry_id WHERE e.id IS NULL OR e.entry_kind<>'owned' OR ((r.path IS NOT NULL) = (r.remote IS NOT NULL))`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("rule ownership/xor violations=%d", bad)
	}
	if err = s.database.QueryRowContext(ctx, `WITH RECURSIVE chain(start,id) AS (SELECT id,launch_parent_id FROM local_sessions WHERE launch_parent_id IS NOT NULL UNION SELECT chain.start,s.launch_parent_id FROM chain JOIN local_sessions s ON s.id=chain.id WHERE s.launch_parent_id IS NOT NULL) SELECT COUNT(*) FROM chain WHERE start=id`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("launch-parent cycles=%d", bad)
	}
	rows, err := s.database.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
func stringsHasPrefixTemp(s string) bool { return len(s) >= 2 && s[:2] == "~:" }
