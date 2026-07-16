package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

var (
	ErrGenerationActive = errors.New("sessioncoord: a working generation is already installed")
	// ErrInvalidSessionID marks a runner whose meta reported a session ID
	// that fails paths.IsValidSessionID. The ID is used as a filesystem path
	// segment, so this is security-relevant and enforced for every caller
	// (discovery, /v1/register, startup convergence, resume/restart
	// registrations). It is a permanent verdict: registration aborts before
	// any commit, fence, or registry change.
	ErrInvalidSessionID = errors.New("sessioncoord: invalid session id")
	// ErrReplaceWithoutClaim marks a registration carrying Replace or
	// ExpectedID provenance without presenting the token of the lifecycle
	// claim it runs under (or presenting a token that is not the installed
	// claim for that session). Replace provenance is only legal inside
	// claimed operations (Resume/Restart); discovery and /v1/register never
	// set these fields. This turns the wiring convention into a checked
	// invariant: a stray Replace registration can never displace a live
	// generation — not even while an unrelated operation's claim is held.
	ErrReplaceWithoutClaim = errors.New("sessioncoord: replace provenance requires a held lifecycle claim")
)

// EventStream is already ordered by the runner transport. Subscribe must not
// return until the subscription is established, so replay/live events can be
// buffered before Meta starts.
type EventStream interface {
	Events() <-chan RunnerEvent
	Close() error
}

type RunnerClient interface {
	Subscribe(context.Context, string) (EventStream, error)
	Meta(context.Context, string) (RunnerMeta, error)
}

type Durable interface {
	RegisterRunner(context.Context, centralstore.RunnerRegistration) (centralstore.Session, centralstore.MutationResult, error)
	ApplyRunnerObservation(context.Context, centralstore.RunnerObservation) (centralstore.MutationResult, error)
	// Session backs the lifecycle operations' durable-row checks (resume
	// candidacy, exit-convergence repair). See lifecycle.go.
	Session(context.Context, centralstore.SessionID) (centralstore.Session, bool, error)
	// ListSessions and SweepDeadSessions back the startup convergence
	// barrier (see convergence.go).
	ListSessions(context.Context) ([]centralstore.Session, error)
	SweepDeadSessions(context.Context, []centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error)
	// AcknowledgeDeadSession backs AcknowledgeDead (see acknowledge.go).
	AcknowledgeDeadSession(context.Context, centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error)
	// ReplaceProjectCatalogAndRematch and PlaceUnplacedSessions back the
	// catalog-replacement operation (see catalog.go).
	ReplaceProjectCatalogAndRematch(context.Context, []centralstore.ProjectEntrySpec, []centralstore.LocalPeerMatchInput, centralstore.UnixMillis) (centralstore.ProjectCatalog, centralstore.MutationResult, error)
	PlaceUnplacedSessions(context.Context, []centralstore.SessionID, centralstore.UnixMillis) (centralstore.MutationResult, error)
	// DismissSessionTree and RemoveSessionAtVersion back the dismissal and
	// hard-deletion coordinator operations (see dismiss.go).
	DismissSessionTree(context.Context, centralstore.SessionID, centralstore.UnixMillis) ([]centralstore.SessionID, centralstore.MutationResult, error)
	RemoveSessionAtVersion(context.Context, centralstore.SessionID, centralstore.RowVersion) (centralstore.MutationResult, error)
}

// The production store satisfies the coordinator's durable seam.
var _ Durable = (*centralstore.Store)(nil)

// DirtySink receives committed outcomes only. It is always called after the
// store transaction has returned and without any coordinator lock held. A
// sink that blocks delays later lifecycle transitions; a re-entrant sink (one
// that calls back into the coordinator) is supported.
type DirtySink interface {
	Committed(context.Context, centralstore.MutationResult)
}
type DirtySinkFunc func(context.Context, centralstore.MutationResult)

func (f DirtySinkFunc) Committed(ctx context.Context, r centralstore.MutationResult) { f(ctx, r) }

// ErrorSink receives non-fatal observable errors (stale-version retry
// exhaustion, malformed exit events, durable observation failures). A nil
// ErrorSink discards all errors.
type ErrorSink interface {
	Error(context.Context, error)
}
type ErrorSinkFunc func(context.Context, error)

