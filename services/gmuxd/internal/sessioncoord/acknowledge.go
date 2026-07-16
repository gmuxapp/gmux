package sessioncoord

import (
	"context"
	"errors"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ErrAckNotDurable marks an acknowledgement that could not be recorded
// durably after bounded stale retries.
var ErrAckNotDurable = errors.New("sessioncoord: could not durably acknowledge session")

// AcknowledgeDead durably clears the user-facing unread/error indicators of a
// dead session (the `.../read` route and presence-driven selection clears).
//
// Live sessions are acknowledged by the runner on WS attach — a daemon write
// would violate runner ownership (ADR 0026 §3) — so a live or fenced target
// is a deliberate silent no-op: today's UI calls this opportunistically on
// every selection. Each attempt (liveness check + row read + conditional
// acknowledge) runs under the lifecycle mutex, exactly like
// ensureDurableExit, so it can never interleave with a registration's
// commit-to-install window; the store call is a short DB transaction with no
// runner I/O. Bounded stale retries mirror ensureDurableExit's budget.
func (c *Coordinator) AcknowledgeDead(ctx context.Context, id centralstore.SessionID) error {
	var version centralstore.RowVersion
	for range 3 {
		c.mu.Lock()
		if _, live := c.registry.current(id); live || c.registry.fenced(id) {
			c.mu.Unlock()
			return nil // runner-owned while live (or being replaced): no-op
		}
		s, ok, err := c.durable.Session(ctx, id)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		if !ok {
			c.mu.Unlock()
			return fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, id)
		}
		if !s.Unread && !s.Error {
			c.mu.Unlock()
			return nil // nothing to clear
		}
		if s.Version > version {
			version = s.Version
		}
		result, err := c.durable.AcknowledgeDeadSession(ctx, id, version)
		c.mu.Unlock()
		if err == nil {
			c.publish(ctx, result)
			c.emitOutcomes(ctx, id)
			return nil
		}
		if errors.Is(err, centralstore.ErrSessionNotFound) {
			return fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, id)
		}
		if !errors.Is(err, centralstore.ErrStaleVersion) {
			return err
		}
		// The stale response carries the current version; the next attempt
		// still re-reads the row so a concurrent registration's facts are
		// respected.
		if result.SessionVersion > version {
			version = result.SessionVersion
		}
	}
	return fmt.Errorf("%w: %s", ErrAckNotDurable, id)
}
