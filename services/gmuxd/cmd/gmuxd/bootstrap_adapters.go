package main

// Concrete, inert production adapters for the S5 bootstrap. They deliberately
// contain no authority selection; constructing them has no side effects.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// semanticAgentAdapter is the shared server-side classification used by task
// families and notification semantics. ConversationSource is the existing
// capability that distinguishes conversation-backed agents from shells.
func semanticAgentAdapter(a adapter.Adapter) bool {
	_, ok := a.(adapter.ConversationSource)
	return ok
}

// productionEndpointSource enumerates both current and legacy runner dirs.
type productionEndpointSource struct{}

func (productionEndpointSource) Endpoints(context.Context) ([]string, error) {
	var out []string
	for _, dir := range append([]string{paths.SessionSocketDir()}, paths.LegacySessionSocketDirs()...) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("enumerate runner sockets %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sock") {
				endpoint := filepath.Join(dir, entry.Name())
				// Fresh runners publish this marker under the socket namespace
				// fence before bind becomes discoverable. Their asserted direct
				// registration is the only path allowed to issue the identity.
				if _, err := os.Stat(endpoint + ".registering"); err == nil {
					lease, leaseErr := socklease.AcquireExisting(endpoint)
					if errors.Is(leaseErr, socklease.ErrHeld) {
						continue // live fresh runner; direct registration owns issuance
					}
					if leaseErr == nil {
						_ = lease.ReleaseKeepingLockFile()
					} else if !errors.Is(leaseErr, socklease.ErrNoLockFile) {
						return nil, fmt.Errorf("inspect registration lease %s: %w", endpoint, leaseErr)
					}
					// The marker outlived its runner. Retire it so normal stale
					// socket discovery and reaping can resume.
					if err := os.Remove(endpoint + ".registering"); err != nil && !os.IsNotExist(err) {
						return nil, fmt.Errorf("remove stale registration marker %s: %w", endpoint, err)
					}
				} else if !os.IsNotExist(err) {
					return nil, fmt.Errorf("inspect runner registration marker %s: %w", endpoint, err)
				}
				out = append(out, endpoint)
			}
		}
	}
	return out, nil
}

// productionRunnerControl retains the existing runner kill transport.
type productionRunnerControl struct{}

func (productionRunnerControl) Terminate(ctx context.Context, endpoint, expectIncarnation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return discovery.KillSessionContext(ctx, endpoint, expectIncarnation)
}

// Reap maps the coordinator's conditional reap onto the runner's /reap route
// and translates "this runner has no such route" into the coordinator's own
// sentinel, so the reap loop can treat a pre-protocol occupant as a decline
// rather than a failure.
func (productionRunnerControl) Reap(ctx context.Context, endpoint, expectIncarnation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := discovery.ReapSessionContext(ctx, endpoint, expectIncarnation)
	if errors.Is(err, discovery.ErrRunnerReapUnsupported) {
		return fmt.Errorf("%w: %v", sessioncoord.ErrReapUnsupported, err)
	}
	return err
}

// productionConversationResolver dispatches opaque refs to the adapter registry.
type productionConversationResolver struct{}

func (productionConversationResolver) DescribeConversation(ctx context.Context, name, ref string) (sessioncoord.ConversationInfo, error) {
	if err := ctx.Err(); err != nil {
		return sessioncoord.ConversationInfo{}, err
	}
	d, ok := adapters.FindByAdapter(name).(adapter.ConversationDescriber)
	if !ok {
		return sessioncoord.ConversationInfo{}, fmt.Errorf("adapter %q has no conversation describer", name)
	}
	info, err := d.DescribeConversation(ref)
	if err != nil {
		return sessioncoord.ConversationInfo{}, err
	}
	return sessioncoord.ConversationInfo{ID: info.ID, AncestorIDs: append([]string(nil), info.AncestorIDs...)}, nil
}

// productionAdapterReconciler probes one coordinator-bounded batch. A missing
// prober is Unknown, preserving retained rows conservatively.
type productionAdapterReconciler struct{}

func (productionAdapterReconciler) ReconcileRetained(ctx context.Context, name string, batch []sessioncoord.ReconcileCandidate) ([]sessioncoord.ReconcileDecision, error) {
	p, ok := adapters.FindByAdapter(name).(adapter.ConversationProber)
	if !ok {
		return nil, nil
	}
	out := make([]sessioncoord.ReconcileDecision, 0, len(batch))
	for _, candidate := range batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gone, known := p.ConversationGone(candidate.ConversationRef)
		d := sessioncoord.DispositionUnknown
		if known && gone {
			d = sessioncoord.DispositionRemove
		} else if known {
			d = sessioncoord.DispositionRetain
		}
		out = append(out, sessioncoord.ReconcileDecision{ID: candidate.ID, Disposition: d})
	}
	return out, nil
}
