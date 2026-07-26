package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/sessionenv"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

const backgroundStartupBudget = 60 * time.Second

type spawnedDaemon struct {
	PID  int
	Done <-chan error
	Kill func() error
}
type backgroundStartSeams struct {
	Spawn    func() (spawnedDaemon, error)
	Identity func() (unixipc.DaemonIdentity, bool)
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

// startBackgroundProduction only supervises the detached child. Verification,
// incumbent shutdown, flock acquisition, and store open belong exclusively to
// the child daemon. Readiness is therefore proof that that exact child has
// completed takeover and started listening.
func startBackgroundProduction(parent context.Context, exe, sockPath, stateDir string, childStderr io.Writer) (pid int, incumbent unixipc.DaemonIdentity, replaced bool, err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return 0, unixipc.DaemonIdentity{}, false, fmt.Errorf("background start: state directory: %w", err)
	}
	identity := func() (unixipc.DaemonIdentity, bool) { return unixipc.HealthIdentity(sockPath) }
	incumbent, replaced = identity()
	ctx, cancel := context.WithTimeout(parent, backgroundStartupBudget)
	defer cancel()
	seams := backgroundStartSeams{Identity: identity, Spawn: func() (spawnedDaemon, error) {
		child := exec.Command(exe, "run", "--replace")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		child.Stderr = childStderr
		child.Env = sessionenv.Strip(os.Environ())
		if err := child.Start(); err != nil {
			return spawnedDaemon{}, fmt.Errorf("background start: spawn: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- child.Wait(); close(done) }()
		return spawnedDaemon{PID: child.Process.Pid, Done: done, Kill: child.Process.Kill}, nil
	}}
	pid, err = awaitSpawnedDaemon(ctx, seams)
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
	cleanup := func() {
		if child.Kill != nil {
			_ = child.Kill()
		}
		// Wait is owned by Done; joining it prevents zombies on every failure.
		<-child.Done
	}
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
			cleanup()
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}
