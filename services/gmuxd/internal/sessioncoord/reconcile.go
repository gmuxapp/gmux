package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

var (
	// ErrNoAdapterReconciler marks a Reconcile call without a configured
	// adapter boundary.
	ErrNoAdapterReconciler = errors.New("sessioncoord: no adapter reconciler configured")
	// ErrReconcileInFlight marks a Reconcile attempted while another pass is
	// running. Reconciliation is single-flight (fail fast, callers retry):
	// this trivially serializes all adapter probing, so no per-adapter
	// stampede is possible — at most one pass runs and it probes adapters
	// sequentially in bounded batches. Per-adapter parallelism is a
	// profiling question deferred to production wiring.
	ErrReconcileInFlight = errors.New("sessioncoord: reconciliation already in flight")
)

// ReconcileCandidate is one dead retained row offered to its owning adapter.
// Version is the row version observed at gather time; every consequence of
// the adapter's decision is applied conditionally against it (ADR 0026 §5).
type ReconcileCandidate struct {
	ID              centralstore.SessionID
	Adapter         string
	ConversationRef string
	Command         []string
	Version         centralstore.RowVersion
}

// Disposition is the adapter's answer for one candidate (ADR 0026 §5:
// retain, remove, or unknown).
type Disposition int

const (
	// DispositionUnknown retains conservatively: the adapter could not tell
	// (storage unreachable, probe failed). Also the fallback for missing or
	// malformed decisions.
	DispositionUnknown Disposition = iota
	// DispositionRetain confirms the conversation exists and is resumable.
	DispositionRetain
	// DispositionRemove confirms the session is no longer resumable; the
	// row is removed (adapter-confirmed non-resumable rows are removed,
	// ADR 0026 §4).
	DispositionRemove
)

// ReconcileDecision is one adapter decision for a probed candidate.
type ReconcileDecision struct {
	ID          centralstore.SessionID
	Disposition Disposition
}

// AdapterReconciler probes one bounded batch of dead retained candidates
// owned by one adapter. Adapter I/O: never called under coordinator locks or
// inside a database transaction. An error fails the whole batch to Unknown;
// a missing, duplicate-conflicting, or unrequested decision degrades to
// Unknown for that candidate. Adapters return decisions and never write the
// store (ADR 0026 §5).
type AdapterReconciler interface {
	ReconcileRetained(ctx context.Context, adapter string, batch []ReconcileCandidate) ([]ReconcileDecision, error)
}

// ResumeVerdict is the runtime-only reconciliation verdict for one retained
// dead row. It is never persisted (ADR 0026 §4 forbids persisting
// resumability; a verdict is a cached derivation of it, owned by adapter
// storage that can change while gmuxd is down) — a fresh daemon re-probes.
type ResumeVerdict int

const (
	VerdictUnknown ResumeVerdict = iota
	VerdictResumable
	// VerdictGone is adapter-confirmed non-resumable. It normally lives only
	// for the instant before the conditional removal commits; it survives
	// when that removal hit a database error, narrowing the snapshot overlay
	// while a later pass retries.
	VerdictGone
)

// WithAdapterReconciler injects the adapter probing boundary used by
// Reconcile.
func WithAdapterReconciler(r AdapterReconciler) Option {
	return func(c *Coordinator) { c.reconciler = r }
}

// WithReconcileBatchSize overrides the per-adapter probe batch bound
// (default 32).
func WithReconcileBatchSize(n int) Option {
	return func(c *Coordinator) {
		if n > 0 {
			c.reconcileBatch = n
		}
	}
}

// ResumeVerdicts returns a point-in-time copy of the verdict map. The
// snapshot composer's VerdictSource seam is adapted onto this at cutover
// (publish-after-change contract: Reconcile publishes committed outcomes,
// and verdict-only changes are runtime overlay state re-read every pass).
func (c *Coordinator) ResumeVerdicts() map[centralstore.SessionID]ResumeVerdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[centralstore.SessionID]ResumeVerdict, len(c.verdicts))
	for id, v := range c.verdicts {
		out[id] = v
	}
	return out
}

// setVerdict records a verdict under the lifecycle mutex (caller holds it)
// and reports whether the stored verdict actually changed.
func (c *Coordinator) setVerdict(id centralstore.SessionID, v ResumeVerdict) bool {
	if v == VerdictUnknown {
		_, had := c.verdicts[id]
		delete(c.verdicts, id)
		return had
	}
	if c.verdicts == nil {
		c.verdicts = make(map[centralstore.SessionID]ResumeVerdict)
	}
	if c.verdicts[id] == v {
		return false
	}
	c.verdicts[id] = v
	return true
}

