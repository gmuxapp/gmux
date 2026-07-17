package equivalence

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// fdExcluded reports whether a JSON path is one of the accepted FD-* diffs
// excluded from the structural comparison (design §8): project_index
// (FD-1), the timestamp fields (FD-4 — asserted separately by
// parse-and-compare), and the world health session counts (FD-6).
func fdExcluded(path string) bool {
	switch {
	case strings.HasSuffix(path, ".project_index"):
		return true
	case strings.HasSuffix(path, ".created_at"), strings.HasSuffix(path, ".started_at"),
		strings.HasSuffix(path, ".exited_at"), strings.HasSuffix(path, ".last_activity_at"):
		return true
	case strings.HasPrefix(path, "world.health.sessions"):
		return true
	}
	return false
}

// diffJSON structurally compares two unmarshaled JSON values and returns
// every differing path not excluded.
func diffJSON(path string, a, b any, excluded func(string) bool, out *[]string) {
	if excluded(path) {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: object vs %T", path, b))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := path + "." + k
			x, inA := av[k]
			y, inB := bv[k]
			if excluded(p) {
				continue
			}
			if !inA || !inB {
				*out = append(*out, fmt.Sprintf("%s: present-in-production=%v present-in-new=%v", p, inA, inB))
				continue
			}
			diffJSON(p, x, y, excluded, out)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: array vs %T", path, b))
			return
		}
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s: len %d vs %d", path, len(av), len(bv)))
			return
		}
		for i := range av {
			diffJSON(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], excluded, out)
		}
	default:
		if fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b) {
			*out = append(*out, fmt.Sprintf("%s: %v vs %v", path, a, b))
		}
	}
}

