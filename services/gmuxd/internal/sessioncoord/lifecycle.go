package sessioncoord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

var (
	// ErrLifecycleOpInFlight marks a stop/resume/restart attempted while
	// another lifecycle operation for the same session is in flight. Claims
	// fail fast rather than queue; callers retry.
	ErrLifecycleOpInFlight = errors.New("sessioncoord: another lifecycle operation is in flight for this session")
	// ErrSessionAlive marks a resume of a session with an installed live
	// generation.
	ErrSessionAlive = errors.New("sessioncoord: session has a live generation")
	// ErrSessionNotAlive marks a stop of a session with no installed live
	// generation.
	ErrSessionNotAlive = errors.New("sessioncoord: session has no live generation")
	// ErrNoRunnerControl / ErrNoRunnerSpawner mark operations whose injected
	// I/O boundary was not configured.
	ErrNoRunnerControl = errors.New("sessioncoord: no runner control configured")
	ErrNoRunnerSpawner = errors.New("sessioncoord: no runner spawner configured")
	// ErrConvergencePending marks a resume of an exit-less row while the
	// startup convergence window is open: its liveness is unknown until a
	// surviving runner had the chance to claim it, so spawning could double
	// the process.
	ErrConvergencePending = errors.New("sessioncoord: session liveness unknown until convergence closes")
	// ErrResumeIdentityMismatch marks a runner whose meta reported a
	// different session ID than the one the caller's Replace provenance was
	// authorized for (RegisterRequest.ExpectedID). Registration aborts before
	// any commit or fence: no durable write, no registry change, and a live
	// other session can never be superseded by a mis-claiming runner.
	ErrResumeIdentityMismatch = errors.New("sessioncoord: runner reported an unexpected session id")
	// ErrStopSuperseded marks a Stop whose targeted generation left the
	// registry, but a live replacement generation was installed by the time
	// the durable repair ran. The dead channel signals "the entry left the
	// registry", not "the process died": a Replace registration during the
	// wait also closes it. Stop must not report plain success in that case —
	// the targeted process was never confirmed dead and the session is live
	// again under a new generation.
	ErrStopSuperseded = errors.New("sessioncoord: stop superseded by a live replacement generation")
	// ErrExitNotDurable marks an observed death whose exit could not be
	// recorded durably after bounded retries.
	ErrExitNotDurable = errors.New("sessioncoord: could not durably record exit")
)

// RunnerControl terminates the process behind one live runner endpoint.
// Signal choice and SIGTERM→SIGKILL escalation semantics live behind this
// interface; the coordinator only requests death and observes it through the
// ordinary observation path (exit event or stream drop). Terminate must be
// idempotent. It is I/O and is never called under any coordinator lock or
// inside a database transaction.
type RunnerControl interface {
	Terminate(ctx context.Context, endpoint string) error
}

// RunnerSpawner launches a new runner process for a retained dead session and
// returns the endpoint the coordinator can Subscribe/Meta against.
// Resume-command rewriting is policy behind this interface, not coordinator
// logic. It is I/O and is never called under any coordinator lock or inside a
// database transaction.
type RunnerSpawner interface {
	Spawn(ctx context.Context, session centralstore.Session) (endpoint string, err error)
}

// RunnerSpawnCleaner is an optional extension implemented by spawners that
// retain an exact process handle. It is invoked when replacement registration
// fails, so a child that never became coordinator-owned cannot leak. Cleanup
// must be idempotent; implementations should use a context independent of the
// failed registration request when necessary.
type RunnerSpawnCleaner interface {
	CleanupSpawn(ctx context.Context, endpoint string) error
	// FinalizeSpawn releases the spawner's temporary ownership after a
	// successful registration. It must not terminate the registered child.
	FinalizeSpawn(endpoint string)
}

// LifecycleClaim identifies one held per-session lifecycle claim. It is an
// opaque identity token: a Replace/ExpectedID registration must present the
// token of the claim it runs under (RegisterRequest.Claim), and Register
// verifies pointer identity against the installed claim — holding *a* claim
// for the ID is not enough, it must be the caller's own. This closes the
// stray-Replace-during-unrelated-Stop window (review M-1).
type LifecycleClaim struct {
	id centralstore.SessionID
	op string
}