// Reconcile runs one adapter reconciliation pass: gather dead retained
// candidates, probe their owning adapters in bounded batches, and apply each
// decision conditionally. It also converges conversation takeover for dead
// rows covered by a live binder (races and late lineage that the
// registration-time eviction missed). Returns the IDs whose rows were
// removed.
//
// Conditionality per the ADR amendment: gated on the closed startup
// convergence barrier (a surviving runner must get the chance to claim its
// row before its adapter is asked about it) and single-flight
// (ErrReconcileInFlight). Triggering — startup after the barrier, on session
// death, on demand — is the caller's policy; the coordinator owns no timer
// and no internal retry: a failed or unknown probe simply leaves the row
// retained until the next caller-triggered pass.
//
// Lock discipline matches every other lifecycle operation: adapter I/O never
// runs under the mutex; each conditional removal is a short DB transaction
// under it; committed outcomes publish outside it.
//
// The second return value reports whether the pass changed any runtime
// resume verdict (set, narrowed, or cleared). Verdicts are runtime overlay
// state — no committed outcome invalidates the snapshot for them — so
// production wiring uses this to mark the sessions payload dirty when the
// overlay moved (design §4 item 2).
func (c *Coordinator) Reconcile(ctx context.Context) ([]centralstore.SessionID, bool, error) {
	if c.reconciler == nil {
		return nil, false, ErrNoAdapterReconciler
	}

	// ── Gather (mutex) ────────────────────────────────────────────────────
	c.mu.Lock()
	if !c.convergeClosed {
		c.mu.Unlock()
		return nil, false, fmt.Errorf("%w: reconciliation waits for the convergence barrier", ErrConvergencePending)
	}
	if c.reconcileInFlight {
		c.mu.Unlock()
		return nil, false, ErrReconcileInFlight
	}
	c.reconcileInFlight = true
	// While the pass is in flight, verdict invalidations (registration,
	// Remove) are recorded so the apply loop below never re-sets a verdict
	// that was invalidated after gather — the invalidation invariant must
	// hold even across this pass's own writes.
	c.verdictsInvalidated = make(map[centralstore.SessionID]bool)
	defer func() {
		c.mu.Lock()
		c.reconcileInFlight = false
		c.verdictsInvalidated = nil
		c.mu.Unlock()
	}()

	sessions, err := c.durable.ListSessions(ctx)
	if err != nil {
		c.mu.Unlock()
		return nil, false, err
	}
	type liveRef struct {
		owner        centralstore.SessionID
		adapter, ref string
	}
	var liveRefs []liveRef
	var candidates []ReconcileCandidate
	for _, s := range sessions {
		if _, live := c.registry.current(s.ID); live {
			if s.ConversationRef != "" {
				liveRefs = append(liveRefs, liveRef{s.ID, s.Adapter, s.ConversationRef})
			}
			continue
		}
		if c.registry.fenced(s.ID) {
			continue // a replacement is inside its commit-to-install window
		}
		if _, busy := c.ops[s.ID]; busy {
			continue // a resume/stop/restart owns this row right now
		}
		candidates = append(candidates, ReconcileCandidate{
			ID: s.ID, Adapter: s.Adapter, ConversationRef: s.ConversationRef,
			Command: append([]string(nil), s.Command...), Version: s.Version,
		})
	}
	c.mu.Unlock()

	// ── Probe (no locks; adapter I/O) ────────────────────────────────────
	byAdapter := make(map[string][]ReconcileCandidate)
	for _, cand := range candidates {
		byAdapter[cand.Adapter] = append(byAdapter[cand.Adapter], cand)
	}
	adapters := make([]string, 0, len(byAdapter))
	for a := range byAdapter {
		adapters = append(adapters, a)
	}
	sort.Strings(adapters)

	dispositions := make(map[centralstore.SessionID]Disposition, len(candidates))
	for _, adapter := range adapters {
		group := byAdapter[adapter]
		if c.takeover {
			c.lineage.warm(ctx, c.resolver, adapter, takeoverRefs(sessions, adapter))
		}
		for start := 0; start < len(group); start += c.reconcileBatch {
			end := min(start+c.reconcileBatch, len(group))
			batch := group[start:end]
			requested := make(map[centralstore.SessionID]bool, len(batch))
			for _, cand := range batch {
				requested[cand.ID] = true
			}
			decisions, probeErr := c.reconciler.ReconcileRetained(ctx, adapter, batch)
			if probeErr != nil {
				// Whole batch unknown: retain conservatively, re-probe on the
				// next pass.
				c.reportError(ctx, fmt.Errorf("sessioncoord: reconcile probe for adapter %s: %w", adapter, probeErr))
				continue
			}
			for _, d := range decisions {
				if !requested[d.ID] {
					continue // unrequested or duplicate: first decision wins
				}
				requested[d.ID] = false
				dispositions[d.ID] = d.Disposition
			}
		}
	}

	// Takeover convergence: a dead candidate covered by a live binder's
	// conversation is removed regardless of its adapter disposition — the
	// identity lives on in the winner (production
	// resolveConversationTakeoverLocked parity, applied lazily for races and
	// late lineage).
	// covered maps each candidate to the live owner whose conversation
	// covers it. The owner is re-validated at apply time: coverage computed
	// here is gather-time state, and an owner that died or was removed during
	// the (unbounded) probe phase must not cause deletion of a row the
	// adapter retained — the coverage predicate itself can evaporate, and the
	// loser's version-conditional delete only protects against the LOSER
	// changing.
	covered := make(map[centralstore.SessionID]centralstore.SessionID)
	if c.takeover {
		for _, cand := range candidates {
			if cand.ConversationRef == "" {
				continue
			}
			for _, lr := range liveRefs {
				if lr.adapter == cand.Adapter && c.lineage.covers(cand.Adapter, lr.ref, cand.ConversationRef) {
					covered[cand.ID] = lr.owner
					break
				}
			}
		}
	}

	// ── Apply (mutex per decision, short DB tx; publish outside) ─────────
	var removed []centralstore.SessionID
	var outcomes []centralstore.MutationResult
	verdictsChanged := false
	for _, cand := range candidates {
		// VerdictGone means ADAPTER-confirmed non-resumable, nothing else. A
		// covered row is removed because a live binder owns its conversation;
		// if that removal fails, its verdict must stay Unknown — the adapter
		// never said the conversation is gone.
		adapterConfirmed := dispositions[cand.ID] == DispositionRemove
		c.mu.Lock()
		_, live := c.registry.current(cand.ID)
		_, busy := c.ops[cand.ID]
		if live || c.registry.fenced(cand.ID) || busy {
			// The row changed hands while probing: the verdict no longer
			// describes it. Skip; any registration already cleared the
			// verdict, and a claim owner re-triggers reconciliation policy.
			verdictsChanged = c.setVerdict(cand.ID, VerdictUnknown) || verdictsChanged
			c.mu.Unlock()
			continue
		}
		// Coverage is only actionable while its owner still has an installed
		// live generation; otherwise degrade to the adapter's disposition.
		coverageValid := false
		if owner, hit := covered[cand.ID]; hit {
			_, coverageValid = c.registry.current(owner)
		}
		// A verdict invalidated during this pass (registration, Remove) must
		// not be re-set by this pass's stale probe results.
		invalidated := c.verdictsInvalidated[cand.ID]
		switch {
		case adapterConfirmed || coverageValid:
			if adapterConfirmed && !invalidated {
				verdictsChanged = c.setVerdict(cand.ID, VerdictGone) || verdictsChanged
			}
			result, removeErr := c.durable.RemoveSessionAtVersion(ctx, cand.ID, cand.Version)
			switch {
			case removeErr == nil:
				verdictsChanged = c.setVerdict(cand.ID, VerdictUnknown) || verdictsChanged
				removed = append(removed, cand.ID)
				outcomes = append(outcomes, result)
			case errors.Is(removeErr, centralstore.ErrStaleVersion), errors.Is(removeErr, centralstore.ErrSessionNotFound):
				// The decision went stale (any conditional mutation landed)
				// or the row vanished: skip silently, next pass re-probes.
				verdictsChanged = c.setVerdict(cand.ID, VerdictUnknown) || verdictsChanged
			default:
				// Database failure: an adapter-confirmed Gone verdict (set
				// above, unless invalidated mid-pass) survives to narrow the
				// snapshot overlay while a later pass retries the removal; a
				// coverage-only removal keeps no verdict.
				c.mu.Unlock()
				c.reportError(ctx, fmt.Errorf("sessioncoord: reconcile removal of %s: %w", cand.ID, removeErr))
				continue
			}
		case dispositions[cand.ID] == DispositionRetain:
			if !invalidated {
				verdictsChanged = c.setVerdict(cand.ID, VerdictResumable) || verdictsChanged
			}
		default:
			verdictsChanged = c.setVerdict(cand.ID, VerdictUnknown) || verdictsChanged
		}
		c.mu.Unlock()
	}

	for _, r := range outcomes {
		c.publish(ctx, r)
	}
	return removed, verdictsChanged, nil
}
