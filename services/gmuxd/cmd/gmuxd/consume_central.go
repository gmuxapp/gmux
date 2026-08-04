package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
)

// acknowledgeSession clears unread at its current owner. Live runner facts are
// runner-owned; retained dead facts are store-owned. The retry closes the
// handoff race where a runner exits between the registry read and /read.
func acknowledgeSession(ctx context.Context, boot *Bootstrap, id centralstore.SessionID) error {
	for range 3 {
		runtime, live := registryRuntime(boot.Registry, id)
		if !live {
			return boot.Coordinator.AcknowledgeDead(ctx, id)
		}
		err := discovery.AcknowledgeUnread(ctx, runtime.Endpoint, runtime.Incarnation)
		if err == nil {
			return nil
		}
		current, stillLive := registryRuntime(boot.Registry, id)
		if stillLive && current.Incarnation == runtime.Incarnation && current.Endpoint == runtime.Endpoint {
			return err
		}
		// Ownership changed while the acknowledgement was in flight. Retry
		// against the new runner or the durable dead row, never the stale path.
	}
	return fmt.Errorf("acknowledge %s: %w", id, errors.New("session ownership kept changing"))
}