func toAny(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func render(t *testing.T, w *World) (prodSessions, prodWorld any, frames wire.Frames) {
	t.Helper()
	ps, pw := RenderProduction(w)
	frames, _ = RenderCentral(t, w)
	if frames.Sessions == nil || frames.World == nil {
		t.Fatalf("new stack composed no matched pair: %+v", frames)
	}
	return toAny(t, ps), toAny(t, pw), frames
}

// indexSessions keys the unmarshaled sessions array by id.
func indexSessions(t *testing.T, payload any) map[string]map[string]any {
	t.Helper()
	obj, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload shape: %T", payload)
	}
	list, ok := obj["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions shape: %T", obj["sessions"])
	}
	out := map[string]map[string]any{}
	for _, item := range list {
		m := item.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

// TestSemanticEquivalenceDefaultWorld is the gate: the same seeded state
// rendered through the production composer and through the new stack
// produces structurally identical JSON outside the FD-* exclusions —
// both the snapshot.sessions and snapshot.world payloads.
func TestSemanticEquivalenceDefaultWorld(t *testing.T) {
	w := DefaultWorld()
	prodSessions, prodWorld, frames := render(t, w)

	// FD-2: the dismissed session exists only in the new model and is
	// filtered before the wire; drop it from neither (production never had
	// it, the new payload must not have it) — asserted explicitly below.
	var diffs []string
	diffJSON("sessions", prodSessions, toAny(t, frames.Sessions), fdExcluded, &diffs)
	diffJSON("world", prodWorld, toAny(t, frames.World), fdExcluded, &diffs)
	if len(diffs) > 0 {
		t.Fatalf("semantic divergence (%d paths):\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
}

// TestFD1FlatIndicesMatchWhenHierarchyGroupingAgrees: with the fixture's
// sidebar order seeded in FD-1 form (child directly after its parent),
// the flattened project_index values match production exactly — the FD-1
// divergence is confined to states where the legacy flat array disagrees
// with hierarchy grouping.
func TestFD1FlatIndicesMatchWhenHierarchyGroupingAgrees(t *testing.T) {
	w := DefaultWorld()
	prodSessions, _, frames := render(t, w)
	prod := indexSessions(t, prodSessions)
	got := indexSessions(t, toAny(t, frames.Sessions))
	for id, pm := range prod {
		gm, ok := got[id]
		if !ok {
			t.Fatalf("session %s missing from new payload", id)
		}
		pi, gi := pm["project_index"], gm["project_index"]
		if fmt.Sprintf("%v", pi) != fmt.Sprintf("%v", gi) {
			t.Errorf("FD-1 %s: project_index %v vs %v", id, pi, gi)
		}
		if fmt.Sprintf("%v", pm["project_slug"]) != fmt.Sprintf("%v", gm["project_slug"]) {
			t.Errorf("%s: project_slug %v vs %v", id, pm["project_slug"], gm["project_slug"])
		}
	}
}

// TestFD2DismissedSessionsVanishFromTheWire: the dismissed row is absent
// from the new sessions payload (hidden-not-forgotten never reaches the
// wire), exactly like production removal.
func TestFD2DismissedSessionsVanishFromTheWire(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	for _, s := range frames.Sessions.Sessions {
		if s.ID == "sess-hidden" {
			t.Fatal("dismissed session leaked onto the wire")
		}
	}
	found := 0
	for _, f := range w.Sessions {
		if !f.Dismissed {
			found++
		}
	}
	if len(frames.Sessions.Sessions) != found {
		t.Fatalf("session count %d want %d", len(frames.Sessions.Sessions), found)
	}
}

// TestFD3DurableActivityStampSurvives: last_activity_at on the wire equals
// the seeded durable stamp (no activity-seed heuristic in the new path).
func TestFD3DurableActivityStampSurvives(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	for _, s := range frames.Sessions.Sessions {
		if s.ID != "sess-dead" {
			continue
		}
		want := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC) // base-30m, truncated
		got, err := time.Parse(time.RFC3339, s.LastActivityAt)
		if err != nil || !got.Equal(want) {
			t.Fatalf("last_activity_at=%q err=%v want %v", s.LastActivityAt, err, want)
		}
		return
	}
	t.Fatal("sess-dead missing")
}

// TestFD4TimestampsParseAndCompare: every emitted timestamp field parses as
// RFC3339 and equals the seed truncated to seconds (the ms→RFC3339
// conversion is semantically exact even though the code path is new).
func TestFD4TimestampsParseAndCompare(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	seeds := map[string]FixtureSession{}
	for _, f := range w.Sessions {
		seeds[f.ID] = f
	}
	check := func(id, field, got string, seed time.Time) {
		t.Helper()
		if seed.IsZero() {
			if got != "" {
				t.Errorf("%s.%s: %q for zero seed", id, field, got)
			}
			return
		}
		parsed, err := time.Parse(time.RFC3339, got)
		if err != nil {
			t.Errorf("%s.%s: %v", id, field, err)
			return
		}
		if want := seed.UTC().Truncate(time.Second); !parsed.Equal(want) {
			t.Errorf("%s.%s: %v want %v", id, field, parsed, want)
		}
	}
	for _, s := range frames.Sessions.Sessions {
		f, ok := seeds[s.ID]
		if !ok {
			t.Fatalf("unseeded session %s", s.ID)
		}
		check(s.ID, "created_at", s.CreatedAt, f.Created)
		check(s.ID, "started_at", s.StartedAt, f.Started)
		check(s.ID, "exited_at", s.ExitedAt, f.Exited)
		check(s.ID, "last_activity_at", s.LastActivityAt, f.LastActivity)
	}
}

// TestFD5RebuiltProjectSessions: world items rebuild sessions[] from
// placements in FD-1 order with namespaced Local-peer keys; reference
// items carry the peer, their durable node_id (ADR 0017 anchor, persisted
// per review fable H-1 — no FD-5 field is dropped), and no sessions.
func TestFD5RebuiltProjectSessions(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	items := frames.World.Projects
	if len(items) != 2 {
		t.Fatalf("items: %+v", items)
	}
	if got, want := fmt.Sprintf("%v", items[0].Sessions), fmt.Sprintf("%v", w.Projects[0].Sessions); got != want {
		t.Fatalf("FD-5 sessions[]: %s want %s", got, want)
	}
	if items[1].Peer != "tower" || items[1].NodeID != "node-tower-1" || items[1].Sessions != nil {
		t.Fatalf("reference item: %+v", items[1])
	}
}

// TestStatusNullForNeverReportedSessions (fable M-1 decision — NOT an
// accepted diff): a session whose runner never reported a status emits
// "status": null on the wire, exactly like production; reported rows carry
// the concrete object. gmux wait's died/idle verdict rides on this.
func TestStatusNullForNeverReportedSessions(t *testing.T) {
	w := DefaultWorld()
	prodSessions, _, frames := render(t, w)
	prod := indexSessions(t, prodSessions)
	got := indexSessions(t, toAny(t, frames.Sessions))
	if prod["sess-nostatus"]["status"] != nil {
		t.Fatalf("production fixture must carry null status: %v", prod["sess-nostatus"]["status"])
	}
	if got["sess-nostatus"]["status"] != nil {
		t.Fatalf("never-reported session must emit null status: %v", got["sess-nostatus"]["status"])
	}
	if got["sess-live"]["status"] == nil {
		t.Fatal("reported session lost its status object")
	}
}

// TestUnplacedLocalPeerStampsCleared (tests review M-3): an unplaced
// Local-peer projection carrying stale residue stamps is cleared by the
// wire, matching the parent's Reconcile disclaim in production. (The
// general diff also covers it; this pins the specific rows.)
func TestUnplacedLocalPeerStampsCleared(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	for _, s := range frames.Sessions.Sessions {
		switch s.ID {
		case "cont-2@box":
			if s.ProjectSlug != "" || s.ProjectIndex != 0 {
				t.Fatalf("unplaced Local-peer stamps not cleared: %+v", s)
			}
		case "cont-1@box":
			if s.ProjectSlug != "app" {
				t.Fatalf("placed Local-peer residue not overwritten: %+v", s)
			}
		}
	}
}

// TestFD1PromotedChildIsARoot (design §8 "incl. promoted children"): the
// promoted child sits at the root scope tail, never inlined under its
// launch parent.
func TestFD1PromotedChildIsARoot(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	for _, item := range frames.World.Projects {
		if item.Slug != "app" {
			continue
		}
		if got, want := fmt.Sprintf("%v", item.Sessions), fmt.Sprintf("%v", w.Projects[0].Sessions); got != want {
			t.Fatalf("promoted-child flatten: %s want %s", got, want)
		}
		return
	}
	t.Fatal("app project missing")
}

// TestFilterOwnedEquivalence (tests review M-2): the ?as=peer narrowing
// driven through the full new pipeline matches production's owned-only
// ComposeSessions filter, under the same FD exclusions.
func TestFilterOwnedEquivalence(t *testing.T) {
	w := DefaultWorld()
	prodOwned := toAny(t, RenderProductionOwnedSessions(w))
	frames, _ := RenderCentral(t, w)
	gotOwned := toAny(t, frames.Sessions.FilterOwned(w.isLocalPeer))
	var diffs []string
	diffJSON("owned", prodOwned, gotOwned, fdExcluded, &diffs)
	if len(diffs) > 0 {
		t.Fatalf("?as=peer divergence:\n%s", strings.Join(diffs, "\n"))
	}
	// The network-peer mirror must actually be gone (guard against a
	// filter that keeps everything on both sides).
	for _, s := range frames.Sessions.FilterOwned(w.isLocalPeer).Sessions {
		if s.ID == "remote-1@tower" {
			t.Fatal("network-peer row leaked through FilterOwned")
		}
	}
}

// TestDiffJSONSelfTest (tests review M-5): the gate's own comparison
// reports scalar, type, length, and key-presence differences — the whole
// suite is a no-op if this ever regresses.
func TestDiffJSONSelfTest(t *testing.T) {
	cases := []struct {
		name string
		a, b any
	}{
		{"string", map[string]any{"x": "a"}, map[string]any{"x": "b"}},
		{"number", map[string]any{"x": 1.0}, map[string]any{"x": 2.0}},
		{"bool", map[string]any{"x": true}, map[string]any{"x": false}},
		{"type", map[string]any{"x": "1"}, map[string]any{"x": []any{}}},
		{"missing key", map[string]any{"x": "a"}, map[string]any{}},
		{"array len", map[string]any{"x": []any{"a"}}, map[string]any{"x": []any{}}},
		{"nested", map[string]any{"x": map[string]any{"y": 1.0}}, map[string]any{"x": map[string]any{"y": 2.0}}},
		{"null vs object", map[string]any{"x": nil}, map[string]any{"x": map[string]any{}}},
	}
	for _, tc := range cases {
		var diffs []string
		diffJSON("t", tc.a, tc.b, func(string) bool { return false }, &diffs)
		if len(diffs) == 0 {
			t.Errorf("%s: diffJSON reported no difference", tc.name)
		}
	}
	// Excluded paths stay silent; equal values stay silent.
	var diffs []string
	diffJSON("t", map[string]any{"project_index": 1.0}, map[string]any{"project_index": 2.0},
		fdExcluded, &diffs)
	diffJSON("t", map[string]any{"x": "a"}, map[string]any{"x": "a"}, fdExcluded, &diffs)
	if len(diffs) != 0 {
		t.Fatalf("unexpected diffs: %v", diffs)
	}
}

// TestFD6HealthCountsDeriveFromRegistryAndRows: local_alive counts
// registry-live local rows, remote_alive live peer projections, dead the
// rest — and the dismissed row counts nowhere.
func TestFD6HealthCountsDeriveFromRegistryAndRows(t *testing.T) {
	w := DefaultWorld()
	frames, _ := RenderCentral(t, w)
	got := frames.World.Health.Sessions
	// sess-live, sess-child local alive; cont-1@box + remote-1@tower peer
	// alive; sess-dead + sess-bare dead; sess-hidden dismissed → uncounted.
	// Deliberately asymmetric (tests review H-1: symmetric counts masked
	// an alive/dead classification swap): local alive = sess-live,
	// sess-child, sess-nostatus, sess-promoted; remote alive = cont-1,
	// cont-2, remote-1; dead = sess-dead, sess-bare; dismissed uncounted.
	want := central.SessionCounts{LocalAlive: 4, RemoteAlive: 3, Dead: 2}
	if got != want {
		t.Fatalf("FD-6 counts: %+v want %+v", got, want)
	}
}

// TestTitleDerivationMatchesProductionResolveTitle pins the wire title
// derivation against the REAL production resolveTitle (via store.Upsert)
// for the design's edge-case table, including the empty-title cases.
func TestTitleDerivationMatchesProductionResolveTitle(t *testing.T) {
	cases := []FixtureSession{
		{ID: "t1", Adapter: "codex", AdapterTitle: "A", ShellTitle: "S", Command: []string{"codex", "x"}},
		{ID: "t2", Adapter: "codex", ShellTitle: "S", Command: []string{"codex", "x"}},
		{ID: "t3", Adapter: "codex", Command: []string{"codex", "resume", "y"}},
		{ID: "t4", Adapter: "codex"},
		{ID: "t5", Adapter: "claude", Command: []string{"claude"}},
		{ID: "t6", Adapter: "shell"},
	}
	for _, f := range cases {
		st := store.New()
		st.SetCommandTitlers(Titlers())
		st.Upsert(store.Session{ID: f.ID, Adapter: f.Adapter, Command: f.Command, AdapterTitle: f.AdapterTitle, ShellTitle: f.ShellTitle, Alive: true})
		prod, ok := st.Get(f.ID)
		if !ok {
			t.Fatalf("%s: production row missing", f.ID)
		}

		w := &World{Sessions: []FixtureSession{{ID: f.ID, Adapter: f.Adapter, Command: f.Command, AdapterTitle: f.AdapterTitle, ShellTitle: f.ShellTitle, Alive: true, Pid: 1, SocketPath: "/s", Created: time.Unix(1, 0)}}}
		frames, _ := RenderCentral(t, w)
		if len(frames.Sessions.Sessions) != 1 {
			t.Fatalf("%s: %+v", f.ID, frames.Sessions)
		}
		if got := frames.Sessions.Sessions[0].Title; got != prod.Title {
			t.Errorf("%s: title %q, production resolveTitle says %q", f.ID, got, prod.Title)
		}
	}
}

// TestCacheCurrentServesTheSamePayload: GET /v1/sessions (Cache.Current)
// and the pushed frames share one conversion path — identical output.
func TestCacheCurrentServesTheSamePayload(t *testing.T) {
	w := DefaultWorld()
	frames, cache := RenderCentral(t, w)
	cur := cache.Current()
	a, _ := json.Marshal(frames.Sessions)
	b, _ := json.Marshal(cur.Sessions)
	if string(a) != string(b) {
		t.Fatalf("one-shot read diverged from pushed snapshot:\n%s\n%s", a, b)
	}
}
