package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

var ErrGenerationActive = errors.New("sessioncoord: a working generation is already installed")

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
}

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
}

type Coordinator struct {
	mu       sync.Mutex
	next     uint64
	registry *Registry
	runners  RunnerClient
	durable  Durable
	dirty    DirtySink
	errSink  ErrorSink
}

func New(registry *Registry, runners RunnerClient, durable Durable, dirty DirtySink, errSink ErrorSink) *Coordinator {
	if registry == nil {
		registry = NewRegistry()
	}
	return &Coordinator{registry: registry, runners: runners, durable: durable, dirty: dirty, errSink: errSink}
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

	// ── Phase 2: lifecycle mutex ──────────────────────────────────────────
	// Holding the mutex across the short DB transaction is acceptable.
	// Runner I/O (Subscribe, Meta) must not run inside this section.

	c.mu.Lock()

	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return Runtime{}, err
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

	runtime := Runtime{
		SessionID:     id,
		Generation:    generation,
		Endpoint:      req.Endpoint,
		PID:           meta.PID,
		RunnerVersion: meta.RunnerVersion,
		BinaryHash:    meta.BinaryHash,
		Subscribed:    reg.Alive && !closed,
		RowVersion:    session.Version,
	}

	if runtime.Subscribed {
		// Start the bounded buffer goroutine from stream.Events() now that
		// we've finished the synchronous pre-registration drain.
		events := bufferEvents(streamCtx, drainCh)
		installMu.Lock()
		installed = true
		installMu.Unlock()
		replacedEntry, replaced := c.registry.install(registryEntry{Runtime: runtime, cancel: streamCancel, stream: stream})
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

	// Publish committed outcome outside the mutex so a blocking or
	// re-entrant dirty sink cannot stall lifecycle transitions.
	c.publish(ctx, outcome)
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
				return
			}
			c.apply(ctx, id, generation, ev)
		}
	}
}

// apply persists one ordered event for the given generation. It does not hold
// the lifecycle mutex across the DB call or the dirty sink. On ErrStaleVersion
// it advances the local row-version token and retries once. Malformed exit
// events (Alive=false without ExitedAt) are reported but still remove liveness
// so the registry cannot remain stuck.
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

	result, err := c.durable.ApplyRunnerObservation(ctx, obs)
	if err != nil {
		if errors.Is(err, centralstore.ErrStaleVersion) && result.SessionVersion > 0 {
			// The store returned the current version. Advance the local
			// token and retry once with the refreshed version.
			c.registry.advance(id, generation, result.SessionVersion)
			if e2, ok2 := c.registry.current(id); ok2 && e2.Generation == generation {
				obs.ObservedVersion = e2.RowVersion
				result, err = c.durable.ApplyRunnerObservation(ctx, obs)
			} else {
				return // generation replaced while retrying
			}
		}
		if err != nil {
			c.reportError(ctx, fmt.Errorf("sessioncoord: observation failed for session %s gen %d: %w", id, generation, err))
			return
		}
	}

	if !c.registry.advance(id, generation, result.SessionVersion) {
		// Generation replaced (or fenced) while the DB call was in flight. The
		// commit still happened, so publish it: invalidation is
		// level-triggered, making publication of a committed outcome always
		// safe, and it must not depend on the replacement's own publish.
		c.publish(ctx, result)
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
