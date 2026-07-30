package main

import (
	"context"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// A reorder must reach subscribers as new project_index stamps on
// snapshot.sessions, which is the only thing the sidebar orders by.
//
// Asserted over the composed graph (real store → coordinator → composer →
// wire.Cache → Frames) rather than on the mutation's dirty flags alone: the
// bug this pins was a *seam* bug. The cache does re-emit a sessions frame for
// a projects-only batch, but rebuilds it from the last cached sessions
// payload, whose placement join is exactly what a reorder invalidates. So a
// world-only reorder republished the pre-reorder order — persisted, silent,
// and invisible until an unrelated session event happened to recompose.
func TestHarnessReorderRepublishesSessionStamps(t *testing.T) {
	ctx := context.Background()
	fleet := newHarnessFleet(0)
	frames := make(chan wire.Frames, 64)
	store, b := openHarness(t, t.TempDir(), fleet, func(_ context.Context, f wire.Frames) {
		select {
		case frames <- f:
		default:
		}
	})
	defer store.Close()
	if _, err := b.Converge(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.StartPostConvergence(ctx, fleet.endpoints()); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	catalog, res, err := store.ReplaceProjectCatalog(ctx, []centralstore.ProjectEntrySpec{
		{Owned: &centralstore.OwnedProjectSpec{Slug: "proj", Rules: []centralstore.MatchRule{{Path: "/"}}}},
	}, 100)
	if err != nil {
		t.Fatal(err)
	}
	b.Composer.Invalidate(res)
	project := catalog[0].ID

	// Placement joins the session row, so the rows are registered directly
	// rather than waited for from the runner transport: convergence runs
	// under a millisecond-scale budget, and this test is about the
	// publication seam, not about registration timing.
	first, second := centralstore.SessionID("10uymur5"), centralstore.SessionID("13s6ewv5")
	for _, id := range []centralstore.SessionID{first, second} {
		row, reg, e := store.RegisterRunner(ctx, centralstore.RunnerRegistration{
			ID: id, Adapter: "shell", Alive: true, CreatedAt: 1, ObservedAt: 1,
		})
		if e != nil {
			t.Fatal(e)
		}
		b.Composer.Invalidate(reg)
		r, e := store.PlaceLocalSession(ctx, row.ID, project)
		if e != nil {
			t.Fatal(e)
		}
		b.Composer.Invalidate(r)
	}
	if got := awaitStamps(t, frames, map[centralstore.SessionID]int{first: 0, second: 1}); !got {
		t.Fatal("placement stamps never reached a sessions frame")
	}

	// The production route (PATCH /v1/projects/{slug}/sessions) reorders
	// through the coordinator, which publishes the mutation's dirtiness.
	if _, err = b.Coordinator.ReorderSiblingScopes(ctx, []centralstore.SiblingReorder{{
		Project: project,
		Order:   []centralstore.SubjectRef{{LocalSessionID: second}, {LocalSessionID: first}},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := awaitStamps(t, frames, map[centralstore.SessionID]int{second: 0, first: 1}); !got {
		t.Fatal("reorder did not republish snapshot.sessions with the new project_index stamps")
	}
}

// awaitStamps reports whether a sessions frame carrying exactly want (session
// ID → project_index, all rows placed in a project) arrives in time.
func awaitStamps(t *testing.T, frames <-chan wire.Frames, want map[centralstore.SessionID]int) bool {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-frames:
			if f.Sessions == nil {
				continue
			}
			got := map[centralstore.SessionID]int{}
			for _, s := range f.Sessions.Sessions {
				if s.ProjectSlug != "" {
					got[centralstore.SessionID(s.ID)] = s.ProjectIndex
				}
			}
			if len(got) != len(want) {
				continue
			}
			match := true
			for id, idx := range want {
				if got[id] != idx {
					match = false
					break
				}
			}
			if match {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
