package centralstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
)

// SweepDeadSessions marks, in one durable transaction, every candidate row
// whose runner did not come back during the startup convergence window:
//
//   - ExitedAt is synthesized from the sweep time (the dead-generation
//     contract: every dead row carries an explicit exit timestamp);
//   - ExitCode stays NULL — the daemon never observed the exit;
//   - working/unread/error are preserved: the turn state at death is the
//     wait verdict and must not depend on how death was detected;
//   - last activity bumps on the alive→dead transition and never moves
//     backwards.
//
// The only per-row predicate is `exited_at_ms IS NULL`: a candidate whose
// exit was recorded meanwhile, or whose row disappeared, is silently
// skipped. There is deliberately NO row-version predicate — the row version
// can churn during the window without resolving liveness (a dead-row
// acknowledgement bumps it; a re-registration with changed facts bumps it
// and the runner may still vanish without exit facts afterwards), and a
// version fence would let such rows escape the one-shot sweep forever
// without an exit timestamp. Concurrent-registration safety is therefore
// entirely the caller's obligation: the lifecycle coordinator must hold its
// mutex across candidate selection and this call, and must exclude every
// row with a currently installed live generation.
//
// The whole sweep commits as one transaction and reports one invalidation.
func (s *Store) SweepDeadSessions(ctx context.Context, candidates []SessionID, at UnixMillis) (MutationResult, error) {
	if at < 0 {
		return MutationResult{}, errors.New("centralstore: sweep timestamp must be non-negative")
	}
	for _, id := range candidates {
		if id == "" {
			return MutationResult{}, errors.New("centralstore: sweep candidate requires session id")
		}
	}
	if len(candidates) == 0 {
		return MutationResult{}, nil
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	var swept int64
	for _, id := range candidates {
		n, err := q.SweepSessionDead(ctx, db.SweepSessionDeadParams{
			SweptAtMs: sql.NullInt64{Int64: int64(at), Valid: true},
			ID:        string(id),
		})
		if err != nil {
			return MutationResult{}, err
		}
		swept += n
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: swept > 0, SessionsDirty: swept > 0}, nil
}
