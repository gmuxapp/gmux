package main

// This file contains the central-store bootstrap prepared for the S5 authority
// switch.  It is intentionally not referenced by serve: package tests drive it
// through the explicit seams below.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
)

const (
	bootstrapLockBudget       = 10 * time.Second
	bootstrapRunnerBudget     = 10 * time.Second
	bootstrapConvergeDeadline = 30 * time.Second
	bootstrapRetryInitial     = time.Second
	bootstrapRetryMaximum     = 10 * time.Second
)

// daemonStateLock is an advisory ownership claim. Close releases it; the file
// is deliberately never unlinked, so every daemon lifetime uses the same inode.
type daemonStateLock struct{ file *os.File }

func (l *daemonStateLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// acquireDaemonStateLock retries because the incumbent removes its socket
// before process exit releases flock.
func acquireDaemonStateLock(ctx context.Context, stateDir string, budget time.Duration) (*daemonStateLock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, statetool.LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(budget)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &daemonStateLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("bootstrap: acquiring %s: %w", statetool.LockFileName, context.DeadlineExceeded)
		}
		t := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// bootstrapOwnership performs phase 1 in its load-bearing order. takeover
// probes/shuts down the incumbent and waits for socket disappearance.
func bootstrapOwnership(ctx context.Context, stateDir string, takeover func(context.Context) error) (*centralstore.Store, *daemonStateLock, error) {
	if err := centralstore.Verify(ctx, stateDir); err != nil && !errors.Is(err, centralstore.ErrDatabaseMissing) {
		return nil, nil, fmt.Errorf("bootstrap verify: %w", err)
	}
	if takeover != nil {
		if err := takeover(ctx); err != nil {
			return nil, nil, fmt.Errorf("bootstrap takeover: %w", err)
		}
	}
	lock, err := acquireDaemonStateLock(ctx, stateDir, bootstrapLockBudget)
	if err != nil {
		return nil, nil, err
	}
	store, err := centralstore.Open(ctx, stateDir)
	if err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("bootstrap open: %w", err)
	}
	return store, lock, nil
}

// EndpointSource is the socket-enumeration boundary. Implementations enumerate
// the primary and legacy runner directories and return point-in-time copies.
type EndpointSource interface {
	Endpoints(context.Context) ([]string, error)
}
type EndpointSourceFunc func(context.Context) ([]string, error)

func (f EndpointSourceFunc) Endpoints(ctx context.Context) ([]string, error) { return f(ctx) }

// BootstrapConfig contains production adapters but is also the integration
// harness boundary. None may perform network I/O while holding cache/store
// locks; PeerSessionSource in particular must return a copy.
type BootstrapConfig struct {
	Store                          *centralstore.Store
	Runners                        sessioncoord.RunnerClient
	Control                        sessioncoord.RunnerControl
	Spawner                        sessioncoord.RunnerSpawner
	Resolver                       sessioncoord.ConversationResolver
	Reconciler                     sessioncoord.AdapterReconciler
	LocalPeers                     sessioncoord.LocalPeerInputSource
	Peers                          central.PeerSource
	PeerSessions                   wire.PeerSessionSource
	Converter                      *wire.Converter
	Endpoints                      EndpointSource
	Errors                         sessioncoord.ErrorSink
	Frames                         func(context.Context, wire.Frames)
	Clock                          func() centralstore.UnixMillis
	RunnerBudget, ConvergeDeadline time.Duration
	RetryInitial, RetryMaximum     time.Duration
}

// Bootstrap is the fully constructed, still-inert production graph. Store is
// exposed for statetool.Handler at the S5 route-wiring seam.
type Bootstrap struct {
	Store       *centralstore.Store
	Registry    *sessioncoord.Registry
	Coordinator *sessioncoord.Coordinator
	Composer    *central.Composer
	Cache       *wire.Cache
	cfg         BootstrapConfig
	firstPair   chan struct{}
	firstOnce   sync.Once
}

type composerDirtyBridge struct {
	mu sync.RWMutex
	c  *central.Composer
}

func (b *composerDirtyBridge) Committed(_ context.Context, r centralstore.MutationResult) {
	b.mu.RLock()
	c := b.c
	b.mu.RUnlock()
	if c != nil {
		c.Invalidate(r)
	}
}

