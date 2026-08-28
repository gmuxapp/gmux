package peering

import (
	"sync/atomic"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// These tests pin the three semantics projectionsEqual is built around
// (see its docstring). Each one is a mutation guard: a plausible
// "simplification" of the comparator — positional compare, pointer-identity
// compare, plain reflect.DeepEqual on the collections — reopens the
// reciprocal snapshot loop or hides a real update, and nothing else in the
// suite notices.

func intPtr(v int) *int { return &v }

// Ordering: ADR 0001 gives deterministic wire ordering today, but an
// order-sensitive gate would turn any future reordering into a permanent loop.
func TestProjectionsEqualIsOrderInsensitive(t *testing.T) {
	a := []SessionProjection{
		{ID: "s1", Adapter: "claude", Alive: true},
		{ID: "s2", Adapter: "codex", Alive: true},
		{ID: "s3", Adapter: "claude", Alive: false},
	}
	b := []SessionProjection{a[2], a[0], a[1]}
	if !projectionsEqual(a, b) {
		t.Fatalf("reordered identical set reported as changed (order-sensitive gate = permanent loop)")
	}
	// A reorder that also carries a real change must still be caught.
	b[0].Alive = true
	if projectionsEqual(a, b) {
		t.Fatalf("reorder + content change reported as equal")
	}
}

// Duplicate IDs must degrade to "changed" from whichever side they arrive on:
// the previous projection (index build) or the incoming one. Matching two
// incoming rows against one cached row silently drops the cached row's peer.
func TestProjectionsEqualRejectsDuplicateIDsSymmetrically(t *testing.T) {
	distinct := []SessionProjection{{ID: "s1", Alive: true}, {ID: "s2", Alive: true}}
	dupes := []SessionProjection{{ID: "s1", Alive: true}, {ID: "s1", Alive: true}}
	if projectionsEqual(distinct, dupes) {
		t.Errorf("duplicate ID in the incoming snapshot reported equal: the s2 row would be dropped with no dirty signal")
	}
	if projectionsEqual(dupes, distinct) {
		t.Errorf("duplicate ID in the cached snapshot reported equal")
	}
	if projectionsEqual(dupes, dupes) {
		t.Errorf("duplicate IDs on both sides must still degrade to \"changed\"")
	}
}

// Regression at the manager level: the asymmetric case used to update the
// cache without firing PeerSessionsDirty, so the composer kept serving the
// previous set until an unrelated event.
func TestReplacePeerSessionsFiresOnDuplicateIDSnapshot(t *testing.T) {
	var dirty atomic.Int64
	m := NewProjectionManager(nil, "self", nil, EventHooks{PeerSessionsDirty: func() { dirty.Add(1) }})
	m.ReplacePeerSessions("p", []SessionProjection{{ID: "s1@p", Peer: "p", Alive: true}, {ID: "s2@p", Peer: "p", Alive: true}})
	if dirty.Load() != 1 {
		t.Fatalf("first snapshot: dirty=%d, want 1", dirty.Load())
	}
	m.ReplacePeerSessions("p", []SessionProjection{{ID: "s1@p", Peer: "p", Alive: true}, {ID: "s1@p", Peer: "p", Alive: true}})
	if dirty.Load() != 2 {
		t.Fatalf("duplicate-ID snapshot: dirty=%d, want 2 (cache changed, so downstream must be told)", dirty.Load())
	}
}

// Pointer fields are compared by value: rows are freshly cloned on every
// delivery, so pointer identity never matches and an identity compare would
// make every re-delivery look changed (loop) — while for the exit path it is
// the value change that must be seen.
func TestProjectionEqualComparesPointerFieldsByValue(t *testing.T) {
	exited := func(code int) SessionProjection {
		return SessionProjection{
			ID: "s1", Adapter: "claude", Alive: false, ExitedAt: "t2",
			ExitCode: intPtr(code), Status: &SessionStatus{Active: false},
		}
	}
	if !projectionEqual(exited(137), exited(137)) {
		t.Errorf("equal ExitCode values with distinct pointers reported as changed")
	}
	if projectionEqual(exited(137), exited(0)) {
		t.Errorf("differing ExitCode values reported as equal: a real exit would be suppressed")
	}
	nilCode := exited(0)
	nilCode.ExitCode = nil
	if projectionEqual(nilCode, exited(0)) {
		t.Errorf("nil vs &0 ExitCode reported as equal: the exit transition would be hidden")
	}
	working := exited(1)
	working.Status = &SessionStatus{Active: true}
	if projectionEqual(exited(1), working) {
		t.Errorf("differing Status values reported as equal")
	}
	noStatus := exited(1)
	noStatus.Status = nil
	if projectionEqual(exited(1), noStatus) {
		t.Errorf("nil vs non-nil Status reported as equal")
	}
}

// End-to-end version of the above through the suppression gate: a session
// that exits must always fire, and its re-delivery must not.
func TestReplacePeerSessionsHandlesExitTransition(t *testing.T) {
	var dirty atomic.Int64
	m := NewProjectionManager(nil, "self", nil, EventHooks{PeerSessionsDirty: func() { dirty.Add(1) }})
	alive := []SessionProjection{{ID: "s1@p", Peer: "p", Adapter: "claude", Alive: true}}
	m.ReplacePeerSessions("p", alive)
	exited := []SessionProjection{{ID: "s1@p", Peer: "p", Adapter: "claude", Alive: false, ExitedAt: "t2", ExitCode: intPtr(137)}}
	m.ReplacePeerSessions("p", exited)
	if dirty.Load() != 2 {
		t.Fatalf("exit transition: dirty=%d, want 2", dirty.Load())
	}
	again := []SessionProjection{{ID: "s1@p", Peer: "p", Adapter: "claude", Alive: false, ExitedAt: "t2", ExitCode: intPtr(137)}}
	m.ReplacePeerSessions("p", again)
	if dirty.Load() != 2 {
		t.Fatalf("re-delivered exited session: dirty=%d, want 2 (no-op must be suppressed)", dirty.Load())
	}
	other := []SessionProjection{{ID: "s1@p", Peer: "p", Adapter: "claude", Alive: false, ExitedAt: "t2", ExitCode: intPtr(1)}}
	m.ReplacePeerSessions("p", other)
	if dirty.Load() != 3 {
		t.Fatalf("different exit code: dirty=%d, want 3", dirty.Load())
	}
}

// nil vs empty collections: JSON encoders disagree on null vs []/{} and the
// distinction carries no meaning here. A refactor to plain reflect.DeepEqual
// on Command/Remotes would reintroduce the split and with it a live loop.
func TestProjectionEqualTreatsNilAndEmptyCollectionsAsEqual(t *testing.T) {
	base := SessionProjection{ID: "s1", Adapter: "claude", Alive: true}
	empty := base
	empty.Command = []string{}
	empty.Remotes = map[string]string{}
	if !projectionEqual(base, empty) {
		t.Errorf("nil vs empty Command/Remotes reported as changed")
	}
	if !projectionsEqual([]SessionProjection{base}, []SessionProjection{empty}) {
		t.Errorf("nil vs empty collections not tolerated through projectionsEqual")
	}
	populated := base
	populated.Command = []string{"claude"}
	if projectionEqual(base, populated) {
		t.Errorf("nil vs non-empty Command reported as equal")
	}
	remotes := base
	remotes.Remotes = map[string]string{"origin": "git@x"}
	if projectionEqual(base, remotes) {
		t.Errorf("nil vs non-empty Remotes reported as equal")
	}
	// Same length, different content, for both collection helpers.
	other := populated
	other.Command = []string{"codex"}
	if projectionEqual(populated, other) {
		t.Errorf("same-length differing Command reported as equal")
	}
	remotes2 := remotes
	remotes2.Remotes = map[string]string{"origin": "git@y"}
	if projectionEqual(remotes, remotes2) {
		t.Errorf("same-length differing Remotes reported as equal")
	}
	// Empty projections on both sides (nil vs empty slice) are equal.
	if !projectionsEqual(nil, []SessionProjection{}) {
		t.Errorf("nil vs empty projection slice reported as changed")
	}
}

// The whole-slice edges the gate relies on: first delivery always fires
// (including an empty one), and N->0 / 0->N are real changes.
func TestReplacePeerSessionsEmptySnapshotSemantics(t *testing.T) {
	var dirty atomic.Int64
	m := NewProjectionManager([]config.PeerConfig{}, "self", nil, EventHooks{PeerSessionsDirty: func() { dirty.Add(1) }})
	m.ReplacePeerSessions("p", nil)
	if dirty.Load() != 1 {
		t.Fatalf("first (empty) snapshot: dirty=%d, want 1", dirty.Load())
	}
	m.ReplacePeerSessions("p", []SessionProjection{})
	if dirty.Load() != 1 {
		t.Fatalf("empty re-delivery: dirty=%d, want 1", dirty.Load())
	}
	m.ReplacePeerSessions("p", []SessionProjection{{ID: "s1@p", Peer: "p", Alive: true}})
	if dirty.Load() != 2 {
		t.Fatalf("0->1: dirty=%d, want 2", dirty.Load())
	}
	m.ReplacePeerSessions("p", nil)
	if dirty.Load() != 3 {
		t.Fatalf("1->0: dirty=%d, want 3", dirty.Load())
	}
}

// worldRelevantSessionChange is the second gate on the same delivery: it
// decides whether a real projection change also moves a world-derived field
// (peers[].session_count, PeerWorld.LocalPeerSessions). It must be *narrow*
// (metadata churn must not put world frames back on the wire) but never
// unsound. The two tests below pin both directions together, so neither
// "return true" nor a re-narrowing that drops the distinct-ID check survives.

func alive(id string) SessionProjection { return SessionProjection{ID: id, Peer: "p", Alive: true} }
func dead(id string) SessionProjection  { return SessionProjection{ID: id, Peer: "p", Alive: false} }
func titled(id, t string) SessionProjection {
	s := alive(id)
	s.Title = t
	return s
}

// Regression (D-1): a repeated ID carries the same row count, the same alive
// count and no unknown ID, yet the ID *set* shrank — LocalPeerSessions is
// keyed by ID, so b's placement row silently leaves the world. Equal row
// counts are not a proxy for equal ID-set size.
func TestWorldRelevantSessionChangeDetectsDuplicateIDCollapse(t *testing.T) {
	prev := []SessionProjection{alive("a@p"), alive("b@p")}
	next := []SessionProjection{alive("a@p"), alive("a@p")}
	if !worldRelevantSessionChange(prev, next) {
		t.Errorf("ID-set collapse via duplicate IDs reported as world-irrelevant")
	}
	// The reverse delivery (duplicates -> distinct set) restores b and is
	// equally world-relevant; a one-sided distinct count misses it.
	if !worldRelevantSessionChange(next, prev) {
		t.Errorf("ID-set expansion out of duplicate IDs reported as world-irrelevant")
	}
	// And it must survive the trip through the manager: the world hook has
	// to actually fire for such a delivery.
	var world atomic.Int64
	m := NewProjectionManager([]config.PeerConfig{}, "self", nil, EventHooks{PeerWorldDirty: func() { world.Add(1) }})
	m.ReplacePeerSessions("p", prev)
	before := world.Load()
	m.ReplacePeerSessions("p", next)
	if world.Load() == before {
		t.Errorf("duplicate-ID collapse did not request a world recompose")
	}
}

// Narrowness control: everything the predicate deliberately excludes must
// stay excluded, and everything it covers must stay covered. Without this a
// blanket `return true` would "fix" the regression above while restoring the
// world-frame chatter the gate exists to prevent.
func TestWorldRelevantSessionChangeStaysNarrow(t *testing.T) {
	cases := []struct {
		name       string
		prev, next []SessionProjection
		want       bool
	}{
		{"identical", []SessionProjection{alive("a@p")}, []SessionProjection{alive("a@p")}, false},
		{"title only", []SessionProjection{titled("a@p", "x")}, []SessionProjection{titled("a@p", "y")}, false},
		{"reordered", []SessionProjection{alive("a@p"), dead("b@p")}, []SessionProjection{dead("b@p"), alive("a@p")}, false},
		{"alive/dead swap at constant count", []SessionProjection{alive("a@p"), dead("b@p")}, []SessionProjection{dead("a@p"), alive("b@p")}, false},
		{"same duplicates on both sides", []SessionProjection{alive("a@p"), alive("a@p")}, []SessionProjection{alive("a@p"), alive("a@p")}, false},
		{"row added", []SessionProjection{alive("a@p")}, []SessionProjection{alive("a@p"), alive("b@p")}, true},
		{"row removed", []SessionProjection{alive("a@p"), alive("b@p")}, []SessionProjection{alive("a@p")}, true},
		{"ID substituted", []SessionProjection{alive("a@p")}, []SessionProjection{alive("c@p")}, true},
		{"alive count delta", []SessionProjection{alive("a@p")}, []SessionProjection{dead("a@p")}, true},
		{"duplicate collapse", []SessionProjection{alive("a@p"), alive("b@p")}, []SessionProjection{alive("a@p"), alive("a@p")}, true},
	}
	for _, tc := range cases {
		if got := worldRelevantSessionChange(tc.prev, tc.next); got != tc.want {
			t.Errorf("%s: worldRelevantSessionChange = %v, want %v", tc.name, got, tc.want)
		}
	}
}
