package wire

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// roundTripFixture owns one real centralstore with an owned project and a
// mixed participant set: three local roots (one slugged, one with a child),
// and a placed Local-peer session. The golden round-trip tests drive the
// flatten → PATCH keys → DecomposeReorder → ReorderSiblings → flatten loop
// against the real durable ordering, pinning FD-1 in both directions and
// the L-2 key cases.
type roundTripFixture struct {
	t     *testing.T
	ctx   context.Context
	store *centralstore.Store
	conv  *Converter
	proj  centralstore.ProjectEntryID
}

func newRoundTripFixture(t *testing.T) *roundTripFixture {
	t.Helper()
	ctx := context.Background()
	s, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	cat, _, err := s.ReplaceProjectCatalog(ctx, []centralstore.ProjectEntrySpec{
		{Owned: &centralstore.OwnedProjectSpec{Slug: "proj", Rules: []centralstore.MatchRule{{Path: "/work"}}}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	proj := cat[0].ID

	insert := func(v centralstore.NewSession) {
		t.Helper()
		if _, _, err := s.InsertSession(ctx, v); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PlaceLocalSession(ctx, v.ID, proj); err != nil {
			t.Fatal(err)
		}
	}
	insert(centralstore.NewSession{ID: "sess-a", Adapter: "shell", CWD: "/work", CreatedAt: 1})
	insert(centralstore.NewSession{ID: "sess-b", Adapter: "claude", CWD: "/work", CreatedAt: 2, Slug: "fix-auth"})
	insert(centralstore.NewSession{ID: "sess-c", Adapter: "shell", CWD: "/work", CreatedAt: 3})
	parent := centralstore.SessionID("sess-a")
	insert(centralstore.NewSession{ID: "sess-child", Adapter: "shell", CWD: "/work", CreatedAt: 4, LaunchParentID: &parent})
	if _, err := s.UpsertLocalPeerPlacement(ctx, centralstore.LocalPeerSubject{PeerKey: "box", SessionID: "cont-1"}, proj); err != nil {
		t.Fatal(err)
	}

	return &roundTripFixture{
		t: t, ctx: ctx, store: s, proj: proj,
		conv: &Converter{IsLocalPeer: func(n string) bool { return n == "box" }},
	}
}

// payloads reads the current durable state through ReadSnapshot into the
// composer payload shapes (every Local-peer placement treated as
// connected).
func (f *roundTripFixture) payloads() (*central.SessionsPayload, *central.ProjectsPayload) {
	f.t.Helper()
	snap, err := f.store.ReadSnapshot(f.ctx, centralstore.SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
	if err != nil {
		f.t.Fatal(err)
	}
	sp := &central.SessionsPayload{}
	for _, v := range snap.Sessions {
		sp.Sessions = append(sp.Sessions, central.SessionRow{SessionView: v})
	}
	pp := &central.ProjectsPayload{Projects: snap.Projects}
	for _, p := range snap.LocalPeerPlacements {
		pp.LocalPeerPlacements = append(pp.LocalPeerPlacements, central.LocalPeerPlacementRow{LocalPeerPlacementView: p})
	}
	return sp, pp
}

func (f *roundTripFixture) peerRows() []Session {
	return []Session{{ID: "cont-1@box", Peer: "box", Adapter: "shell", Slug: "container-fix", Alive: true}}
}

// flatOrder renders the FD-1 wire order (sessions[] keys) for the project.
func (f *roundTripFixture) flatOrder() []string {
	f.t.Helper()
	sp, pp := f.payloads()
	world := f.conv.World(sp, pp, f.peerRows())
	for _, item := range world.Projects {
		if item.Slug == "proj" {
			return item.Sessions
		}
	}
	f.t.Fatal("project missing from world payload")
	return nil
}

func (f *roundTripFixture) apply(keys []string) {
	f.t.Helper()
	sp, pp := f.payloads()
	orders, ok := f.conv.DecomposeReorder("proj", keys, sp, pp)
	if !ok {
		f.t.Fatal("project not found")
	}
	for _, o := range orders {
		if _, err := f.store.ReorderSiblings(f.ctx, o.Project, o.Parent, o.Order); err != nil {
			f.t.Fatalf("ReorderSiblings(%+v): %v", o, err)
		}
	}
}

// TestGoldenRoundTripRootReorder: flatten → permuted flat key list using
// both accepted key shapes (plain ID, Local-peer namespaced key) plus an
// unknown key and a SLUG key — both silently dropped (production filters
// to known session IDs; fable M-2) → decompose → real ReorderSiblings →
// flatten shows the requested order with drops applied and the child block
// still inlined under its parent. A duplicate mention pins the deliberate
// first-mention-wins dedup (fable L-2).
func TestGoldenRoundTripRootReorder(t *testing.T) {
	f := newRoundTripFixture(t)

	initial := f.flatOrder()
	want := []string{"sess-a", "sess-child", "sess-b", "sess-c", "cont-1@box"}
	if !reflect.DeepEqual(initial, want) {
		t.Fatalf("registration order: %v", initial)
	}

	// Roots today: [sess-a, sess-b, sess-c, cont-1@box]. Request:
	// container first (namespaced key), sess-b's SLUG key (dropped — slug
	// keys are not production PATCH currency), sess-c (plain ID, twice —
	// dedup keeps the first mention), an unknown ghost key, then sess-a.
	// sess-b, never validly mentioned, keeps relative order at the tail.
	f.apply([]string{"cont-1@box", "fix-auth", "sess-c", "ghost-key", "sess-a", "sess-c"})

	got := f.flatOrder()
	want = []string{"cont-1@box", "sess-c", "sess-a", "sess-child", "sess-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after reorder: %v want %v", got, want)
	}
}

// TestGoldenRoundTripPartialRequest: keys the request omits keep their
// relative order at the tail of their scope (production ReorderSessions
// merge parity); the request may span scopes and each touched scope is
// reordered independently (sibling moves only).
func TestGoldenRoundTripPartialRequest(t *testing.T) {
	f := newRoundTripFixture(t)

	// Mention only sess-c (root scope). Everything else keeps relative
	// order behind it.
	f.apply([]string{"sess-c"})
	got := f.flatOrder()
	want := []string{"sess-c", "sess-a", "sess-child", "sess-b", "cont-1@box"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("partial reorder: %v want %v", got, want)
	}
}

// TestGoldenRoundTripChildScope: give sess-a a second child; reordering the
// child keys touches only the child scope (parent order intact), proving
// the decompose emits scoped orders rather than flat rewrites.
func TestGoldenRoundTripChildScope(t *testing.T) {
	f := newRoundTripFixture(t)
	parent := centralstore.SessionID("sess-a")
	if _, _, err := f.store.InsertSession(f.ctx, centralstore.NewSession{ID: "sess-child2", Adapter: "shell", CWD: "/work", CreatedAt: 5, LaunchParentID: &parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PlaceLocalSession(f.ctx, "sess-child2", f.proj); err != nil {
		t.Fatal(err)
	}

	f.apply([]string{"sess-child2", "sess-child"})
	got := f.flatOrder()
	want := []string{"sess-a", "sess-child2", "sess-child", "sess-b", "sess-c", "cont-1@box"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child reorder: %v want %v", got, want)
	}
}

// TestDecomposeUnknownProjectAndNoOps: unknown slug → not found; a request
// of only unknown keys decomposes to zero scope orders; a reference entry
// is not a reorder target.
func TestDecomposeUnknownProjectAndNoOps(t *testing.T) {
	conv := &Converter{}
	world := &central.ProjectsPayload{Projects: centralstore.ProjectCatalog{
		{ID: 1, Kind: centralstore.ProjectEntryOwned, Slug: "proj"},
		{ID: 2, Kind: centralstore.ProjectEntryReference, Slug: "remote", PeerKey: "tower"},
	}}
	if _, ok := conv.DecomposeReorder("missing", []string{"a"}, nil, world); ok {
		t.Fatal("unknown slug must not resolve")
	}
	if _, ok := conv.DecomposeReorder("remote", []string{"a"}, nil, world); ok {
		t.Fatal("reference entry must not be a reorder target")
	}
	orders, ok := conv.DecomposeReorder("proj", []string{"ghost-1", "ghost-2"}, nil, world)
	if !ok || len(orders) != 0 {
		t.Fatalf("unknown-keys request: ok=%v orders=%+v", ok, orders)
	}
	if _, ok := conv.DecomposeReorder("proj", []string{"a"}, nil, nil); ok {
		t.Fatal("nil world must not resolve")
	}
}

// TestGoldenRoundTripLocalPeerChildScope (tests review H-3 + M-1): two
// Local-peer sessions placed as children of a THIRD Local-peer session — a
// "c:p:<peer>:<session>" sibling scope — on a peer whose key and parent
// session ID contain the scope separator and escape characters. The
// decompose must parse the Local-peer parent scope (parentRefOf's c:p:
// branch) and the escaping must round-trip against the real store's
// scopes.
func TestGoldenRoundTripLocalPeerChildScope(t *testing.T) {
	ctx := context.Background()
	s, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cat, _, err := s.ReplaceProjectCatalog(ctx, []centralstore.ProjectEntrySpec{
		{Owned: &centralstore.OwnedProjectSpec{Slug: "proj", Rules: []centralstore.MatchRule{{Path: "/work"}}}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	proj := cat[0].ID

	const peer = "prod:box%dev" // ':' and '%' must survive scope escaping
	placeLP := func(sess, parent string) {
		t.Helper()
		if _, err := s.UpsertLocalPeerPlacement(ctx, centralstore.LocalPeerSubject{
			PeerKey: peer, SessionID: sess, ParentSessionID: parent,
		}, proj); err != nil {
			t.Fatal(err)
		}
	}
	placeLP("par:ent", "")
	placeLP("c%1", "par:ent")
	placeLP("c:2", "par:ent")

	conv := &Converter{IsLocalPeer: func(n string) bool { return n == peer }}
	payloads := func() (*central.SessionsPayload, *central.ProjectsPayload) {
		snap, err := s.ReadSnapshot(ctx, centralstore.SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
		if err != nil {
			t.Fatal(err)
		}
		pp := &central.ProjectsPayload{Projects: snap.Projects}
		for _, p := range snap.LocalPeerPlacements {
			pp.LocalPeerPlacements = append(pp.LocalPeerPlacements, central.LocalPeerPlacementRow{LocalPeerPlacementView: p})
		}
		return &central.SessionsPayload{}, pp
	}
	flat := func() []string {
		sp, pp := payloads()
		world := conv.World(sp, pp, nil)
		return world.Projects[0].Sessions
	}

	want := []string{"par:ent@" + peer, "c%1@" + peer, "c:2@" + peer}
	if got := flat(); !reflect.DeepEqual(got, want) {
		t.Fatalf("initial flatten: %v", got)
	}

	// Reorder the two children within their Local-peer parent scope.
	sp, pp := payloads()
	orders, ok := conv.DecomposeReorder("proj", []string{"c:2@" + peer, "c%1@" + peer}, sp, pp)
	if !ok || len(orders) != 1 {
		t.Fatalf("decompose: ok=%v orders=%+v", ok, orders)
	}
	parent := orders[0].Parent.Subject
	if parent == nil || parent.LocalPeer == nil || string(parent.LocalPeer.PeerKey) != peer || parent.LocalPeer.SessionID != "par:ent" {
		t.Fatalf("Local-peer parent ref not parsed: %+v", orders[0].Parent)
	}
	if _, err := s.ReorderSiblings(ctx, orders[0].Project, orders[0].Parent, orders[0].Order); err != nil {
		t.Fatalf("ReorderSiblings: %v", err)
	}
	want = []string{"par:ent@" + peer, "c:2@" + peer, "c%1@" + peer}
	if got := flat(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after child reorder: %v", got)
	}
}

// TestScopeEscapingRoundTrip (tests review M-1): escapeScope/unescapeScope
// are exact inverses for adversarial inputs, and escaped output can never
// contain a spurious separator.
func TestScopeEscapingRoundTrip(t *testing.T) {
	cases := []string{"", "plain", "a:b", "a%b", "%3A", "%25", "a%3Ab", "::%%::", "prod:box%dev", "%", ":"}
	for _, in := range cases {
		esc := escapeScope(in)
		if in != "" && strings.Contains(esc, ":") {
			t.Errorf("escapeScope(%q)=%q leaks a separator", in, esc)
		}
		if got := unescapeScope(esc); got != in {
			t.Errorf("round trip %q -> %q -> %q", in, esc, got)
		}
	}
}
