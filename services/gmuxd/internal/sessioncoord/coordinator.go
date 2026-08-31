package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

var (
	ErrGenerationActive = errors.New("sessioncoord: a working generation is already installed")
	// ErrSessionIDExists rejects a direct new-runner registration whose
	// client-minted ID is already durable. Resume/restart replacements carry
	// lifecycle provenance and are exempt.
	ErrSessionIDExists = errors.New("sessioncoord: session id already exists")
	// ErrRunnerTransportNoncompliant means Subscribe returned a stream after
	// its lifetime context had already been cancelled. Such a transport can
	// neither establish the subscribe-before-meta fence nor satisfy bounded
	// bootstrap shutdown, so callers must withhold readiness.
	ErrRunnerTransportNoncompliant = errors.New("sessioncoord: runner transport ignored cancellation")
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
	ErrReplaceWithoutClaim      = errors.New("sessioncoord: replace provenance requires a held lifecycle claim")
	ErrAssertedIdentityMismatch = errors.New("sessioncoord: asserted session id does not match runner meta")
	// ErrRunnerIncarnationMismatch marks a registration whose subscription and
	// metadata were served by different runner processes. An endpoint is a
	// reusable pathname, so two calls to it are two calls to a location, not
	// to a process: without agreeing incarnations there is no evidence that
	// the stream being installed belongs to the runner whose facts are being
	// committed. Registration aborts before any commit, fence or registry
	// change; the next discovery pass converges whoever actually owns the
	// endpoint.
	ErrRunnerIncarnationMismatch = errors.New("sessioncoord: subscription and metadata came from different runners")
)

// EventStream is already ordered by the runner transport. Subscribe must not
// return until the subscription is established, so replay/live events can be
// buffered before Meta starts.
type EventStream interface {
	Events() <-chan RunnerEvent
	Close() error
}

// StreamIncarnation is an optional EventStream capability: a transport that
// learned which runner answered the subscription reports it here.
//
// It is optional rather than part of EventStream so that a transport, a test
// double, or a runner from before the incarnation protocol can simply not
// implement it. Absence means "unknown", which every consumer resolves in its
// own conservative direction.
type StreamIncarnation interface {
	// Incarnation returns the ephemeral identity of the runner that served
	// this stream, or the empty string when the transport could not learn it.
	Incarnation() string
}

// streamIncarnation extracts a stream's incarnation, or the empty string.
func streamIncarnation(stream EventStream) string {
	if si, ok := stream.(StreamIncarnation); ok {
		return si.Incarnation()
	}
	return ""
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
	// Incarnation is the ephemeral identity of the runner process that served
	// this response, or empty for a runner that predates the protocol. It is
	// never persisted: it exists to tell one process from its own successor at
	// the same endpoint, which is a question about right now.
	Incarnation string
}

type RunnerEvent struct {
	ObservedAt        centralstore.UnixMillis
	Facts             centralstore.RunnerFacts
	Alive             *bool
	TransientActivity bool // lossy signal; never persisted as a fact
	// Frame carries a runner-asserted turn frame. It is runtime-only — never a
	// durable fact and never a store write — and is retained in registry
	// runtime for the generation that sent it.
	//
	// A turn EDGE carries the frame together with its status facts in one event,
	// so a close and the result it asserted cannot be separated in transit; the
	// frame is retained before those facts are applied, which is what lets the
	// close's own outcome carry that frame.
	Frame *TurnFrame
	// FrameOnly marks an event that carries nothing but a frame (a mid-turn
	// injection, a rebind clear, a replay snapshot). It is retained and NOT
	// applied: a durable observation for it would churn the row version for a
	// fact the store does not hold.
	FrameOnly bool
}

