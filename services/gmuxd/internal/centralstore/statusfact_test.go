package centralstore

import (
	"context"
	"testing"
)

// TestStatusReportedFactLifecycle pins the runner-authoritative,
// generation-scoped status-reported bit (review fable M-1 / FD-7 decision
// + delta review Δ-1): unset until an active/error fact is observed, set
// by any observation carrying one (including an explicit false), never
// set by acknowledgement, RESET by a replacement generation alongside the
// other generation-scoped facts (re-set when the new generation's own
// facts carry status), preserved by the sweep (death of the same
// generation).
func TestStatusReportedFactLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)

	// Registration without status facts: not reported.
	reg := registration("19bj3702", "shell", "/tmp", true, 10)
	got, _, err := s.RegisterRunner(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusReported {
		t.Fatal("fresh registration without status facts must not be reported")
	}

	// Acknowledgement path never sets it (daemon-side, not runner).
	if _, err = s.AcknowledgeDeadSession(ctx, "19bj3702", got.Version); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Session(ctx, "19bj3702"); v.StatusReported {
		t.Fatal("acknowledgement must not set status-reported")
	}

	// An observation carrying an explicit active=false IS a report.
	v, _, _ := s.Session(ctx, "19bj3702")
	if _, err = s.ApplyRunnerObservation(ctx, RunnerObservation{ID: "19bj3702", ObservedVersion: v.Version, ObservedAt: 20, Facts: RunnerFacts{Active: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	v, _, _ = s.Session(ctx, "19bj3702")
	if !v.StatusReported || v.Active {
		t.Fatalf("explicit false status must set reported: %+v", v)
	}

	// A replacement generation resets the bit with the other
	// generation-scoped facts (Δ-1): a resumed generation that never
	// reports must render "status": null (wait verdict "died"), not
	// inherit the dead generation's report.
	replacement := registration("19bj3702", "shell", "/tmp", true, 30)
	replacement.NewGeneration = true
	got, _, err = s.RegisterRunner(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusReported || got.Active {
		t.Fatalf("replacement generation must reset the reported bit: %+v", got)
	}

	// ...and the new generation's own status facts re-set it (Δ-3: a
	// reported all-false status is a valid re-entered state).
	v, _, _ = s.Session(ctx, "19bj3702")
	if _, err = s.ApplyRunnerObservation(ctx, RunnerObservation{ID: "19bj3702", ObservedVersion: v.Version, ObservedAt: 35, Facts: RunnerFacts{Active: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ = s.Session(ctx, "19bj3702"); !v.StatusReported || v.Active {
		t.Fatalf("new generation's report must re-set the bit: %+v", v)
	}

	// A replacement generation whose registration facts carry status is
	// reported from the start (merge runs after the reset).
	replacement2 := registration("19bj3702", "shell", "/tmp", true, 40)
	replacement2.NewGeneration = true
	replacement2.Facts.Active = ptr(true)
	if got, _, err = s.RegisterRunner(ctx, replacement2); err != nil {
		t.Fatal(err)
	}
	if !got.StatusReported || !got.Active {
		t.Fatalf("replacement with status facts must be reported: %+v", got)
	}

	// Registration facts carrying error also report; sweep preserves.
	reg2 := registration("1q4ts9e1", "shell", "/tmp", true, 40)
	reg2.Facts.Error = ptr(true)
	if got, _, err = s.RegisterRunner(ctx, reg2); err != nil {
		t.Fatal(err)
	}
	if !got.StatusReported {
		t.Fatal("registration error fact must report")
	}
	if _, err = s.SweepDeadSessions(ctx, []SessionID{"1q4ts9e1"}, 50); err != nil {
		t.Fatal(err)
	}
	if v, _, _ = s.Session(ctx, "1q4ts9e1"); !v.StatusReported || !v.Error {
		t.Fatalf("sweep must preserve status facts: %+v", v)
	}

	// InsertSession derives the bit from active/error when unset.
	ins, _, err := s.InsertSession(ctx, NewSession{ID: "1kum8jak", Adapter: "shell", CWD: "/tmp", CreatedAt: 60, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if !ins.StatusReported {
		t.Fatal("insert with active=true must be reported")
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

// TestInterruptedFactRoundTrip pins the third canonical status fact: an
// intentional stop is durable, orthogonal to Error, counts as a reported
// status, and is reset by a replacement generation with the other
// generation-scoped facts.
func TestInterruptedFactRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	if _, _, err := s.RegisterRunner(ctx, registration("1kum8jak", "pi", "/tmp", true, 10)); err != nil {
		t.Fatal(err)
	}
	v, _, _ := s.Session(ctx, "1kum8jak")
	obs := RunnerObservation{ID: "1kum8jak", ObservedVersion: v.Version, ObservedAt: 20,
		Facts: RunnerFacts{Active: ptr(false), Error: ptr(false), Interrupted: ptr(true)}}
	if _, err := s.ApplyRunnerObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	v, _, _ = s.Session(ctx, "1kum8jak")
	if !v.Interrupted || v.Active || v.Error || !v.StatusReported {
		t.Fatalf("interrupted turn end = %+v, want interrupted-only reported status", v)
	}

	// A new turn clears the interruption (the runner replaces the status
	// wholesale; the store must not make it sticky).
	obs = RunnerObservation{ID: "1kum8jak", ObservedVersion: v.Version, ObservedAt: 30,
		Facts: RunnerFacts{Active: ptr(true), Error: ptr(false), Interrupted: ptr(false)}}
	if _, err := s.ApplyRunnerObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	if v, _, _ = s.Session(ctx, "1kum8jak"); v.Interrupted || !v.Active {
		t.Fatalf("new turn = %+v, want active without interruption", v)
	}

	// Active+Error is representable: an active retry/rate-limit condition
	// is not a terminal failure and must not close the turn.
	obs = RunnerObservation{ID: "1kum8jak", ObservedVersion: v.Version, ObservedAt: 40,
		Facts: RunnerFacts{Active: ptr(true), Error: ptr(true)}}
	if _, err := s.ApplyRunnerObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	if v, _, _ = s.Session(ctx, "1kum8jak"); !v.Active || !v.Error {
		t.Fatalf("active error = %+v, want Active and Error together", v)
	}

	// Replacement generation resets it with the other generation-scoped facts.
	repl := registration("1kum8jak", "pi", "/tmp", true, 50)
	repl.NewGeneration = true
	repl.Facts.Interrupted = nil
	got, _, err := s.RegisterRunner(ctx, repl)
	if err != nil {
		t.Fatal(err)
	}
	if got.Interrupted || got.StatusReported {
		t.Fatalf("replacement generation = %+v, want interruption reset", got)
	}
}

// TestStatusFactsWithoutReportedBitAreCorrupt: the status facts and the
// status-reported provenance bit are one invariant. A row carrying any of
// active/error/interrupted with status_reported = 0 could not have been
// written by this package, so reading it is a hard corruption error rather
// than a silent "status": null that would flip gmux wait's died/idle verdict.
// Written through the raw handle because the domain layer cannot produce it.
func TestStatusFactsWithoutReportedBitAreCorrupt(t *testing.T) {
	ctx := context.Background()
	s := openKernelStore(t)
	for _, column := range []string{"active", "error_column", "interrupted"} {
		col := column
		if col == "error_column" {
			col = "has_error"
		}
		id := "corrupt-" + col
		if _, _, err := s.InsertSession(ctx, NewSession{
			ID: SessionID(id), Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp",
			Remotes: map[string]string{}, CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.database.ExecContext(ctx,
			"UPDATE local_sessions SET "+col+" = 1, status_reported = 0 WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Session(ctx, SessionID(id)); err == nil {
			t.Errorf("%s = 1 with status_reported = 0 must be rejected", col)
		}
	}
}