func (f ErrorSinkFunc) Error(ctx context.Context, err error) { f(ctx, err) }

type RunnerMeta struct {
	Registration  centralstore.RunnerRegistration
	PID           int
	RunnerVersion string
	BinaryHash    string
}

type RunnerEvent struct {
	ObservedAt centralstore.UnixMillis
	Facts      centralstore.RunnerFacts
	Alive      *bool
}

type RegisterRequest struct {
	Endpoint string
	// Replace is explicit resume/restart/replacement provenance. Without it,
	// registration never displaces an installed working generation.
	Replace bool
	// ExpectedID, when set, scopes the request to one session: if the
	// runner's meta reports a different ID, registration aborts before any
	// commit or fence with ErrResumeIdentityMismatch. Replace provenance is
	// authorized for a specific session, not for whatever ID a spawned
	// runner happens to claim — without this check a mis-claiming runner
	// could supersede an unrelated live session's generation.
	ExpectedID centralstore.SessionID
	// Claim is the lifecycle claim token authorizing Replace/ExpectedID
	// provenance. Register verifies token identity against the installed
	// claim for the runner's ID under the lifecycle mutex — the caller must
	// hold its OWN claim, not merely observe that some claim exists
	// (otherwise a stray Replace during an unrelated Stop would pass).
	Claim *LifecycleClaim
}

type Coordinator struct {
	mu       sync.Mutex
	next     uint64
	registry *Registry
	runners  RunnerClient
	durable  Durable
	dirty    DirtySink
	errSink  ErrorSink
	control  RunnerControl
	spawner  RunnerSpawner
	now      func() centralstore.UnixMillis

	// takeover/resolver/lineage back conversation takeover (takeover.go).
	// The lineage cache has its own lock; warming is adapter I/O and never
	// runs under mu.
	takeover bool
	resolver ConversationResolver
	lineage  lineageCache
	// takeoverWarnOnce rate-limits the takeover-unconfigured warning to once
	// per process.
	takeoverWarnOnce sync.Once

	// reconciler/reconcileBatch/reconcileInFlight/verdicts back adapter
	// reconciliation (reconcile.go). verdicts is guarded by mu; it is
	// runtime-only (never persisted) and re-populated by probing.
	// localPeerInputs backs ReplaceCatalog's point-in-time Local-peer match
	// input snapshot (see catalog.go).
	localPeerInputs LocalPeerInputSource

	reconciler        AdapterReconciler
	reconcileBatch    int
	reconcileInFlight bool
	verdicts          map[centralstore.SessionID]ResumeVerdict
	// verdictsInvalidated is non-nil exactly while a reconcile pass is in
	// flight; it records IDs whose verdicts were invalidated (registration,
	// Remove) after the pass gathered its candidates, so the pass never
	// re-sets a stale verdict on them.
	verdictsInvalidated map[centralstore.SessionID]bool

	// ops tracks per-session in-flight lifecycle operations (stop, resume,
	// restart). Guarded by mu; held across those operations' runner I/O so a
	// concurrent lifecycle op for the same session fails fast instead of
	// double-spawning or double-killing. See lifecycle.go.
	ops map[centralstore.SessionID]*LifecycleClaim

	// beforeDismissLock is a test-only synchronization seam: when set, it is
	// called immediately before Dismiss attempts the lifecycle mutex, letting
	// serialization tests deterministically observe "blocked at the mutex".
	beforeDismissLock func()

	// outcomes is the post-commit outcome bus (see outcomes.go). It has its
	// own lock; publishing never runs under the lifecycle mutex.
	outcomes outcomeBus

	// Startup convergence barrier state (see convergence.go). Guarded by mu.
	convergeCandidates map[centralstore.SessionID]struct{}
	convergeClosed     bool
	converged          chan struct{}
}

// Option configures optional coordinator collaborators.
type Option func(*Coordinator)

// WithRunnerControl injects the process-termination boundary used by Stop
// and Restart.
func WithRunnerControl(rc RunnerControl) Option { return func(c *Coordinator) { c.control = rc } }

