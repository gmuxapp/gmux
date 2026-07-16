package sessioncoord

import (
	"context"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// LocalPeerInputSource supplies the point-in-time connected Local-peer match
// inputs (ADR 0025 exception: the parent owns Local-peer placement). It is
// called under the lifecycle mutex, so it must be a fast in-memory snapshot
// (the peer manager's own lock), never I/O.
type LocalPeerInputSource func() []centralstore.LocalPeerMatchInput

// WithLocalPeerMatchInputs injects the Local-peer snapshot source used by
// ReplaceCatalog. Production wiring adapts the peering manager onto it.
func WithLocalPeerMatchInputs(src LocalPeerInputSource) Option {
	return func(c *Coordinator) { c.localPeerInputs = src }
}

// ReplaceCatalog is the authoritative project-configuration mutation (the
// PUT /v1/projects and add-project routes; design §6). Under one lifecycle
// mutex hold it:
//
//  1. snapshots the connected Local-peer match inputs (point-in-time);
//  2. commits ReplaceProjectCatalogAndRematch (placed subjects re-derived,
//     movers appended, scopes densified);
//  3. runs the live auto-assign pass: every session with an installed live
//     generation that is unplaced-but-matching under the new rules is placed
//     via PlaceUnplacedSessions — liveness proof stays with the coordinator,
//     the matcher stays in the store (checklist item 1).
//
// Holding the mutex across both short transactions means no registration can
// interleave between the rematch and the auto-assign, so a session cannot be
// double-placed or missed. Committed outcomes publish outside the mutex as
// one combined invalidation. A replace that committed followed by a failing
// auto-assign still publishes the replace outcome — the catalog change is
// durable regardless; the error surfaces to the caller and the next
// registration or catalog change converges placement.
func (c *Coordinator) ReplaceCatalog(ctx context.Context, specs []centralstore.ProjectEntrySpec) (centralstore.ProjectCatalog, error) {
	c.mu.Lock()
	var peers []centralstore.LocalPeerMatchInput
	if c.localPeerInputs != nil {
		peers = c.localPeerInputs()
	}
	at := c.now()
	catalog, replaceResult, err := c.durable.ReplaceProjectCatalogAndRematch(ctx, specs, peers, at)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	// liveSessionIDs deliberately excludes superseded (fenced) entries —
	// unlike Snapshot — so a mid-replacement generation is never treated as
	// live here. Today a fence cannot be observed under this mutex hold
	// (fences resolve within a single Register hold), but this is the only
	// caller that acts on liveness from an enumeration, so it must not
	// depend on that invariant surviving future reordering (review L-4).
	live := c.registry.liveSessionIDs()
	var placeResult centralstore.MutationResult
	var placeErr error
	if len(live) > 0 {
		placeResult, placeErr = c.durable.PlaceUnplacedSessions(ctx, live, at)
	}
	c.mu.Unlock()

	combined := centralstore.MutationResult{
		Changed:       replaceResult.Changed || placeResult.Changed,
		SessionsDirty: replaceResult.SessionsDirty || placeResult.SessionsDirty,
		WorldDirty:    replaceResult.WorldDirty || placeResult.WorldDirty,
	}
	c.publish(ctx, combined)
	if placeErr != nil {
		return catalog, fmt.Errorf("sessioncoord: live auto-assign after catalog replace: %w", placeErr)
	}
	return catalog, nil
}