func newBootstrap(cfg BootstrapConfig) (*Bootstrap, error) {
	if cfg.Store == nil || cfg.Runners == nil || cfg.Converter == nil || cfg.Endpoints == nil {
		return nil, errors.New("bootstrap: missing required store, runner, converter, or endpoint seam")
	}
	if cfg.Clock == nil {
		cfg.Clock = func() centralstore.UnixMillis { return centralstore.UnixMillis(time.Now().UnixMilli()) }
	}
	registry := sessioncoord.NewRegistry()
	bridge := &composerDirtyBridge{}
	opts := []sessioncoord.Option{sessioncoord.WithClock(cfg.Clock), sessioncoord.WithRunnerControl(cfg.Control), sessioncoord.WithRunnerSpawner(cfg.Spawner), sessioncoord.WithConversationTakeover(cfg.Resolver), sessioncoord.WithAdapterReconciler(cfg.Reconciler)}
	if cfg.LocalPeers != nil {
		opts = append(opts, sessioncoord.WithLocalPeerMatchInputs(cfg.LocalPeers))
	}
	coord := sessioncoord.New(registry, cfg.Runners, cfg.Store, bridge, cfg.Errors, opts...)
	cache := wire.NewCache(cfg.Converter, cfg.PeerSessions)
	b := &Bootstrap{Store: cfg.Store, Registry: registry, Coordinator: coord, Cache: cache, cfg: cfg, firstPair: make(chan struct{})}
	sink := central.SinkFunc(func(ctx context.Context, batch central.Batch) {
		frames := cache.Apply(batch)
		if frames.Sessions != nil && frames.World != nil {
			b.firstOnce.Do(func() { close(b.firstPair) })
		}
		if cfg.Frames != nil {
			cfg.Frames(ctx, frames)
		}
	})
	runtimeSource := central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts {
		out := make(map[centralstore.SessionID]central.RuntimeFacts)
		for _, r := range registry.Snapshot() {
			out[r.SessionID] = central.RuntimeFacts{PID: r.PID, Endpoint: r.Endpoint, RunnerVersion: r.RunnerVersion, BinaryHash: r.BinaryHash}
		}
		return out
	})
	verdictSource := central.VerdictSourceFunc(func() map[centralstore.SessionID]central.ResumeVerdict {
		in := coord.ResumeVerdicts()
		out := make(map[centralstore.SessionID]central.ResumeVerdict, len(in))
		for id, verdict := range in {
			out[id] = central.ResumeVerdict(verdict)
		}
		return out
	})
	composer := central.New(cfg.Store, runtimeSource, sink, central.WithVerdictSource(verdictSource), central.WithPeerSource(cfg.Peers), central.WithErrorSink(centralErrorAdapter{cfg.Errors}))
	b.Composer = composer
	bridge.mu.Lock()
	bridge.c = composer
	bridge.mu.Unlock()
	return b, nil
}

type centralErrorAdapter struct{ sink sessioncoord.ErrorSink }

func (a centralErrorAdapter) Error(ctx context.Context, err error) {
	if a.sink != nil {
		a.sink.Error(ctx, err)
	}
}

func (b *Bootstrap) bounds() (time.Duration, time.Duration, time.Duration, time.Duration) {
	rb, gd, ri, rm := b.cfg.RunnerBudget, b.cfg.ConvergeDeadline, b.cfg.RetryInitial, b.cfg.RetryMaximum
	if rb <= 0 {
		rb = bootstrapRunnerBudget
	}
	if gd <= 0 {
		gd = bootstrapConvergeDeadline
	}
	if ri <= 0 {
		ri = bootstrapRetryInitial
	}
	if rm <= 0 {
		rm = bootstrapRetryMaximum
	}
	return rb, gd, ri, rm
}

// Converge is phase 5. Every candidate is classified by a bounded Register;
// the global deadline cancels stragglers. A failed durable finish retries
// until success or daemon shutdown, and readiness remains withheld.
func (b *Bootstrap) Converge(ctx context.Context) ([]string, error) {
	if err := b.Coordinator.BeginConvergence(ctx); err != nil {
		return nil, err
	}
	endpoints, err := b.cfg.Endpoints.Endpoints(ctx)
	if err != nil {
		return nil, err
	}
	runnerBudget, globalBudget, retry, retryMax := b.bounds()
	global, cancel := context.WithTimeout(ctx, globalBudget)
	defer cancel()
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		ep := ep
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe, stop := context.WithTimeout(global, runnerBudget)
			defer stop()
			if _, e := b.Coordinator.Register(probe, sessioncoord.RegisterRequest{Endpoint: ep}); e != nil && b.cfg.Errors != nil {
				class := "unreachable"
				if errors.Is(e, sessioncoord.ErrInvalidSessionID) || errors.Is(e, sessioncoord.ErrResumeIdentityMismatch) || errors.Is(e, sessioncoord.ErrReplaceWithoutClaim) {
					class = "permanent"
				}
				b.cfg.Errors.Error(context.WithoutCancel(ctx), fmt.Errorf("bootstrap register %s (%s): %w", ep, class, e))
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-global.Done():
		cancel()
		// FinishConvergence is a join point: no registration may still be
		// approaching its commit after the sweep closes the window.
		<-done
	}
	for {
		_, err = b.Coordinator.FinishConvergence(ctx, b.cfg.Clock())
		if err == nil {
			return endpoints, nil
		}
		if b.cfg.Errors != nil {
			b.cfg.Errors.Error(ctx, fmt.Errorf("bootstrap finish convergence: %w", err))
		}
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		retry = min(retry*2, retryMax)
	}
}

