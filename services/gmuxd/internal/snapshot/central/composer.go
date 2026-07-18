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
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
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

// ResumeVerdict is the adapter-reconciliation verdict for one retained dead
// row. Verdicts are runtime-only (ADR 0026 forbids persisting resumability);
// a fresh daemon re-probes.
type ResumeVerdict int

const (
	// VerdictUnknown means no probe result (never probed, probe failed, or
	// storage unreachable). The overlay applies the conservative default.
	VerdictUnknown ResumeVerdict = iota
	// VerdictResumable means the owning adapter confirmed the session's
	// conversation exists and is resumable.
	VerdictResumable
	// VerdictGone means the owning adapter confirmed the conversation is
	// gone: the row is non-resumable and pending removal.
	VerdictGone
)

// VerdictSource provides a point-in-time copy of the reconciliation verdict
// map. Like RuntimeSource it performs no I/O and is read once per pass — but
// unlike runtime facts, verdict-only changes carry no committed
// MutationResult and therefore trigger no invalidation by themselves: a
// changed verdict becomes visible on the NEXT composition pass, whenever one
// happens. Production wiring that wants prompt narrowing must mark the
// sessions kind dirty after a reconciliation pass that changed verdicts
// (cutover checklist). A nil source means every dead row uses the default.
type VerdictSource interface {
	ResumeVerdicts() map[centralstore.SessionID]ResumeVerdict
}

// VerdictSourceFunc adapts a function to VerdictSource.
type VerdictSourceFunc func() map[centralstore.SessionID]ResumeVerdict

func (f VerdictSourceFunc) ResumeVerdicts() map[centralstore.SessionID]ResumeVerdict { return f() }

// LocalPeerSessionKey identifies one Local-peer session projection:
// namespaced by the peer key because peer session IDs are only unique per
// peer.
type LocalPeerSessionKey struct {
	PeerKey   centralstore.PeerKey
	SessionID string
}

// PeerWorld is one point-in-time copy of every runtime/peer-manager input
// to the world payload. Field-for-field it mirrors production
// snapshot.WorldPayload (internal/snapshot/snapshot.go) minus the durable
// Projects: peers/health are runtime status, launchers are startup adapter
// discovery (pure derived config — deliberately not SQLite state), and
// peer projects/discovered are cached network-peer projections re-broadcast
// verbatim (ADR 0002/0025 host-authoritative discovery).
//
// Type-tightening note (cutover checklist 4): the peer-status types live in
// internal/peering, which this package may import (peering never imports the
// snapshot stack — production adapts peering.Manager to PeerSource from
// cmd/gmuxd, so no cycle). The full per-session peer PROJECTIONS, whose
// shape is exactly the ADR 0001 session wire shape, deliberately do NOT
// flow through here: they are supplied to the wire conversion layer
// (internal/snapshot/wire, which imports this package) directly, keeping
// the composer free of wire shapes and breaking the import cycle the
// projects slice flagged. This payload only needs connectivity, hence the
// presence-set LocalPeerSessions.
type PeerWorld struct {
	Peers           []peering.PeerInfo
	Health          *HealthInfo
	Launchers       []peering.LauncherDef
	DefaultLauncher string
	PeerProjects    map[string][]peering.SpokeProject
	PeerDiscovered  map[string][]peering.SpokeDiscovered

	// LocalPeerSessions holds the key of every currently CONNECTED
	// Local-peer session projection. Presence in this set is what
	// "connected" means for the placement join: a durable Local-peer
	// placement row whose key is absent is dropped from the payload
	// (parent-owned placement is metadata, never an offline replica of the
	// peer's snapshot). The projections themselves ride the wire layer's
	// peer-session overlay, not this payload.
	LocalPeerSessions map[LocalPeerSessionKey]struct{}
}

// PeerSource provides a point-in-time copy of the peer-manager world
// inputs. Implementations must not perform I/O; the composer calls it once
// per projects-kind pass.
//
// Same eventual-consistency contract as RuntimeSource: the composer only
// re-reads this view during a pass, so the peer manager MUST follow every
// connect, disconnect, and cache update with MarkDirty(false, true) (or an
// Invalidate carrying WorldDirty). A peer change without a subsequent dirty
// signal would leave the last emitted world payload stale forever.
type PeerSource interface {
	PeerWorld() PeerWorld
}

// PeerSourceFunc adapts a function to PeerSource.
type PeerSourceFunc func() PeerWorld

func (f PeerSourceFunc) PeerWorld() PeerWorld { return f() }

// SessionRow is one composed session: the durable projection plus the
// runtime overlay.
type SessionRow struct {
	centralstore.SessionView

	// Alive is derived from the runtime registry, never from SQLite.
	Alive bool
	// Resumable marks a retained dead row as a resume candidate. It is
	// derived, never persisted: dead + a recorded command (production
	// parity: a row with no command cannot be respawned) + not narrowed to
	// VerdictGone by adapter reconciliation. Unknown verdicts stay resumable
	// (conservative default: the resume affordance is never gated on
	// probing).
	Resumable bool
	Runtime   *RuntimeFacts
}

// SessionsPayload is the composed sessions-kind payload.
type SessionsPayload struct {
	Sessions []SessionRow
}

