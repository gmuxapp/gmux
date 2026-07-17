package main

// Concrete, inert production adapters for the S5 bootstrap. They deliberately
// contain no authority selection; constructing them has no side effects.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

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
				out = append(out, filepath.Join(dir, entry.Name()))
			}
		}
	}
	return out, nil
}

// productionRunnerControl retains the existing runner kill transport.
type productionRunnerControl struct{}

func (productionRunnerControl) Terminate(ctx context.Context, endpoint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return discovery.KillSession(endpoint)
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