// WithRunnerSpawner injects the process-launch boundary used by Resume and
// Restart.
func WithRunnerSpawner(rs RunnerSpawner) Option { return func(c *Coordinator) { c.spawner = rs } }

// WithClock injects the timestamp source for synthesized exits. The default
// is the wall clock in Unix milliseconds.
func WithClock(now func() centralstore.UnixMillis) Option {
	return func(c *Coordinator) { c.now = now }
}

func New(registry *Registry, runners RunnerClient, durable Durable, dirty DirtySink, errSink ErrorSink, opts ...Option) *Coordinator {
	if registry == nil {
		registry = NewRegistry()
	}
	c := &Coordinator{
		registry: registry, runners: runners, durable: durable, dirty: dirty, errSink: errSink,
		now:            func() centralstore.UnixMillis { return centralstore.UnixMillis(time.Now().UnixMilli()) },
		ops:            make(map[centralstore.SessionID]*LifecycleClaim),
		converged:      make(chan struct{}),
		reconcileBatch: 32,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}
func (c *Coordinator) Registry() *Registry { return c.registry }

// Register performs subscribe-first convergence. Runner I/O (Subscribe and
// Meta) is performed outside the lifecycle mutex so a slow or hung runner
// cannot stall concurrent registrations or drain goroutines.
//
// The stream's own channel provides a natural buffer for events arriving
// during Meta. After Meta, the mutex is acquired and the stream channel is
// drained synchronously (non-blocking) before the DB registration. This
// guarantees no event is lost between Subscribe and install without racing
// against a separate goroutine for the pre-registration drain.
//
// On successful install the stream lifetime is detached from the request
// context and governed by the registry entry's cancel function. A canceled
// request context aborts Subscribe and Meta before install; it has no effect
// after the entry is installed.
func (c *Coordinator) Register(ctx context.Context, req RegisterRequest) (Runtime, error) {
	// ── Phase 1: runner I/O outside the lifecycle mutex ──────────────────
	//
	// streamCtx governs the installed stream for its full lifetime. During
	// setup we propagate ctx cancellation into streamCtx so a canceled
	// request aborts a hung Subscribe without leaking the goroutine or the
	// stream. Once the entry is installed (setupDone closed without cancel)
	// streamCtx is detached from ctx.

	streamCtx, streamCancel := context.WithCancel(context.Background())
	setupDone := make(chan struct{})
	// installMu makes install-vs-cancel atomic: the bridge goroutine may only
	// cancel the stream while `installed` is false, and the install path
	// flips the flag under the same lock immediately before installing. A
	// request context cancelled after install therefore cannot cancel the
	// installed stream.
	var installMu sync.Mutex
	installed := false
	go func() {
		select {
		case <-ctx.Done():
			installMu.Lock()
			if !installed {
				streamCancel()
			}
			installMu.Unlock()
		case <-setupDone:
		}
	}()

	streamInstalled := false
	defer func() {
		close(setupDone)
		if !streamInstalled {
			streamCancel()
		}
	}()

	stream, err := c.runners.Subscribe(streamCtx, req.Endpoint)
	if err != nil {
		return Runtime{}, err
	}
	streamCleaned := false
	defer func() {
		if !streamInstalled && !streamCleaned {
			_ = stream.Close()
		}
	}()

	// Events arriving from this point are buffered in stream.Events()
	// (the stream's own channel). Meta is called outside the lock so it
	// cannot stall unrelated drain goroutines.
	meta, err := c.runners.Meta(ctx, req.Endpoint)
	if err != nil {
		return Runtime{}, err
	}
	id := meta.Registration.ID
	if !paths.IsValidSessionID(string(id)) {
		return Runtime{}, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	if req.ExpectedID != "" && id != req.ExpectedID {
		// Abort before the mutex/fence/commit: no registration side effects.
		return Runtime{}, fmt.Errorf("%w: expected %s, runner reported %s", ErrResumeIdentityMismatch, req.ExpectedID, id)
	}

	// Takeover preparation (still I/O phase): read the durable rows and warm
	// the lineage cache for every same-adapter ref, so the coverage
	// computation under the mutex needs no I/O. A failed list read degrades
	// this registration to no takeover (availability beats eviction
	// completeness; the next reconciliation pass converges leftovers). The
	// list may be stale by commit time — registrations serialize on the
	// lifecycle mutex and evictions are version-conditional, so staleness
	// yields a missed eviction, never a wrong one.
	var takeoverList []centralstore.Session
	if c.takeover {
		list, listErr := c.durable.ListSessions(ctx)
		if listErr != nil {
			c.reportError(ctx, fmt.Errorf("sessioncoord: takeover list for %s: %w", id, listErr))
		} else {
			takeoverList = list
			metaRef := ""
			if meta.Registration.Facts.ConversationRef != nil {
				metaRef = *meta.Registration.Facts.ConversationRef
			}
			c.lineage.warm(ctx, c.resolver, meta.Registration.Adapter, takeoverRefs(list, meta.Registration.Adapter, metaRef))
		}
	}

	// ── Phase 2: lifecycle mutex ──────────────────────────────────────────
	// Holding the mutex across the short DB transaction is acceptable.
	// Runner I/O (Subscribe, Meta) must not run inside this section.

	c.mu.Lock()

	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return Runtime{}, err
	}

	if req.Replace || req.ExpectedID != "" {
		if req.Claim == nil || c.ops[id] != req.Claim {
			c.mu.Unlock()
			return Runtime{}, fmt.Errorf("%w: %s", ErrReplaceWithoutClaim, id)
		}
	}

	old, hadOld := c.registry.current(id)
	if hadOld && !req.Replace {
		c.mu.Unlock()
		return old.Runtime, ErrGenerationActive
	}

	c.next++
	generation := c.next
	reg := meta.Registration
	reg.NewGeneration = req.Replace

	// Drain events that arrived between Subscribe and now. We read directly
	// from stream.Events() with select-default (non-blocking) so this is
	// instantaneous. No goroutine contention: no drain goroutine is reading
	// stream.Events() yet.
	closed := false
	drainCh := stream.Events()
loop:
	for {
		select {
		case ev, ok := <-drainCh:
			if !ok {
				closed = true
				break loop
			}
			reduce(&reg, ev)
		default:
			break loop
		}
	}

	// A closed stream cannot establish liveness.
	if closed {
		reg.Alive = false
	}
	if !reg.Alive && reg.Facts.ExitedAt.Set == nil {
		// The runner died before registration completed (stream closed before
		// subscribe finished, or meta reported a dead runner) without an
		// observed exit fact. The store requires dead generations to carry an
		// explicit exit timestamp; synthesize it from the observation time so
		// a fast-dead registration does not fail with
		// ErrGenerationExitRequired.
		x := reg.ObservedAt
		reg.Facts.ExitedAt = centralstore.NullablePatch[centralstore.UnixMillis]{Set: &x}
	}

	// Conversation takeover (ADR 0026 §9). The post-drain merged ref decides
	// coverage; an event-bound ref that was never described degrades to ref
	// equality for this registration (the reconcile pass converges
	// lineage-only losers later). A live binder evicts covered dead rows in
	// the same RegisterRunner transaction; a genuinely new fast-dead
	// registration covered by a live row is skipped entirely (production
	// dead-write-skip parity) — no durable write, no registry change.
	if c.takeover && takeoverList != nil {
		ref := ""
		if reg.Facts.ConversationRef != nil {
			ref = *reg.Facts.ConversationRef
		} else {
			for _, s := range takeoverList {
				if s.ID == id {
					ref = s.ConversationRef
					break
				}
			}
		}
		if reg.Alive {
			reg.Evict = c.takeoverEvictions(id, reg.Adapter, ref, takeoverList)
		} else if c.coveredByLive(id, reg.Adapter, ref, takeoverList) {
			// The skip applies only to genuinely new rows: an existing row's
			// dead re-registration owns its identity (or already lost it to
			// the winner's eviction). The list was read before the mutex, so
			// confirm absence against the durable row under the mutex.
			_, exists, rowErr := c.durable.Session(ctx, id)
			if rowErr != nil {
				c.mu.Unlock()
				return Runtime{}, rowErr
			}
			if !exists {
				c.mu.Unlock()
				return Runtime{}, fmt.Errorf("%w: %s", ErrConversationOwnedByLive, id)
			}
		}
	}

	// Fence the old generation before committing the replacement. From this
	// point apply's current/advance checks fail for the old generation, so an
	// in-flight observation cannot commit onto the freshly registered row
	// during the commit-to-install window. The fence is lifted only if the
	// registration fails.
	if hadOld {
		c.registry.supersede(id, old.Generation)
	}

	session, outcome, err := c.durable.RegisterRunner(ctx, reg)
	if err != nil {
		if hadOld {
			c.registry.restore(id, old.Generation)
		}
		c.mu.Unlock()
		return Runtime{}, err
	}

	// Any committed registration invalidates reconciliation verdicts: the
	// merged facts (possibly a new conversation ref) supersede the probe that
	// produced them, and evicted losers no longer exist.
	c.invalidateVerdict(id)
	for _, ev := range reg.Evict {
		c.invalidateVerdict(ev.ID)
	}

	runtime := Runtime{
		SessionID:     id,
		Generation:    generation,
		Endpoint:      req.Endpoint,
		PID:           meta.PID,
		RunnerVersion: meta.RunnerVersion,
		BinaryHash:    meta.BinaryHash,
		// A closed pre-drain stream already forced reg.Alive=false above, so
		// liveness alone decides subscription.
		Subscribed:    reg.Alive,
		RowVersion:    session.Version,
	}

	if runtime.Subscribed {
		// Start the bounded buffer goroutine from stream.Events() now that
		// we've finished the synchronous pre-registration drain.
		events := bufferEvents(streamCtx, drainCh)
		installMu.Lock()
		installed = true
		installMu.Unlock()
		replacedEntry, replaced := c.registry.install(registryEntry{Runtime: runtime, cancel: streamCancel, stream: stream, dead: make(chan struct{})})
		streamInstalled = true
		if replaced {
			closeEntry(replacedEntry)
		}
		go c.drain(streamCtx, id, generation, events)
	} else {
		streamCleaned = true
		_ = stream.Close()
		if hadOld {
			// Fast-dead replacement: remove the (superseded) old live entry
			// without installing a new subscription.
			if removed, yes := c.registry.remove(id, old.Generation); yes {
				closeEntry(removed)
			}
		}
	}

	c.mu.Unlock()

	// Silent-loss guard: a conversation-bearing registration in an embedding
	// that never configured takeover would silently forfeit conversation
	// lineage takeover. Warn prominently, once per process (production always
	// configures WithConversationTakeover).
	if !c.takeover && reg.Facts.ConversationRef != nil && *reg.Facts.ConversationRef != "" {
		c.takeoverWarnOnce.Do(func() {
			c.reportError(ctx, fmt.Errorf("sessioncoord: session %s carries a conversation ref but conversation takeover is not configured (WithConversationTakeover); duplicate retained rows will not be taken over", id))
		})
	}

	// Publish committed outcome outside the mutex so a blocking or
	// re-entrant dirty sink cannot stall lifecycle transitions.
	c.publish(ctx, outcome)
	session.ID = id // the store echoes it; make the outcome self-describing even for sparse fakes
	c.emitUpserted(session)
	if len(reg.Evict) > 0 {
		evicted := make([]centralstore.SessionID, len(reg.Evict))
		for i, ev := range reg.Evict {
			evicted[i] = ev.ID
		}
		c.emitOutcomes(ctx, evicted...)
	}
	return runtime, nil
}

func reduce(reg *centralstore.RunnerRegistration, ev RunnerEvent) {
	_ = mergeFacts(&reg.Facts, ev.Facts)
	if ev.Alive != nil {
		reg.Alive = *ev.Alive
	}
	if ev.ObservedAt > reg.ObservedAt {
		reg.ObservedAt = ev.ObservedAt
	}
}

// mergeFacts overlays only fields represented by an event.
func mergeFacts(dst *centralstore.RunnerFacts, src centralstore.RunnerFacts) error {
	if src.ConversationRef != nil {
		dst.ConversationRef = src.ConversationRef
	}
	if src.CWD != nil {
		dst.CWD = src.CWD
	}
	if src.WorkspaceRoot != nil {
		dst.WorkspaceRoot = src.WorkspaceRoot
	}
	if src.Slug != nil {
		dst.Slug = src.Slug
	}
	if src.ShellTitle != nil {
		dst.ShellTitle = src.ShellTitle
	}
	if src.AdapterTitle != nil {
		dst.AdapterTitle = src.AdapterTitle
	}
	if src.Subtitle != nil {
		dst.Subtitle = src.Subtitle
	}
	if src.Command != nil {
		dst.Command = src.Command
	}
	if src.Remotes != nil {
		dst.Remotes = src.Remotes
	}
	if src.Working != nil {
		dst.Working = src.Working
	}
	if src.Unread != nil {
		dst.Unread = src.Unread
	}
	if src.Error != nil {
		dst.Error = src.Error
	}
	if src.StartedAt.Set != nil || src.StartedAt.Clear {
		dst.StartedAt = src.StartedAt
	}
	if src.ExitedAt.Set != nil || src.ExitedAt.Clear {
		dst.ExitedAt = src.ExitedAt
	}
	if src.ExitCode.Set != nil || src.ExitCode.Clear {
		dst.ExitCode = src.ExitCode
	}
	if src.TerminalSize.Set != nil || src.TerminalSize.Clear {
		dst.TerminalSize = src.TerminalSize
	}
	return nil
}

// drain reads events for a registered generation until the stream closes or
// the context is canceled. It runs in its own goroutine. On exit it removes
// the generation entry and cancels the stream.
//
// The drain loop does not hold the lifecycle mutex. Registry operations use
// the registry's own lock. The generation check in registry.advance and
// registry.remove prevents stale-generation writes from reaching the store.
func (c *Coordinator) drain(ctx context.Context, id centralstore.SessionID, generation uint64, events <-chan RunnerEvent) {
	// exitObserved is an optimization only: it avoids a pointless synthesized
	// apply after a stream that already carried exit facts (whose apply
	// removed the entry). Correctness never depends on it — a synthesized
	// exit for a generation that is no longer installed is dropped by apply's
	// generation check regardless.
	exitObserved := false
	defer func() {
		// Remove and close this generation's entry if still current. If
		// apply already removed it (Alive=false event) remove returns false
		// and closeEntry is not called — no double-cancel.
		if e, ok := c.registry.remove(id, generation); ok {
			closeEntry(e)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Mid-life stream drop. A generation's death must always land
				// durably: if no exit fact was observed on this stream,
				// synthesize one (same contract as the fast-dead registration
				// synthesis and the startup convergence sweep: explicit exit
				// timestamp, no exit code, turn state preserved). Routing it
				// through apply gives it the ordinary fence-wait, generation
				// check, stale retry, commit-before-liveness-removal ordering,
				// and post-commit publish — so a drop of a replaced generation
				// can never write onto the replacement's row.
				if !exitObserved {
					at := c.now()
					alive := false
					c.apply(ctx, id, generation, RunnerEvent{
						ObservedAt: at,
						Alive:      &alive,
						Facts:      centralstore.RunnerFacts{ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &at}},
					})
				}
				return
			}
			if ev.Facts.ExitedAt.Set != nil {
				exitObserved = true
			}
			c.apply(ctx, id, generation, ev)
		}
	}
}