// StartPostConvergence repairs runtime state before publishing the first
// subscriber-visible matched pair. Callers create listeners only after return.
func (b *Bootstrap) StartPostConvergence(ctx context.Context, endpoints []string) error {
	if _, err := b.Coordinator.ReapOrphans(ctx, endpoints); err != nil {
		return fmt.Errorf("bootstrap initial reap: %w", err)
	}
	if err := b.Reconcile(ctx); err != nil {
		return err
	}
	go b.Composer.Run(ctx)
	b.Composer.MarkDirty(true, true)
	select {
	case <-b.firstPair:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reconcile is the common startup/death/deletion/periodic trigger seam.
func (b *Bootstrap) Reconcile(ctx context.Context) error {
	for {
		_, changed, err := b.Coordinator.Reconcile(ctx)
		if changed {
			b.Composer.MarkDirty(true, false)
		}
		if !errors.Is(err, sessioncoord.ErrReconcileInFlight) {
			return err
		}
		// An overlapping trigger is level-triggered, never lost. Wait for the
		// current pass to release single-flight and perform the promised pass.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Scan is the periodic discovery trigger: register candidates, then piggyback
// orphan reaping and reconciliation. Registration itself deduplicates active
// generations safely.
func (b *Bootstrap) Scan(ctx context.Context) error {
	eps, err := b.cfg.Endpoints.Endpoints(ctx)
	if err != nil {
		return err
	}
	rb, globalBudget, _, _ := b.bounds()
	scanCtx, stop := context.WithTimeout(ctx, globalBudget)
	defer stop()
	workers := 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, ep := range eps {
		ep := ep
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-scanCtx.Done():
				return
			}
			defer func() { <-sem }()
			p, cancel := context.WithTimeout(scanCtx, rb)
			_, e := b.Coordinator.Register(p, sessioncoord.RegisterRequest{Endpoint: ep})
			cancel()
			if e != nil && b.cfg.Errors != nil {
				b.cfg.Errors.Error(ctx, fmt.Errorf("bootstrap periodic register %s: %w", ep, e))
			}
		}()
	}
	wg.Wait()
	if _, err := b.Coordinator.ReapOrphans(ctx, eps); err != nil {
		if b.cfg.Errors != nil {
			b.cfg.Errors.Error(ctx, fmt.Errorf("bootstrap periodic reap: %w", err))
		}
		return err
	}
	return b.Reconcile(ctx)
}

// TriggerConfig is the inert production trigger graph. Tick is supplied by
// the discovery scheduler; conversation deletions come from WatchSources.
type TriggerConfig struct {
	Tick                <-chan time.Time
	ConversationDeleted <-chan struct{}
	PeerSessionsChanged <-chan struct{}
	PeerWorldChanged    <-chan struct{}
	// Activity forwards transient runner activity to the concrete SSE/cache
	// fan-out. It is deliberately not folded into durable dirty state.
	Activity func(sessioncoord.Outcome)
}

// StartTriggers wires every asynchronous post-convergence path. All workers
// are joined before return; callers run this in the bootstrap lifetime group.
func (b *Bootstrap) StartTriggers(ctx context.Context, cfg TriggerConfig) error {
	seed, outcomes, unsubscribe, err := b.SubscribeOutcomes(ctx)
	if err != nil {
		return err
	}
	defer unsubscribe()
	var wg sync.WaitGroup
	run := func(ch <-chan struct{}, fn func()) {
		if ch == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					fn()
				}
			}
		}()
	}
	run(cfg.ConversationDeleted, func() {
		if e := b.Reconcile(ctx); e != nil && b.cfg.Errors != nil {
			b.cfg.Errors.Error(ctx, e)
		}
	})
	run(cfg.PeerSessionsChanged, func() { b.Composer.MarkDirty(true, false) })
	run(cfg.PeerWorldChanged, func() { b.Composer.MarkDirty(false, true) })
	// Reconciliation can perform adapter I/O. Keep the outcome subscriber
	// bounded and non-blocking by coalescing death into one level-triggered
	// slot; this prevents a slow adapter from backpressuring coordinator
	// commits. Seed is processed through the same path, closing the
	// snapshot/subscribe race.
	death := make(chan struct{}, 1)
	noteDeath := func(o sessioncoord.Outcome) {
		if o.Type == sessioncoord.OutcomeUpserted && o.Session != nil && !o.Alive {
			select {
			case death <- struct{}{}:
			default:
			}
		}
	}
	for _, o := range seed {
		noteDeath(o)
	}
	run(death, func() {
		if e := b.Reconcile(ctx); e != nil && b.cfg.Errors != nil {
			b.cfg.Errors.Error(ctx, e)
		}
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case o, ok := <-outcomes:
				if !ok {
					return
				}
				noteDeath(o)
				if o.Type == sessioncoord.OutcomeActivity && cfg.Activity != nil {
					cfg.Activity(o)
				}
			}
		}
	}()
	if cfg.Tick != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-cfg.Tick:
					if !ok {
						return
					}
					if e := b.Scan(ctx); e != nil && b.cfg.Errors != nil {
						b.cfg.Errors.Error(ctx, e)
					}
				}
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

// SubscribeOutcomes atomically establishes the post-barrier consumer fence.
func (b *Bootstrap) SubscribeOutcomes(ctx context.Context) ([]sessioncoord.Outcome, <-chan sessioncoord.Outcome, func(), error) {
	return b.Coordinator.SubscribeOutcomesSeed(ctx)
}
