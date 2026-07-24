package centralstore

import (
	"context"
	"testing"
)

// TestStatusReportedFactLifecycle pins the runner-authoritative,
// generation-scoped status-reported bit (review fable M-1 / FD-7 decision
// + delta review Δ-1): unset until a working/error fact is observed, set
// by any observation carrying one (including an explicit false), never
// set by acknowledgement, RESET by a replacement generation alongside the
// other generation-scoped facts (re-set when the new generation's own
// facts carry status), preserved by the sweep (death of the same
// generation).
func TestStatusReportedFactLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)

	// Registration without status facts: not reported.
	reg := registration("sess-s", "shell", "/tmp", true, 10)
	got, _, err := s.RegisterRunner(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusReported {
		t.Fatal("fresh registration without status facts must not be reported")
	}

	// Acknowledgement path never sets it (daemon-side, not runner).
	if _, err = s.AcknowledgeDeadSession(ctx, "sess-s", got.Version); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Session(ctx, "sess-s"); v.StatusReported {
		t.Fatal("acknowledgement must not set status-reported")
	}

	// An observation carrying an explicit working=false IS a report.
	v, _, _ := s.Session(ctx, "sess-s")
	if _, err = s.ApplyRunnerObservation(ctx, RunnerObservation{ID: "sess-s", ObservedVersion: v.Version, ObservedAt: 20, Facts: RunnerFacts{Working: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	v, _, _ = s.Session(ctx, "sess-s")
	if !v.StatusReported || v.Working {
		t.Fatalf("explicit false status must set reported: %+v", v)
	}

	// A replacement generation resets the bit with the other
	// generation-scoped facts (Δ-1): a resumed generation that never
	// reports must render "status": null (wait verdict "died"), not
	// inherit the dead generation's report.
	replacement := registration("sess-s", "shell", "/tmp", true, 30)
	replacement.NewGeneration = true
	got, _, err = s.RegisterRunner(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusReported || got.Working {
		t.Fatalf("replacement generation must reset the reported bit: %+v", got)
	}

	// ...and the new generation's own status facts re-set it (Δ-3: a
	// reported all-false status is a valid re-entered state).
	v, _, _ = s.Session(ctx, "sess-s")
	if _, err = s.ApplyRunnerObservation(ctx, RunnerObservation{ID: "sess-s", ObservedVersion: v.Version, ObservedAt: 35, Facts: RunnerFacts{Working: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ = s.Session(ctx, "sess-s"); !v.StatusReported || v.Working {
		t.Fatalf("new generation's report must re-set the bit: %+v", v)
	}

	// A replacement generation whose registration facts carry status is
	// reported from the start (merge runs after the reset).
	replacement2 := registration("sess-s", "shell", "/tmp", true, 40)
	replacement2.NewGeneration = true
	replacement2.Facts.Working = ptr(true)
	if got, _, err = s.RegisterRunner(ctx, replacement2); err != nil {
		t.Fatal(err)
	}
	if !got.StatusReported || !got.Working {
		t.Fatalf("replacement with status facts must be reported: %+v", got)
	}

	// Registration facts carrying error also report; sweep preserves.
	reg2 := registration("sess-e", "shell", "/tmp", true, 40)
	reg2.Facts.Error = ptr(true)
	if got, _, err = s.RegisterRunner(ctx, reg2); err != nil {
		t.Fatal(err)
	}
	if !got.StatusReported {
		t.Fatal("registration error fact must report")
	}
	if _, err = s.SweepDeadSessions(ctx, []SessionID{"sess-e"}, 50); err != nil {
		t.Fatal(err)
	}
	if v, _, _ = s.Session(ctx, "sess-e"); !v.StatusReported || !v.Error {
		t.Fatalf("sweep must preserve status facts: %+v", v)
	}

	// InsertSession derives the bit from working/error when unset.
	ins, _, err := s.InsertSession(ctx, NewSession{ID: "sess-i", Adapter: "shell", CWD: "/tmp", CreatedAt: 60, Working: true})
	if err != nil {
		t.Fatal(err)
	}
	if !ins.StatusReported {
		t.Fatal("insert with working=true must be reported")
	}
}

// TestReferenceNodeIDRoundTrip pins the durable reference node_id (review
// fable H-1 decision, ADR 0017): stamped at creation, round-trips through
// the catalog, updatable in place (metadata, not identity), and absent on
// owned entries.
func TestReferenceNodeIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)

	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
		owned("app", "/app"),
		{Reference: &ProjectReference{PeerKey: "tower", Slug: "remote", NodeID: "node-abc"}},
		{Reference: &ProjectReference{PeerKey: "old", Slug: "legacy"}}, // pre-ADR-0007 daemon: no node id
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cat[0].NodeID != "" || cat[1].NodeID != "node-abc" || cat[2].NodeID != "" {
		t.Fatalf("catalog node ids: %+v", cat)
	}

	// Reload from a fresh read (durability).
	cat, err = s.ListProjectCatalog(ctx)
	if err != nil || cat[1].NodeID != "node-abc" {
		t.Fatalf("reload: %v %+v", err, cat)
	}

	// In-place node_id update is a metadata change, not an identity change.
	specs := []ProjectEntrySpec{
		{ID: cat[0].ID, Owned: &OwnedProjectSpec{Slug: "app", Rules: []MatchRule{{Path: "/app"}}}},
		{ID: cat[1].ID, Reference: &ProjectReference{PeerKey: "tower", Slug: "remote", NodeID: "node-xyz"}},
		{ID: cat[2].ID, Reference: &ProjectReference{PeerKey: "old", Slug: "legacy"}},
	}
	cat, r, err := s.ReplaceProjectCatalog(ctx, specs, 2)
	if err != nil || !r.Changed {
		t.Fatalf("node_id update: %v changed=%v", err, r.Changed)
	}
	if cat[1].NodeID != "node-xyz" {
		t.Fatalf("updated node id: %+v", cat[1])
	}

	// Identical spec (incl. node_id) is a no-op.
	_, r, err = s.ReplaceProjectCatalog(ctx, specs2(cat), 3)
	if err != nil || r.Changed {
		t.Fatalf("identical replace must be a no-op: %v %+v", err, r)
	}
}

// specs2 rebuilds the spec list from a catalog (identity-preserving).
func specs2(cat ProjectCatalog) []ProjectEntrySpec {
	out := make([]ProjectEntrySpec, len(cat))
	for i, e := range cat {
		if e.Kind == ProjectEntryOwned {
			out[i] = ProjectEntrySpec{ID: e.ID, Owned: &OwnedProjectSpec{Slug: e.Slug, Rules: e.Rules}}
		} else {
			out[i] = ProjectEntrySpec{ID: e.ID, Reference: &ProjectReference{PeerKey: e.PeerKey, Slug: e.Slug, NodeID: e.NodeID}}
		}
	}
	return out
}
