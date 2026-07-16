// Package central composes full snapshot payloads from the authoritative
// central store (ADR 0026). It is the nonproduction, query-backed successor
// to the store.Store-fed composition in internal/snapshot:
//
//   - every composition pass reads all requested payload inputs from ONE
//     SQLite read transaction (centralstore.ReadSnapshot), so a cross-kind
//     mutation can never yield a torn sessions/projects pair;
//   - invalidation is level-triggered and coalesced: commits mark kinds
//     dirty, one composer goroutine emits at most one in-flight composition,
//     and dirt arriving during a pass triggers another pass — no missed
//     updates, no unbounded queue;
//   - runner-live facts (alive, PID, endpoint, runner version/hash) are
//     overlaid from a point-in-time runtime view, never read from SQLite.
//
// The payload types mirror what the production SSE snapshot needs but are
// intentionally this package's own: conversion to the exact wire shape (and
// the peer-projection merge) happens at the production cutover.
package central

import (
	"context"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// Reader is the transaction-consistent durable read seam, satisfied by
// *centralstore.Store.
type Reader interface {
	ReadSnapshot(context.Context, centralstore.SnapshotQuery) (centralstore.StoreSnapshot, error)
}

// RuntimeFacts is the runner-registry overlay for one live session. These
// facts exist only while a runner generation is installed.
type RuntimeFacts struct {
	PID           int
	Endpoint      string
	RunnerVersion string
	BinaryHash    string
}

// RuntimeSource provides a point-in-time copy of the live runner registry.
// Implementations must not perform I/O; the composer calls it once per pass.
// Presence in the returned map is what "alive" means.
//
// Load-bearing eventual-consistency invariant: the composer never re-reads
// the runtime view except during a pass, so overlay skew self-corrects only
// if every runtime transition is followed by an invalidation. Publishers
// MUST publish (mark dirty) after installing a generation and after removing
// one — publish-after-install and publish-after-remove. A registry change
// without a subsequent dirty signal would leave the last emitted snapshot's
// alive/resumable overlay stale forever.
type RuntimeSource interface {
	RuntimeFacts() map[centralstore.SessionID]RuntimeFacts
}

// RuntimeSourceFunc adapts a function to RuntimeSource.
type RuntimeSourceFunc func() map[centralstore.SessionID]RuntimeFacts

func (f RuntimeSourceFunc) RuntimeFacts() map[centralstore.SessionID]RuntimeFacts { return f() }

// SessionRow is one composed session: the durable projection plus the
// runtime overlay.
type SessionRow struct {
	centralstore.SessionView

	// Alive is derived from the runtime registry, never from SQLite.
	Alive bool
	// Resumable marks a retained dead row as a resume candidate. Adapter
	// reconciliation (a later slice) may narrow this; until then every
	// retained row without a live runner is a candidate.
	Resumable bool
	Runtime   *RuntimeFacts
}

// SessionsPayload is the composed sessions-kind payload.
type SessionsPayload struct {
	Sessions []SessionRow
}

// ProjectsPayload is the composed projects-kind payload (the local durable
// input to the production snapshot.world; peers/health/launchers are runtime
// projections merged at cutover).
type ProjectsPayload struct {
	Projects            centralstore.ProjectCatalog
	LocalPeerPlacements []centralstore.LocalPeerPlacementView
}

// Batch is one composition result. When a cross-kind invalidation triggered
// the pass, both payloads are non-nil and were read from the same store
// transaction: consumers receive them as a matched pair in one call.
type Batch struct {
	Sessions *SessionsPayload
	Projects *ProjectsPayload
}

// Sink receives composed batches in order. Emit is called from the composer
// goroutine; a slow sink delays later compositions (they coalesce meanwhile)
// but never loses dirt.
type Sink interface {
	Emit(context.Context, Batch)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(context.Context, Batch)

func (f SinkFunc) Emit(ctx context.Context, b Batch) { f(ctx, b) }

// ErrorSink receives composition failures. The failed kinds stay dirty and
// are retried. A nil sink discards errors.
type ErrorSink interface {
	Error(context.Context, error)
}

// Composer owns the level-triggered dirty state and the single composition
// goroutine. Construct with New, start with Run, stop with Close.
type Composer struct {
	reader  Reader
	runtime RuntimeSource
	sink    Sink
	errSink ErrorSink
	// retryDelay throttles retries after a composition failure so a
	// persistent store error cannot become a hot loop.
	retryDelay time.Duration

	mu            sync.Mutex
	dirtySessions bool
	dirtyProjects bool

	wake chan struct{}
	done chan struct{}
	once sync.Once
}

// Option configures a Composer.
type Option func(*Composer)

// WithErrorSink installs a composition error receiver.
func WithErrorSink(s ErrorSink) Option { return func(c *Composer) { c.errSink = s } }

// WithRetryDelay overrides the failure retry throttle (tests use ~0).
func WithRetryDelay(d time.Duration) Option { return func(c *Composer) { c.retryDelay = d } }

func New(reader Reader, runtime RuntimeSource, sink Sink, opts ...Option) *Composer {
	c := &Composer{
		reader: reader, runtime: runtime, sink: sink,
		retryDelay: 100 * time.Millisecond,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Invalidate marks the kinds a committed mutation dirtied. It is the
// level-triggered signal: cheap, non-blocking, callable from any goroutine.
// SessionsDirty maps to the sessions kind; WorldDirty to the projects kind.
func (c *Composer) Invalidate(r centralstore.MutationResult) {
	c.MarkDirty(r.SessionsDirty, r.WorldDirty)
}

// MarkDirty marks kinds dirty directly.
func (c *Composer) MarkDirty(sessions, projects bool) {
	if !sessions && !projects {
		return
	}
	c.mu.Lock()
	c.dirtySessions = c.dirtySessions || sessions
	c.dirtyProjects = c.dirtyProjects || projects
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default: // a wake-up is already pending; the pass will see this dirt
	}
}

// Run is the composition loop. It blocks until ctx is canceled or Close is
// called. At most one composition is in flight at any time.
func (c *Composer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-c.wake:
		}
		// The select above picks randomly among ready cases; when Close raced
		// a pending wake-up, prefer shutdown so nothing is emitted after
		// Close (conc review LOW-01).
		select {
		case <-c.done:
			return
		default:
		}
		for {
			sessions, projects := c.takeDirty()
			if !sessions && !projects {
				break
			}
			batch, err := c.compose(ctx, sessions, projects)
			if err != nil {
				// No lost dirt: restore the taken kinds, report, and retry
				// after a throttle (or on the next invalidation, whichever
				// comes first).
				c.MarkDirty(sessions, projects)
				if c.errSink != nil {
					c.errSink.Error(ctx, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-c.done:
					return
				case <-time.After(c.retryDelay):
				}
				continue
			}
			c.sink.Emit(ctx, batch)
		}
	}
}

// Close stops Run. Safe to call multiple times.
func (c *Composer) Close() { c.once.Do(func() { close(c.done) }) }

func (c *Composer) takeDirty() (sessions, projects bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sessions, projects = c.dirtySessions, c.dirtyProjects
	c.dirtySessions, c.dirtyProjects = false, false
	return
}

// compose reads every requested payload from one store transaction and
// overlays a point-in-time runtime view. Neither step performs network I/O;
// delivery happens after the read transaction has ended, inside Run.
//
// The runtime view is captured before the store read; it may therefore be
// slightly older than the durable state, never newer than the next pass.
// Correctness relies on the RuntimeSource publish-after-install /
// publish-after-remove contract: any registry change triggers another
// invalidation, so a skewed overlay is always followed by a corrected pass.
func (c *Composer) compose(ctx context.Context, sessions, projects bool) (Batch, error) {
	var runtime map[centralstore.SessionID]RuntimeFacts
	if sessions && c.runtime != nil {
		runtime = c.runtime.RuntimeFacts()
	}
	snap, err := c.reader.ReadSnapshot(ctx, centralstore.SnapshotQuery{
		IncludeSessions: sessions,
		IncludeProjects: projects,
	})
	if err != nil {
		return Batch{}, err
	}
	var out Batch
	if sessions {
		rows := make([]SessionRow, 0, len(snap.Sessions))
		for _, v := range snap.Sessions {
			row := SessionRow{SessionView: v}
			if facts, live := runtime[v.ID]; live {
				row.Alive = true
				f := facts
				row.Runtime = &f
			} else {
				row.Resumable = true
			}
			rows = append(rows, row)
		}
		out.Sessions = &SessionsPayload{Sessions: rows}
	}
	if projects {
		out.Projects = &ProjectsPayload{
			Projects:            snap.Projects,
			LocalPeerPlacements: snap.LocalPeerPlacements,
		}
	}
	return out, nil
}
