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
	return c.acknowledgeDead(ctx, id, nil)
}

// AcknowledgeDeadToken consumes only the result token observed by the caller.
// It is the retained-session half of outcome-bound /read.
func (c *Coordinator) AcknowledgeDeadToken(ctx context.Context, id centralstore.SessionID, token string) error {
	return c.acknowledgeDead(ctx, id, &token)
}

type durableTokenAcknowledger interface {
	AcknowledgeDeadSessionToken(context.Context, centralstore.SessionID, centralstore.RowVersion, string) (centralstore.MutationResult, error)
}

func (c *Coordinator) acknowledgeDead(ctx context.Context, id centralstore.SessionID, token *string) error {
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
		var result centralstore.MutationResult
		if token == nil {
			result, err = c.durable.AcknowledgeDeadSession(ctx, id, version)
		} else if durable, ok := c.durable.(durableTokenAcknowledger); ok {
			result, err = durable.AcknowledgeDeadSessionToken(ctx, id, version, *token)
		} else {
			c.mu.Unlock()
			return errors.New("sessioncoord: durable store does not support token-bound acknowledgement")
		}
		seq := c.outcomes.allocSeq() // stamp before releasing c.mu
		c.mu.Unlock()
		if err == nil {
			c.publish(ctx, result)
			c.emitOutcomes(ctx, seq, id)
			return nil
		}
		if errors.Is(err, centralstore.ErrSessionNotFound) {
			return fmt.Errorf("%w: %s", centralstore.ErrSessionNotFound, id)
		}
		if errors.Is(err, centralstore.ErrUnreadTokenChanged) {
			return err
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