// apply persists one ordered event for the given generation. It does not hold
// the lifecycle mutex across the DB call or the dirty sink. On ErrStaleVersion
// it advances the local row-version token and retries: once for ordinary
// events, and up to three times for exit-carrying events (an exit fact or a
// death), mirroring ensureDurableExit's budget — a generation's death must
// always land durably, and version churn alone must not drop it. Retry
// exhaustion is reported to the ErrorSink; the row remains repairable by
// Stop's ensureDurableExit or the startup sweep. Malformed exit events
// (Alive=false without ExitedAt) are reported but still remove liveness so
// the registry cannot remain stuck.
func (c *Coordinator) apply(ctx context.Context, id centralstore.SessionID, generation uint64, ev RunnerEvent) {
	// Read the current RowVersion under the registry's own lock. No
	// lifecycle mutex needed on the fast path.
	e, ok := c.registry.current(id)
	if !ok {
		if !c.registry.fenced(id) {
			return
		}
		// The entry is fenced: a replacement is inside its commit-to-install
		// window. Fence resolution is bounded by the lifecycle mutex —
		// Register either restores the old generation (failed replacement)
		// or installs the new one before unlocking — so briefly acquiring it
		// waits out the window, then re-check. If the restore made this
		// generation current again the event must still commit; a failed
		// replacement must not silently drop in-flight observations of the
		// still-installed old generation.
		c.mu.Lock()
		c.mu.Unlock() //nolint:staticcheck // empty critical section is the point
		e, ok = c.registry.current(id)
		if !ok {
			return
		}
	}
	if e.Generation != generation {
		return
	}

	obs := centralstore.RunnerObservation{
		ID:              id,
		ObservedVersion: e.RowVersion,
		ObservedAt:      ev.ObservedAt,
		Facts:           ev.Facts,
	}

	staleRetries := 1
	if ev.Facts.ExitedAt.Set != nil || (ev.Alive != nil && !*ev.Alive) {
		staleRetries = 3
	}
	result, err := c.durable.ApplyRunnerObservation(ctx, obs)
	for retry := 0; err != nil && errors.Is(err, centralstore.ErrStaleVersion) && result.SessionVersion > 0 && retry < staleRetries; retry++ {
		// The store returned the current version. Advance the local token and
		// retry with the refreshed version.
		c.registry.advance(id, generation, result.SessionVersion)
		e2, ok2 := c.registry.current(id)
		if !ok2 || e2.Generation != generation {
			return // generation replaced while retrying
		}
		obs.ObservedVersion = e2.RowVersion
		result, err = c.durable.ApplyRunnerObservation(ctx, obs)
	}
	if err != nil {
		c.reportError(ctx, fmt.Errorf("sessioncoord: observation failed for session %s gen %d: %w", id, generation, err))
		return
	}

	if !c.registry.advance(id, generation, result.SessionVersion) {
		// Generation replaced (or fenced) while the DB call was in flight. The
		// commit still happened, so publish it: invalidation is
		// level-triggered, making publication of a committed outcome always
		// safe, and it must not depend on the replacement's own publish.
		c.publish(ctx, result)
		if result.Changed {
			c.emitOutcomes(ctx, id)
		}
		return
	}

	// Handle exit event. Alive=false must carry ExitedAt to be well-formed.
	// A malformed event is reported but does not prevent liveness removal
	// so the registry cannot remain stuck.
	if ev.Alive != nil && !*ev.Alive {
		if ev.Facts.ExitedAt.Set == nil {
			c.reportError(ctx, fmt.Errorf("sessioncoord: malformed exit event: Alive=false without ExitedAt for session %s gen %d", id, generation))
		}
		// Exit facts committed (advance succeeded) before liveness removed.
		// Canceling the stream propagates to the bufferEvents goroutine and
		// the drain context, causing drain to exit its select loop.
		if removed, yes := c.registry.remove(id, generation); yes {
			closeEntry(removed)
		}
	}

	// Publish outside all locks so a blocking or re-entrant sink cannot
	// stall the coordinator or deadlock.
	c.publish(ctx, result)
	if result.Changed {
		c.emitOutcomes(ctx, id)
	}
}

// invalidateVerdict clears a reconciliation verdict and, while a reconcile
// pass is in flight, records the invalidation so that pass cannot re-set a
// stale verdict. Caller must hold c.mu.
func (c *Coordinator) invalidateVerdict(id centralstore.SessionID) {
	delete(c.verdicts, id)
	if c.verdictsInvalidated != nil {
		c.verdictsInvalidated[id] = true
	}
}

func (c *Coordinator) publish(ctx context.Context, r centralstore.MutationResult) {
	if c.dirty != nil && r.Changed {
		c.dirty.Committed(ctx, r)
	}
}

func (c *Coordinator) reportError(ctx context.Context, err error) {
	if c.errSink != nil {
		c.errSink.Error(ctx, err)
	}
}
