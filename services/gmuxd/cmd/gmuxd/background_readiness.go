package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

const backgroundStartupBudget = 60 * time.Second

type spawnedDaemon struct {
	PID  int
	Done <-chan error
}
type backgroundStartSeams struct {
	Spawn    func() (spawnedDaemon, error)
	Identity func() (unixipc.DaemonIdentity, bool)
	Retry    func(context.Context) error
	Poll     time.Duration
}

// startBackgroundSpawnFirst is the complete, still-unselected production
// policy wrapper. It captures the incumbent before spawning, applies the 60s
// ownership/takeover budget, and delegates identity-keyed readiness and lock
// retries to awaitSpawnedDaemon. The incumbent observation is returned for
// caller messaging; it can never satisfy child readiness.
func startBackgroundSpawnFirst(parent context.Context, s backgroundStartSeams) (pid int, incumbent unixipc.DaemonIdentity, replaced bool, err error) {
	if s.Identity != nil {
		incumbent, replaced = s.Identity()
	}
	ctx, cancel := context.WithTimeout(parent, backgroundStartupBudget)
	defer cancel()
	pid, err = awaitSpawnedDaemon(ctx, s)
	return
}

// awaitSpawnedDaemon is the prelanded spawn-first readiness policy. A healthy
// incumbent is never success: readiness belongs to the exact child PID. Retry
// performs bounded ownership/takeover work and may report lock contention.
func awaitSpawnedDaemon(ctx context.Context, s backgroundStartSeams) (int, error) {
	if s.Spawn == nil || s.Identity == nil {
		return 0, fmt.Errorf("background start: incomplete seams")
	}
	child, err := s.Spawn()
	if err != nil {
		return 0, err
	}
	poll := s.Poll
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if id, ok := s.Identity(); ok && id.PID == child.PID {
			return child.PID, nil
		}
		select {
		case err := <-child.Done:
			if err == nil {
				err = fmt.Errorf("child exited")
			}
			return 0, fmt.Errorf("background start: child %d exited before identity readiness: %w", child.PID, err)
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
			if s.Retry != nil {
				if err := s.Retry(ctx); err != nil && ctx.Err() != nil {
					return 0, ctx.Err()
				}
			}
		}
	}
}