// claim reserves the per-session lifecycle slot. It fails fast when another
// lifecycle operation for the same session is in flight. The returned release
// must be called exactly once; the returned token authorizes Replace
// registrations issued under this claim.
func (c *Coordinator) claim(id centralstore.SessionID, op string) (*LifecycleClaim, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, busy := c.ops[id]; busy {
		return nil, nil, fmt.Errorf("%w (%s)", ErrLifecycleOpInFlight, current.op)
	}
	cl := &LifecycleClaim{id: id, op: op}
	c.ops[id] = cl
	return cl, func() {
		c.mu.Lock()
		delete(c.ops, id)
		c.mu.Unlock()
	}, nil
}

// Stop terminates the session's live generation and waits — bounded by ctx —
// for its death to be observed through the ordinary observation path, then
// ensures the durable row converged to dead. No database transaction spans
// the terminate/wait I/O. On a wait bound expiry the live entry is left
// installed and nothing durable changes.
func (c *Coordinator) Stop(ctx context.Context, id centralstore.SessionID) error {
	if c.control == nil {
		return ErrNoRunnerControl
	}
	_, release, err := c.claim(id, "stop")
	if err != nil {
		return err
	}
	defer release()
	return c.stopClaimed(ctx, id)
}

func (c *Coordinator) stopClaimed(ctx context.Context, id centralstore.SessionID) error {
	e, ok := c.registry.current(id)
	if !ok {
		if !c.registry.fenced(id) {
			return ErrSessionNotAlive
		}
		// The entry is fenced: a replacement is inside its commit-to-install
		// window. Wait the window out (fence resolution is bounded by the
		// lifecycle mutex, exactly like apply's fence-wait) and re-check: a
		// failed replacement restores the old generation, which is then a
		// legitimate stop target — reporting ErrSessionNotAlive for it would
		// be a lie.
		c.mu.Lock()
		c.mu.Unlock() //nolint:staticcheck // empty critical section is the point
		e, ok = c.registry.current(id)
		if !ok {
			return ErrSessionNotAlive
		}
	}
	endpoint, dead := e.Endpoint, e.dead

	// ── runner I/O; no locks, no DB transaction ──────────────────────────
	if err := c.control.Terminate(ctx, endpoint); err != nil {
		return fmt.Errorf("sessioncoord: terminate %s: %w", id, err)
	}
	select {
	case <-dead:
	case <-ctx.Done():
		// Deterministic outcome when death and the bound race: death that
		// arrived by the time the bound fired is death.
		select {
		case <-dead:
		default:
			return fmt.Errorf("sessioncoord: runner for %s did not die: %w", id, ctx.Err())
		}
	}

	// Death was observed. The exit normally landed durably before the dead
	// channel closed (exit event or drain synthesis commit before liveness
	// removal); repair the row if that commit failed.
	return c.ensureDurableExit(ctx, id)
}

// ensureDurableExit verifies the row for a just-observed death carries an
// exit timestamp, and synthesizes one at the current row version if not
// (bounded stale retries).
//
// Each attempt (liveness check + Session read + conditional apply) runs
// under the lifecycle mutex. Register holds that mutex across its
// RegisterRunner commit and registry install, so the repair can never run
// inside a registration's commit-to-install window and stamp an exit onto a
// freshly committed, not-yet-installed live row: under the mutex it either
// sees the installed live entry (ErrStopSuperseded) or runs entirely before
// the registration's commit. Short DB transactions under the mutex are
// within the lifecycle rules; there is no runner I/O here.
//
// A row that disappeared (not found before the apply, or ErrSessionNotFound
// from it) needs no repair. A row claimed by a live replacement generation
// returns ErrStopSuperseded: the targeted process was never confirmed dead.
func (c *Coordinator) ensureDurableExit(ctx context.Context, id centralstore.SessionID) error {
	var version centralstore.RowVersion
	for range 3 {
		c.mu.Lock()
		if _, live := c.registry.current(id); live {
			c.mu.Unlock()
			return fmt.Errorf("%w: session %s", ErrStopSuperseded, id)
		}
		s, ok, err := c.durable.Session(ctx, id)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		if !ok || s.ExitedAt != nil {
			c.mu.Unlock()
			return nil // row deleted, or the exit already landed
		}
		if s.Version > version {
			version = s.Version
		}
		at := c.now()
		result, err := c.durable.ApplyRunnerObservation(ctx, centralstore.RunnerObservation{
			ID: id, ObservedVersion: version, ObservedAt: at,
			Facts: centralstore.RunnerFacts{ExitedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &at}},
		})
		c.mu.Unlock()
		if err == nil {
			c.publish(ctx, result)
			c.emitOutcomes(ctx, id)
			return nil
		}
		if errors.Is(err, centralstore.ErrSessionNotFound) {
			return nil // row deleted between read and apply; nothing to repair
		}
		if !errors.Is(err, centralstore.ErrStaleVersion) {
			return err
		}
		// The stale response carries the current version; use it directly
		// for the next attempt so version churn in the read-to-apply window
		// alone cannot exhaust the retry budget. The next attempt still
		// re-reads the row so a concurrently landed exit is never
		// overwritten.
		if result.SessionVersion > version {
			version = result.SessionVersion
		}
	}
	return fmt.Errorf("%w: session %s", ErrExitNotDurable, id)
}