// LocalPeerPlacementRow is one durable parent-owned Local-peer placement
// row that is currently connected (its key is present in the PeerSource's
// LocalPeerSessions set). The peer's ephemeral session projection itself is
// joined at the wire conversion layer, keyed by (PeerKey, SessionID) —
// peer-owned facts are never rematched or persisted locally.
type LocalPeerPlacementRow struct {
	centralstore.LocalPeerPlacementView
}

// ProjectsPayload is the composed world-kind payload: the durable catalog
// and Local-peer placement join, plus the runtime/peer-manager overlay
// supplied by the PeerSource. With a nil PeerSource the overlay fields are
// zero and every Local-peer placement row is dropped (no connected
// sessions), which is the conservative reading of the join rule.
type ProjectsPayload struct {
	Projects            centralstore.ProjectCatalog
	LocalPeerPlacements []LocalPeerPlacementRow

	Peers           []peering.PeerInfo
	Health          *HealthInfo
	Launchers       []peering.LauncherDef
	DefaultLauncher string
	PeerProjects    map[string][]peering.SpokeProject
	PeerDiscovered  map[string][]peering.SpokeDiscovered
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
	reader   Reader
	runtime  RuntimeSource
	verdicts VerdictSource
	peers    PeerSource
	sink     Sink
	errSink  ErrorSink
	// retryDelay throttles retries after a composition failure so a
	// persistent store error cannot become a hot loop.
	retryDelay time.Duration

	mu            sync.Mutex
	dirtySessions bool
	dirtyProjects bool

	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
	runOnce sync.Once
	runDone chan struct{}
}

// Option configures a Composer.
type Option func(*Composer)

// WithErrorSink installs a composition error receiver.
func WithErrorSink(s ErrorSink) Option { return func(c *Composer) { c.errSink = s } }

// WithRetryDelay overrides the failure retry throttle (tests use ~0).
func WithRetryDelay(d time.Duration) Option { return func(c *Composer) { c.retryDelay = d } }

// WithVerdictSource installs the adapter-reconciliation verdict overlay for
// dead rows' Resumable derivation.
func WithVerdictSource(s VerdictSource) Option { return func(c *Composer) { c.verdicts = s } }

// WithPeerSource installs the peer-manager world overlay: connected peers,
// health, launchers, cached peer projections, and the connected Local-peer
// session projections the durable placement rows are joined onto.
func WithPeerSource(s PeerSource) Option { return func(c *Composer) { c.peers = s } }

func New(reader Reader, runtime RuntimeSource, sink Sink, opts ...Option) *Composer {
	c := &Composer{
		reader: reader, runtime: runtime, sink: sink,
		retryDelay: 100 * time.Millisecond,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		runDone:    make(chan struct{}),
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
	select {
	case <-c.done:
		c.mu.Unlock()
		return
	default:
	}
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
	started := false
	c.runOnce.Do(func() { started = true })
	if !started {
		<-c.runDone
		return
	}
	defer close(c.runDone)
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

// Close stops Run and joins the composition worker. Safe to call multiple
// times and concurrently with invalidation. If Run was never started, Close
// establishes the completed join itself.
func (c *Composer) Close() {
	c.once.Do(func() {
		close(c.done)
		started := false
		c.runOnce.Do(func() { started = true })
		if started {
			close(c.runDone)
		}
	})
	<-c.runDone
}

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
	var verdicts map[centralstore.SessionID]ResumeVerdict
	var peerWorld PeerWorld
	if sessions && c.runtime != nil {
		runtime = c.runtime.RuntimeFacts()
	}
	if sessions && c.verdicts != nil {
		verdicts = c.verdicts.ResumeVerdicts()
	}
	if projects && c.peers != nil {
		peerWorld = c.peers.PeerWorld()
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
				row.Resumable = len(v.Command) > 0 && verdicts[v.ID] != VerdictGone
			}
			rows = append(rows, row)
		}
		out.Sessions = &SessionsPayload{Sessions: rows}
	}
	if projects {
		// Join durable Local-peer placement rows onto the point-in-time peer
		// projections: a row without a connected session is not emitted.
		// Placement is parent-owned metadata; the peer's session facts stay
		// connection-owned (ADR 0025/0026 §7).
		joined := make([]LocalPeerPlacementRow, 0, len(snap.LocalPeerPlacements))
		for _, view := range snap.LocalPeerPlacements {
			if _, connected := peerWorld.LocalPeerSessions[LocalPeerSessionKey{PeerKey: view.PeerKey, SessionID: view.SessionID}]; !connected {
				continue
			}
			joined = append(joined, LocalPeerPlacementRow{LocalPeerPlacementView: view})
		}
		out.Projects = &ProjectsPayload{
			Projects:            snap.Projects,
			LocalPeerPlacements: joined,
			Peers:               peerWorld.Peers,
			Health:              peerWorld.Health,
			Launchers:           peerWorld.Launchers,
			DefaultLauncher:     peerWorld.DefaultLauncher,
			PeerProjects:        peerWorld.PeerProjects,
			PeerDiscovered:      peerWorld.PeerDiscovered,
		}
	}
	return out, nil
}
