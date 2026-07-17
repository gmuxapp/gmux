package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/sessionenv"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

const backgroundStartupBudget = 60 * time.Second

// errBackgroundTakeoverRetry marks temporary ownership/lock contention.
// Every other Retry error is terminal and is returned immediately.
var errBackgroundTakeoverRetry = errors.New("background takeover retryable")

func retryableBackgroundTakeover(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", errBackgroundTakeoverRetry, err)
}

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

// startBackgroundProduction is the concrete spawn-first entry point prepared
// for the S5 switch. Callers supply paths and writers, not lifecycle policy.
// The child is detached before the database is inspected, but the incumbent is
// not contacted until Verify succeeds. On every failure the new child is
// reaped while the incumbent is left alone whenever takeover has not begun.
func startBackgroundProduction(parent context.Context, exe, sockPath, stateDir string, childStderr io.Writer) (pid int, incumbent unixipc.DaemonIdentity, replaced bool, err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return 0, unixipc.DaemonIdentity{}, false, fmt.Errorf("background start: state directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, backgroundStartupBudget)
	defer cancel()
	var child *exec.Cmd
	takeoverDone := false
	identity := func() (unixipc.DaemonIdentity, bool) { return unixipc.HealthIdentity(sockPath) }
	incumbent, replaced = identity()
	seams := backgroundStartSeams{
		Identity: identity,
		Spawn: func() (spawnedDaemon, error) {
			child = exec.Command(exe, "run")
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			child.Stderr = childStderr
			child.Env = sessionenv.Strip(os.Environ())
			if err := child.Start(); err != nil {
				return spawnedDaemon{}, fmt.Errorf("background start: spawn: %w", err)
			}
			if err := centralstore.Verify(ctx, stateDir); err != nil && !errors.Is(err, centralstore.ErrDatabaseMissing) {
				_ = child.Process.Kill()
				_ = child.Wait()
				return spawnedDaemon{}, fmt.Errorf("background start: verify: %w", err)
			}
			done := make(chan error, 1)
			go func() { done <- child.Wait(); close(done) }()
			return spawnedDaemon{PID: child.Process.Pid, Done: done}, nil
		},
		Retry: func(ctx context.Context) error {
			if !takeoverDone {
				if id, ok := unixipc.HealthIdentity(sockPath); ok && (incumbent.PID == 0 || id.PID == incumbent.PID) {
					if !unixipc.Shutdown(sockPath) {
						return fmt.Errorf("shutdown incumbent pid %d failed", id.PID)
					}
				}
				takeoverDone = true
			}
			// Socket removal precedes process exit. Prove that the stable lock
			// inode is free; contention is the sole retryable takeover error.
			f, openErr := os.OpenFile(filepath.Join(stateDir, statetool.LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
			if openErr != nil {
				return fmt.Errorf("open %s: %w", statetool.LockFileName, openErr)
			}
			lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if lockErr == nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			_ = f.Close()
			if errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN) {
				return retryableBackgroundTakeover(lockErr)
			}
			if lockErr != nil {
				return fmt.Errorf("lock %s: %w", statetool.LockFileName, lockErr)
			}
			return nil
		},
	}
	pid, err = awaitSpawnedDaemon(ctx, seams)
	if err != nil && child != nil && child.Process != nil {
		_ = child.Process.Kill()
	}
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
	childExited := func(err error) (int, error) {
		if err == nil {
			err = fmt.Errorf("child exited")
		}
		return 0, fmt.Errorf("background start: child %d exited before identity readiness: %w", child.PID, err)
	}
	for {
		// Exit is authoritative even when a stale/final health response and
		// process completion become observable in the same polling turn.
		select {
		case err := <-child.Done:
			return childExited(err)
		default:
		}
		if id, ok := s.Identity(); ok && id.PID == child.PID {
			select {
			case err := <-child.Done:
				return childExited(err)
			default:
				return child.PID, nil
			}
		}
		select {
		case err := <-child.Done:
			return childExited(err)
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
			if s.Retry != nil {
				if err := s.Retry(ctx); err != nil {
					if ctx.Err() != nil {
						return 0, ctx.Err()
					}
					if !errors.Is(err, errBackgroundTakeoverRetry) {
						return 0, fmt.Errorf("background start: takeover: %w", err)
					}
				}
			}
		}
	}
}