// Resume spawns a new runner for a dead, retained session and converges it
// through the ordinary replacement registration path. Concurrent resumes for
// one session cannot double-spawn (per-session claim); resume of a live
// session fails cleanly; the fence/replacement window is safe because the
// registration path owns it.
func (c *Coordinator) Resume(ctx context.Context, id centralstore.SessionID) (Runtime, error) {
	if c.spawner == nil {
		return Runtime{}, ErrNoRunnerSpawner
	}
	cl, release, err := c.claim(id, "resume")
	if err != nil {
		return Runtime{}, err
	}
	defer release()
	return c.resumeClaimed(ctx, id, cl)
}

func (c *Coordinator) resumeClaimed(ctx context.Context, id centralstore.SessionID, cl *LifecycleClaim) (Runtime, error) {
	if _, live := c.registry.current(id); live {
		return Runtime{}, fmt.Errorf("%w: %s", ErrSessionAlive, id)
	}
	session, ok, err := c.durable.Session(ctx, id)
	if err != nil {
		return Runtime{}, err
	}
	if !ok {
		return Runtime{}, fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, id)
	}
	if session.ExitedAt == nil {
		c.mu.Lock()
		windowOpen := c.convergeCandidates != nil && !c.convergeClosed
		c.mu.Unlock()
		if windowOpen {
			// An exit-less row during the open window is a convergence
			// candidate: a surviving runner may still claim it. Spawning now
			// could double the process. This deliberately keys on the row's
			// current exit-less state rather than membership in the window's
			// candidate set — strictly more conservative: a row that became
			// exit-less during the window is also blocked.
			return Runtime{}, fmt.Errorf("%w: %s", ErrConvergencePending, id)
		}
	}

	// ── runner I/O; no locks, no DB transaction ──────────────────────────
	endpoint, err := c.spawner.Spawn(ctx, session)
	if err != nil {
		return Runtime{}, fmt.Errorf("sessioncoord: resume spawn for %s: %w", id, err)
	}

	// Replace provenance sets NewGeneration (the store clears the dead
	// generation's generation-scoped facts) and makes a racing registration
	// safe via the fence/replacement path. ExpectedID scopes that provenance
	// to the session the resume was authorized for: a spawned runner whose
	// meta claims a different (possibly live) session aborts before any
	// commit or fence. Register failure here means the spawned runner never
	// converged; nothing durable changed and a later discovery pass may
	// still pick the process up.
	runtime, err := c.Register(ctx, RegisterRequest{Endpoint: endpoint, Replace: true, ExpectedID: id, Claim: cl})
	if err != nil {
		if cleaner, ok := c.spawner.(RunnerSpawnCleaner); ok {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			cleanupErr := cleaner.CleanupSpawn(cleanupCtx, endpoint)
			cancel()
			if cleanupErr != nil {
				c.reportError(ctx, fmt.Errorf("sessioncoord: cleanup failed spawn for %s: %w", id, cleanupErr))
			}
		}
		return Runtime{}, fmt.Errorf("sessioncoord: resume registration for %s: %w", id, err)
	}
	if cleaner, ok := c.spawner.(RunnerSpawnCleaner); ok {
		cleaner.FinalizeSpawn(endpoint)
	}
	return runtime, nil
}

// Restart composes stop and resume under one per-session claim so nothing
// interleaves between the phases. A stop-phase failure aborts before any
// spawn. Restart of an already-dead session degrades to resume.
func (c *Coordinator) Restart(ctx context.Context, id centralstore.SessionID) (Runtime, error) {
	if c.control == nil {
		return Runtime{}, ErrNoRunnerControl
	}
	if c.spawner == nil {
		return Runtime{}, ErrNoRunnerSpawner
	}
	cl, release, err := c.claim(id, "restart")
	if err != nil {
		return Runtime{}, err
	}
	defer release()
	if _, live := c.registry.current(id); live {
		if err := c.stopClaimed(ctx, id); err != nil {
			return Runtime{}, err
		}
	}
	return c.resumeClaimed(ctx, id, cl)
}