type RegisterRequest struct {
	Endpoint string
	// ActiveSubagentReservation is the opaque receipt issued before a
	// gmux-mediated agent launch. Empty preserves ordinary registration.
	ActiveSubagentReservation string
	// AssertedID is caller-supplied identity for ordinary registration. Unlike
	// ExpectedID it carries no replacement provenance and needs no claim.
	AssertedID centralstore.SessionID
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

	// beforeDismissLock and beforeReparentLock are test-only synchronization
	// seams: when set, they are called immediately before the operation attempts
	// the lifecycle mutex, letting serialization tests deterministically observe
	// "blocked at the mutex".
	beforeDismissLock  func()
	beforeReparentLock func()

	// outcomes is the post-commit outcome bus (see outcomes.go). It has its
	// own lock; publishing never runs under the lifecycle mutex.
	outcomes outcomeBus

	// activeSubagents is the host-local admission projection for gmux-mediated
	// semantic-agent launches. It is guarded by mu and derives liveness from
	// installed registry generations, never durable active/status flags.
	activeSubagents *activeSubagentBudget

	// semanticSession classifies durable rows for successful process-exit
	// supervision. Nil disables the policy (useful for embedders and old tests).
	semanticSession func(centralstore.Session) bool

	// Startup convergence barrier state (see convergence.go). Guarded by mu.
	convergeCandidates map[centralstore.SessionID]struct{}
	convergeClosed     bool
	converged          chan struct{}

	closing bool
	drains  sync.WaitGroup
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

// WithActiveSubagentBudget enables host-local launch admission. initial is a
// durable ownership snapshot; runtime liveness is populated as surviving
// runners register during startup convergence.
func WithActiveSubagentBudget(limits []int, disabled bool, semantic func(string) bool, initial []centralstore.Session) Option {
	return func(c *Coordinator) {
		c.activeSubagents = newActiveSubagentBudget(limits, disabled, semantic, initial)
	}
}

// WithProcessChildAutoRead enables successful full-exit supervision. The
// classifier is the server's semantic capability boundary; semantic sessions
// always retain strict attention.
func WithProcessChildAutoRead(semantic func(centralstore.Session) bool) Option {
	return func(c *Coordinator) { c.semanticSession = semantic }
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
	var (
		launchReservation   activeSubagentLaunch
		reservationClaimed  bool
		reservationConsumed bool
	)
	if req.ActiveSubagentReservation != "" {
		c.mu.Lock()
		if c.activeSubagents == nil {
			c.mu.Unlock()
			return Runtime{}, ErrActiveSubagentReservationInvalid
		}
		var err error
		launchReservation, err = c.activeSubagents.claim(req.ActiveSubagentReservation, req.AssertedID)
		c.mu.Unlock()
		if err != nil {
			return Runtime{}, err
		}
		reservationClaimed = true
		defer func() {
			if reservationConsumed {
				return
			}
			// Phase 2's panic guard releases c.mu before re-panicking, so this
			// cleanup can block behind ordinary lifecycle traffic without ever
			// self-deadlocking or silently stranding a claimed receipt.
			c.mu.Lock()
			c.activeSubagents.unclaim(req.ActiveSubagentReservation)
			c.mu.Unlock()
		}()
	}

	// ── Phase 1: runner I/O outside the lifecycle mutex ──────────────────
	//
	// streamCtx governs the installed stream for its full lifetime. During
	// setup we propagate ctx cancellation into streamCtx so a canceled
	// request aborts a hung Subscribe without leaking the goroutine or the
	// stream. Once the entry is installed (setupDone closed without cancel)
	// streamCtx is detached from ctx.

	// Capture the endpoint's physical identity before any runner I/O, so the
	// installed Runtime describes the socket this registration actually talked
	// to rather than whatever the pathname names once the dust settles. It is
	// re-checked at install time (settledIdent).
	preSocket := socketIdent(req.Endpoint)

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
	if streamCtx.Err() != nil {
		if stream != nil {
			_ = stream.Close()
		}
		return Runtime{}, fmt.Errorf("%w: %v", ErrRunnerTransportNoncompliant, streamCtx.Err())
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
	// Both runner calls addressed a pathname, not a process. Require them to
	// report the same runner before treating either as evidence about the
	// other: a pathname can change hands between the two, and then the stream
	// we are about to install and the facts we are about to commit describe
	// different processes.
	//
	// Two empty incarnations mean neither call could identify its runner --
	// a pre-protocol runner, or a transport that does not carry the field.
	// That is not a mismatch, it is an absence of evidence, and it is handled
	// by refusing to claim an identity for the installed generation (below).
	subIncarnation := streamIncarnation(stream)
	if subIncarnation != meta.Incarnation {
		return Runtime{}, fmt.Errorf("%w: subscription %q, metadata %q",
			ErrRunnerIncarnationMismatch, subIncarnation, meta.Incarnation)
	}
	id := meta.Registration.ID
	if !paths.IsValidSessionID(string(id)) {
		return Runtime{}, fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	if req.AssertedID != "" && id != req.AssertedID {
		_ = stream.Close()
		return Runtime{}, fmt.Errorf("%w: asserted %s, runner reported %s", ErrAssertedIdentityMismatch, req.AssertedID, id)
	}
	if req.ExpectedID != "" && id != req.ExpectedID {
		// Abort before the mutex/fence/commit: no registration side effects.
		return Runtime{}, fmt.Errorf("%w: expected %s, runner reported %s", ErrResumeIdentityMismatch, req.ExpectedID, id)
	}
	if reservationClaimed {
		if err := c.activeSubagents.validateParent(launchReservation, meta.Registration.ParentSessionID); err != nil {
			return Runtime{}, err
		}
	}

	// Ordinary discovery has now verified the endpoint's claimed identity.
	// Reject an already-installed generation before durable reads or lineage
	// resolver I/O. A different endpoint claiming the same ID remains visible
	// as ErrGenerationActive; replacement operations bypass this check and are
	// authorized by their lifecycle claim at the commit fence below.
	if !req.Replace && req.ExpectedID == "" {
		c.mu.Lock()
		if c.closing {
			c.mu.Unlock()
			return Runtime{}, errors.New("sessioncoord: coordinator closed")
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return Runtime{}, err
		}
		old, hadOld := c.registry.current(id)
		if hadOld && req.AssertedID != "" && old.Endpoint == req.Endpoint && meta.Incarnation != "" && old.Incarnation == meta.Incarnation {
			// Discovery may have observed this runner between its bind and
			// direct registration. Treat the later assertion as an idempotent
			// acknowledgement only with process-level proof, and consume the
			// launch receipt so it no longer double-counts the installed child.
			if reservationClaimed {
				c.activeSubagents.release(req.ActiveSubagentReservation, true)
				reservationConsumed = true
			}
			c.mu.Unlock()
			_ = stream.Close()
			return old.Runtime, nil
		}
		c.mu.Unlock()
		if hadOld {
			if req.AssertedID != "" {
				return old.Runtime, fmt.Errorf("%w: %s", ErrSessionIDExists, id)
			}
			return old.Runtime, ErrGenerationActive
		}
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
	phase2Locked := true
	phase2Unlock := func() {
		phase2Locked = false
		c.mu.Unlock()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if phase2Locked {
				c.mu.Unlock()
			}
			panic(recovered)
		}
	}()

	if c.closing {
		phase2Unlock()
		return Runtime{}, errors.New("sessioncoord: coordinator closed")
	}
	if err := ctx.Err(); err != nil {
		phase2Unlock()
		return Runtime{}, err
	}

	if req.Replace || req.ExpectedID != "" {
		if req.Claim == nil || c.ops[id] != req.Claim {
			phase2Unlock()
			return Runtime{}, fmt.Errorf("%w: %s", ErrReplaceWithoutClaim, id)
		}
	}
	if reservationClaimed {
		if err := c.activeSubagents.validateClaimedBudget(launchReservation); err != nil {
			phase2Unlock()
			return Runtime{}, err
		}
	}

	// A direct /v1/register assertion is a freshly minted identity, unlike
	// discovery (no assertion) and claimed resume/restart replacement. Reject
	// durable reuse under the lifecycle mutex; RegisterRunner's primary key
	// handling remains the authoritative transaction boundary.
	if req.AssertedID != "" && !req.Replace && req.ExpectedID == "" && req.Claim == nil {
		if _, exists, rowErr := c.durable.Session(ctx, id); rowErr != nil {
			phase2Unlock()
			return Runtime{}, rowErr
		} else if exists {
			phase2Unlock()
			return Runtime{}, fmt.Errorf("%w: %s", ErrSessionIDExists, id)
		}
	}

	// Re-check at the commit fence: two genuine registrations may both pass
	// the pre-I/O absence check, but only one generation may install.
	old, hadOld := c.registry.current(id)
	if hadOld && !req.Replace {
		idempotent := req.AssertedID != "" && old.Endpoint == req.Endpoint && meta.Incarnation != "" && old.Incarnation == meta.Incarnation
		if idempotent && reservationClaimed {
			c.activeSubagents.release(req.ActiveSubagentReservation, true)
			reservationConsumed = true
		}
		phase2Unlock()
		if req.AssertedID != "" {
			if idempotent {
				_ = stream.Close()
				return old.Runtime, nil
			}
			return old.Runtime, fmt.Errorf("%w: %s", ErrSessionIDExists, id)
		}
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
	// replayedFrame carries the turn frame out of this pre-registration window and
	// into the installed Runtime.
	//
	// It is load-bearing for every RECONNECT: the runner's connect-time replay is
	// the first thing on a fresh stream, so the frame describing the turn that is
	// running right now lands HERE, before any drain goroutine exists. reduce()
	// folds durable facts only — a frame is runtime state, not a fact — so without
	// this the entry would install with no frame at all, and a wait armed in the
	// reconnect window would learn no turn_seq and resolve result-free even though
	// the runner told us everything we needed.
	var replayedFrame *TurnFrame
	drainCh := stream.Events()
loop:
	for {
		select {
		case ev, ok := <-drainCh:
			if !ok {
				closed = true
				break loop
			}
			if ev.Frame != nil {
				replayedFrame = ev.Frame // newest wins, like every other frame apply
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
				phase2Unlock()
				return Runtime{}, rowErr
			}
			if !exists {
				phase2Unlock()
				return Runtime{}, fmt.Errorf("%w: %s", ErrConversationOwnedByLive, id)
			}
		}
	}

	// A runner can finish between Subscribe and the first durable commit. Give
	// that fused fast-dead result the same lifecycle-serialized supervision
	// decision as an ordinary exit event. Durable state supplies the current
	// organizational parent for existing rows; launch metadata is used only for
	// a genuinely new row.
	var registrationPolicyErr error
	if successfulFastDeadRegistrationCandidate(reg) && c.semanticSession != nil {
		child := centralstore.Session{ID: id, Adapter: reg.Adapter, DriveMode: reg.DriveMode, ParentSessionID: reg.ParentSessionID}
		if existing, exists, readErr := c.durable.Session(ctx, id); readErr != nil {
			registrationPolicyErr = fmt.Errorf("sessioncoord: read fast-dead registration policy state for %s: %w", id, readErr)
		} else {
			if exists {
				child = existing
			}
			reg.SuppressUnread = c.supervisedSuccessfulExit(child, reg.Facts)
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
	if errors.Is(err, centralstore.ErrSuppressionWouldClearUnread) {
		reg.SuppressUnread = false
		session, outcome, err = c.durable.RegisterRunner(ctx, reg)
	}
	if err != nil {
		if hadOld {
			c.registry.restore(id, old.Generation)
		}
		phase2Unlock()
		return Runtime{}, err
	}

	// Any committed registration invalidates reconciliation verdicts: the
	// merged facts (possibly a new conversation ref) supersede the probe that
	// produced them, and evicted losers no longer exist.
	c.invalidateVerdict(id)
	for _, ev := range reg.Evict {
		c.invalidateVerdict(ev.ID)
	}

	// Settle this generation's endpoint identity.
	//
	// Two independent facts have to hold before the installed Runtime may
	// claim to know which socket it is subscribed to:
	//
	//  1. The pathname named the same inode before Subscribe and after the
	//     commit (settledIdent). Without this, a pathname rebound mid-flight
	//     would be recorded as ours.
	//  2. Both runner calls reported the same, known incarnation. Stat
	//     bracketing alone cannot establish this: a bound AF_UNIX node can be
	//     hard-linked, so an inode can leave a pathname and come back to it
	//     (A -> B -> A) with every stat agreeing while the two calls reached
	//     different runners.
	//
	// Fail either and the identity is unknown, which suppresses nothing and
	// authorises nothing.
	socketIdent := settledIdent(preSocket, req.Endpoint)
	if meta.Incarnation == "" {
		socketIdent = socklease.Ident{}
	}

	runtime := Runtime{
		SessionID:     id,
		Generation:    generation,
		Endpoint:      req.Endpoint,
		Socket:        socketIdent,
		Incarnation:   meta.Incarnation,
		PID:           meta.PID,
		RunnerVersion: meta.RunnerVersion,
		BinaryHash:    meta.BinaryHash,
		// The frame the runner replayed during the pre-registration drain (nil for
		// a session that has asserted no turn, or a runner too old to send one).
		Frame: replayedFrame,
		// A closed pre-drain stream already forced reg.Alive=false above, so
		// liveness alone decides subscription.
		Subscribed: reg.Alive,
		RowVersion: session.Version,
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
		c.drains.Add(1)
		go func() {
			defer c.drains.Done()
			c.drain(streamCtx, id, generation, events)
		}()
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

	if c.activeSubagents != nil {
		// Project committed takeover outcomes, not requested candidates:
		// version-conditional evictions may be skipped. This is O(evictions),
		// normally zero, and never rebuilds the global graph per launch.
		for _, candidate := range reg.Evict {
			row, exists, readErr := c.durable.Session(ctx, candidate.ID)
			if readErr != nil {
				// Registration is already committed and installed. Preserve the
				// pre-commit projection conservatively; a later reconciliation or
				// restart converges it, and never turn success into a retryable error.
				c.reportError(ctx, fmt.Errorf("sessioncoord: refresh takeover candidate %s: %w", candidate.ID, readErr))
				continue
			}
			if exists {
				_, live := c.registry.current(candidate.ID)
				c.activeSubagents.upsert(row, live)
			} else {
				c.activeSubagents.remove(candidate.ID)
			}
		}
		session.ID = id // sparse durable fakes need the identity projected here
		c.activeSubagents.upsert(session, runtime.Subscribed)
		if reservationClaimed {
			c.activeSubagents.release(req.ActiveSubagentReservation, true)
			reservationConsumed = true
		}
	}

	seq := c.outcomes.allocSeq() // stamp commit order before releasing c.mu
	phase2Unlock()
	if registrationPolicyErr != nil {
		c.reportError(ctx, registrationPolicyErr)
	}

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
	c.emitUpsertedWithAttention(session, seq, reg.SuppressUnread)
	if len(reg.Evict) > 0 {
		evicted := make([]centralstore.SessionID, len(reg.Evict))
		for i, ev := range reg.Evict {
			evicted[i] = ev.ID
		}
		c.emitOutcomes(ctx, seq, evicted...)
	}
	return runtime, nil
}

func reduce(reg *centralstore.RunnerRegistration, ev RunnerEvent) {
	// Buffered events are folded into the registration snapshot before the
	// first durable commit. Preserve the conversation boundary while doing so:
	// if /meta described A and a later replay/live event rebinds to B, A's
	// title/subtitle/slug facts must not survive beside B's ref in the flattened
	// registration. Later metadata events for B can populate them again.
	if ev.Facts.ConversationRef != nil && *ev.Facts.ConversationRef != "" &&
		reg.Facts.ConversationRef != nil && *reg.Facts.ConversationRef != "" &&
		*ev.Facts.ConversationRef != *reg.Facts.ConversationRef {
		reg.Facts.AdapterTitle = nil
		reg.Facts.Subtitle = nil
		reg.Facts.Slug = nil
	}
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
	if src.Active != nil {
		dst.Active = src.Active
	}
	if src.Interrupted != nil {
		dst.Interrupted = src.Interrupted
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
// Ordinary observations use only the registry lock. A successful full-exit
// candidate holds the lifecycle mutex from supervision decision through its
// durable commit and registry removal; publication and stream teardown occur
// after unlock. Generation checks prevent stale writes from taking ownership.
// Close prevents new installed streams, cancels every current stream, and
// joins all drain workers. Cancellation never synthesizes runner death.
func (c *Coordinator) Close() {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		c.drains.Wait()
		return
	}
	c.closing = true
	entries := c.registry.removeAll()
	for _, e := range entries {
		closeEntry(e)
	}
	c.mu.Unlock()
	c.drains.Wait()
}

func (c *Coordinator) drain(ctx context.Context, id centralstore.SessionID, generation uint64, events <-chan RunnerEvent) {
	// exitObserved is an optimization only: it avoids a pointless synthesized
	// apply after a stream that already carried exit facts (whose apply
	// removed the entry). Correctness never depends on it — a synthesized
	// exit for a generation that is no longer installed is dropped by apply's
	// generation check regardless.
	exitObserved := false
	defer func() {
		// Remove and close this generation's entry if still current. Serialize
		// the registry edge with budget liveness so admission observes either
		// the occupied slot or the released slot, never a split state.
		c.mu.Lock()
		e, ok := c.registry.remove(id, generation)
		if ok && c.activeSubagents != nil {
			c.activeSubagents.setLive(id, false)
		}
		c.mu.Unlock()
		if ok {
			closeEntry(e)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Coordinator shutdown owns cancellation and must not manufacture
				// runner death merely because it closed the local stream.
				if ctx.Err() != nil {
					return
				}
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
			if ev.Frame != nil {
				// Retain BEFORE applying this event's facts: a turn edge's
				// status commit publishes an outcome stamped with the frame
				// retained here (see frameOf), which is how a waiter resolving
				// the close reads the result asserted for that exact turn.
				c.registry.setFrame(id, generation, ev.Frame)
				if ev.FrameOnly {
					// Nothing durable to apply — but an armed wait may have to
					// resolve on it: a mid-turn injection changes what the turn's
					// answer means and interrupts every other waiter (ADR 0027,
					// "Steering interrupts waits"). It writes no row, so no domain
					// outcome announces it; publish the frame as a transient signal
					// so waiters see it without polling.
					c.publishFrameSignal(id, ev.Frame)
					continue
				}
			}
			if ev.TransientActivity {
				// The runner protocol's activity frame is explicitly transient;
				// publish it without manufacturing a durable mutation.
				c.PublishActivity(id)
				continue
			}
			if ev.Facts.ExitedAt.Set != nil {
				exitObserved = true
			}
			c.apply(ctx, id, generation, ev)
		}
	}
}

// apply persists one ordered event for the given generation. Successful full
// exit candidates hold the lifecycle mutex through the policy decision and DB
// commit; publication and stream teardown always occur after unlock. On ErrStaleVersion
// it advances the local row-version token and retries: once for ordinary
// events, and up to three times for exit-carrying events (an exit fact or a
// death), mirroring ensureDurableExit's budget — a generation's death must
// always land durably, and version churn alone must not drop it. Retry
// exhaustion is reported to the ErrorSink; the row remains repairable by
// Stop's ensureDurableExit or the startup sweep. Malformed exit events
// (Alive=false without ExitedAt) are reported but still remove liveness so
// the registry cannot remain stuck.
func (c *Coordinator) apply(ctx context.Context, id centralstore.SessionID, generation uint64, ev RunnerEvent) {
	candidate := successfulFullExitCandidate(ev) && c.semanticSession != nil
	lifecycleLocked := candidate
	if lifecycleLocked {
		c.mu.Lock()
	}
	unlock := func() {
		if lifecycleLocked {
			c.mu.Unlock()
			lifecycleLocked = false
		}
	}
	defer unlock()

	e, ok := c.registry.current(id)
	if !ok {
		if !c.registry.fenced(id) {
			return
		}
		// Waiting on c.mu resolves a replacement's commit-to-install fence.
		if !lifecycleLocked {
			c.mu.Lock()
			c.mu.Unlock() //nolint:staticcheck // empty critical section is the point
		}
		e, ok = c.registry.current(id)
		if !ok {
			return
		}
	}
	if e.Generation != generation {
		return
	}

	obs := centralstore.RunnerObservation{ID: id, ObservedVersion: e.RowVersion, ObservedAt: ev.ObservedAt, Facts: ev.Facts}
	var deferredErrors []error
	if candidate {
		if row, exists, readErr := c.durable.Session(ctx, id); readErr != nil {
			deferredErrors = append(deferredErrors, fmt.Errorf("sessioncoord: read successful exit policy state for %s: %w", id, readErr))
		} else if exists {
			obs.ObservedVersion = row.Version
			obs.SuppressUnread = c.supervisedSuccessfulExit(row, ev.Facts)
		}
	}

	staleRetries := 1
	if ev.Facts.ExitedAt.Set != nil || (ev.Alive != nil && !*ev.Alive) {
		staleRetries = 3
	}
	result, err := c.durable.ApplyRunnerObservation(ctx, obs)
	if errors.Is(err, centralstore.ErrSuppressionWouldClearUnread) {
		obs.SuppressUnread = false
		result, err = c.durable.ApplyRunnerObservation(ctx, obs)
	}
	for retry := 0; err != nil && errors.Is(err, centralstore.ErrStaleVersion) && result.SessionVersion > 0 && retry < staleRetries; retry++ {
		c.registry.advance(id, generation, result.SessionVersion)
		e2, ok2 := c.registry.current(id)
		if !ok2 || e2.Generation != generation {
			unlock()
			for _, deferredErr := range deferredErrors {
				c.reportError(ctx, deferredErr)
			}
			return
		}
		obs.ObservedVersion = e2.RowVersion
		obs.SuppressUnread = false
		if candidate {
			if row, exists, readErr := c.durable.Session(ctx, id); readErr != nil {
				deferredErrors = append(deferredErrors, fmt.Errorf("sessioncoord: re-read successful exit policy state for %s: %w", id, readErr))
			} else if exists {
				obs.ObservedVersion = row.Version
				obs.SuppressUnread = c.supervisedSuccessfulExit(row, ev.Facts)
			}
		}
		result, err = c.durable.ApplyRunnerObservation(ctx, obs)
		if errors.Is(err, centralstore.ErrSuppressionWouldClearUnread) {
			obs.SuppressUnread = false
			result, err = c.durable.ApplyRunnerObservation(ctx, obs)
		}
	}

	// Exit liveness is removed even when persistence fails. The startup sweep
	// remains the repair authority; a dead process must never stay installed.
	if err != nil {
		var removed registryEntry
		var yes bool
		if ev.Alive != nil && !*ev.Alive {
			if !lifecycleLocked {
				c.mu.Lock()
			}
			removed, yes = c.registry.remove(id, generation)
			if yes && c.activeSubagents != nil {
				c.activeSubagents.setLive(id, false)
			}
			if !lifecycleLocked {
				c.mu.Unlock()
			}
		}
		unlock()
		for _, deferredErr := range deferredErrors {
			c.reportError(ctx, deferredErr)
		}
		c.reportError(ctx, fmt.Errorf("sessioncoord: observation failed for session %s gen %d: %w", id, generation, err))
		if yes {
			closeEntry(removed)
		}
		return
	}

	attentionSuppressed := obs.SuppressUnread
	if !c.registry.advance(id, generation, result.SessionVersion) {
		// A foreign generation may now own the row. Publish only untagged
		// invalidation; never attach this generation's suppression decision to a
		// replacement row.
		unlock()
		for _, deferredErr := range deferredErrors {
			c.reportError(ctx, deferredErr)
		}
		c.publish(ctx, result)
		if result.Changed {
			c.emitOutcomes(ctx, c.outcomes.allocSeq(), id)
		}
		return
	}

	var captured *centralstore.Session
	var seq uint64
	if candidate && attentionSuppressed && result.Changed {
		row, exists, readErr := c.durable.Session(ctx, id)
		if readErr != nil {
			deferredErrors = append(deferredErrors, fmt.Errorf("sessioncoord: capture supervised exit outcome for %s: %w", id, readErr))
		} else if exists && row.Version == result.SessionVersion {
			captured = &row
		}
	}
	if lifecycleLocked && result.Changed {
		seq = c.outcomes.allocSeq()
	}

	var removed registryEntry
	var removedOK bool
	if ev.Alive != nil && !*ev.Alive {
		if ev.Facts.ExitedAt.Set == nil {
			deferredErrors = append(deferredErrors, fmt.Errorf("sessioncoord: malformed exit event: Alive=false without ExitedAt for session %s gen %d", id, generation))
		}
		if !lifecycleLocked {
			c.mu.Lock()
		}
		removed, removedOK = c.registry.remove(id, generation)
		if removedOK && c.activeSubagents != nil {
			c.activeSubagents.setLive(id, false)
		}
		if !lifecycleLocked {
			c.mu.Unlock()
		}
	}
	unlock()

	for _, deferredErr := range deferredErrors {
		c.reportError(ctx, deferredErr)
	}
	c.publish(ctx, result)
	if captured != nil {
		c.outcomes.publish(Outcome{Type: OutcomeUpserted, ID: id, Session: captured, Sequence: seq, AttentionSuppressed: true})
	} else if result.Changed {
		if seq == 0 {
			seq = c.outcomes.allocSeq()
		}
		c.emitOutcomes(ctx, seq, id)
	}
	// Stream cancellation and body Close may block; neither belongs under the
	// lifecycle mutex, and publication above must retain a live context.
	if removedOK {
		closeEntry(removed)
	}
}

// successfulFastDeadRegistrationCandidate requires the result-bearing facts
// that distinguish a completed process from a merely disconnected runner.
// ExitedAt may have been synthesized above when a dead registration omitted
// it; that does not broaden the policy because exit code, unread=true, and a
// fresh token must still have been reported by the runner.
func successfulFastDeadRegistrationCandidate(reg centralstore.RunnerRegistration) bool {
	return !reg.Alive && reg.Facts.ExitedAt.Set != nil && reg.Facts.ExitCode.Set != nil &&
		*reg.Facts.ExitCode.Set == 0 && reg.Facts.Unread != nil && *reg.Facts.Unread &&
		reg.Facts.UnreadToken != nil && *reg.Facts.UnreadToken != ""
}

func successfulFullExitCandidate(ev RunnerEvent) bool {
	return ev.Alive != nil && !*ev.Alive && ev.Facts.ExitedAt.Set != nil &&
		ev.Facts.ExitCode.Set != nil && *ev.Facts.ExitCode.Set == 0 &&
		ev.Facts.Unread != nil && *ev.Facts.Unread &&
		ev.Facts.UnreadToken != nil && *ev.Facts.UnreadToken != ""
}

// supervisedSuccessfulExit runs only while c.mu is held. Registry membership
// is the authority for current local liveness; durable Alive/Active facts are
// deliberately irrelevant.
func (c *Coordinator) supervisedSuccessfulExit(child centralstore.Session, facts centralstore.RunnerFacts) bool {
	if child.Unread || child.Error || child.Interrupted ||
		(facts.Error != nil && *facts.Error) || (facts.Interrupted != nil && *facts.Interrupted) ||
		c.semanticSession(child) || child.ParentSessionID == nil {
		return false
	}
	_, alive := c.registry.current(*child.ParentSessionID)
	return alive
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

// socketIdent returns the current physical identity of an endpoint pathname,
// or the unknown identity when the pathname is not a filesystem socket (a
// synthetic test endpoint, a vanished pathname, or something that is not a
// socket at all).
func socketIdent(ep string) socklease.Ident {
	id, ok := socklease.StatSocket(ep)
	if !ok {
		return socklease.Ident{}
	}
	return id
}

// settledIdent returns the identity of ep only if it still matches the one
// observed earlier, and the unknown identity otherwise. "The pathname named
// the same socket before and after" is the strongest claim available without
// holding the runner's lease, which the daemon deliberately cannot take while
// the runner is alive.
//
// Residual: a pathname rebound to a new inode and back to a recycled one
// inside the window would compare equal. That needs an inode number to be
// reused within a single registration on the same device, and its
// consequence is a suppressed probe that the next rebind (a new inode)
// immediately un-suppresses.
func settledIdent(before socklease.Ident, ep string) socklease.Ident {
	if !before.Same(socketIdent(ep)) {
		return socklease.Ident{}
	}
	return before
}
