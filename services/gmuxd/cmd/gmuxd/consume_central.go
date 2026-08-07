package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// acknowledgementRuntime is indirected so the route-level replacement-fence
// test can stop exactly before lifecycle-serialized owner resolution. Tests
// replacing it must not run in parallel; production never writes it.
var acknowledgementRuntime = func(c *sessioncoord.Coordinator, id centralstore.SessionID) (sessioncoord.Runtime, bool) {
	return c.AcknowledgementRuntime(id)
}

// acknowledgeSession clears unread at its current owner. Live runner facts are
// runner-owned; retained dead facts are store-owned. Owner resolution is taken
// through the coordinator's lifecycle mutex, so a replacement's
// commit-to-install fence is waited out rather than mistaken for dead absence.
// After runner I/O, ownership is validated again: success against a generation
// displaced in flight is retried against the installed successor or dead row.
func acknowledgeSession(ctx context.Context, boot *Bootstrap, id centralstore.SessionID, token string) error {
	for range 5 {
		runtime, live := acknowledgementRuntime(boot.Coordinator, id)
		if !live {
			err := boot.Coordinator.AcknowledgeDeadToken(ctx, id, token)
			if errors.Is(err, sessioncoord.ErrAckOwnerChanged) {
				continue
			}
			return err
		}

		err := discovery.AcknowledgeUnread(ctx, runtime.Endpoint, runtime.Incarnation, token)
		current, stillLive := acknowledgementRuntime(boot.Coordinator, id)
		sameOwner := stillLive && current.Generation == runtime.Generation &&
			current.Incarnation == runtime.Incarnation && current.Endpoint == runtime.Endpoint
		if sameOwner {
			return err
		}
		// Ownership changed while /read was in flight. Even a successful reply
		// came from the displaced generation, so retry and prove consumption at
		// the owner that survived commit-to-install.
	}
	return fmt.Errorf("acknowledge %s: %w", id, errors.New("session ownership kept changing"))
}
