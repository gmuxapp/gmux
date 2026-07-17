package main

import (
	"context"
	"errors"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartBackgroundSpawnFirstCapturesIncumbentAndWaitsForChild(t *testing.T) {
	done := make(chan error)
	var probes atomic.Int32
	seams := backgroundStartSeams{Spawn: func() (spawnedDaemon, error) { return spawnedDaemon{PID: 22, Done: done}, nil }, Identity: func() (unixipc.DaemonIdentity, bool) {
		if probes.Add(1) < 3 {
			return unixipc.DaemonIdentity{PID: 11, Version: "old"}, true
		}
		return unixipc.DaemonIdentity{PID: 22, Version: "new"}, true
	}, Poll: time.Millisecond}
	pid, incumbent, replaced, err := startBackgroundSpawnFirst(context.Background(), seams)
	if err != nil || pid != 22 || !replaced || incumbent.PID != 11 {
		t.Fatalf("pid=%d incumbent=%+v replaced=%v err=%v", pid, incumbent, replaced, err)
	}
}

func TestAwaitSpawnedDaemonIdentityAware(t *testing.T) {
	var probes atomic.Int32
	done := make(chan error)
	s := backgroundStartSeams{Spawn: func() (spawnedDaemon, error) { return spawnedDaemon{PID: 22, Done: done}, nil }, Identity: func() (unixipc.DaemonIdentity, bool) {
		if probes.Add(1) < 3 {
			return unixipc.DaemonIdentity{PID: 11}, true
		}
		return unixipc.DaemonIdentity{PID: 22}, true
	}, Poll: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pid, err := awaitSpawnedDaemon(ctx, s)
	if err != nil || pid != 22 {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
}
func TestAwaitSpawnedDaemonChildExitDespiteIncumbent(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("boom")
	_, err := awaitSpawnedDaemon(context.Background(), backgroundStartSeams{Spawn: func() (spawnedDaemon, error) { return spawnedDaemon{PID: 22, Done: done}, nil }, Identity: func() (unixipc.DaemonIdentity, bool) { return unixipc.DaemonIdentity{PID: 11}, true }})
	if err == nil {
		t.Fatal("child exit mistaken for incumbent health")
	}
}
func TestAwaitSpawnedDaemonBoundedLockRetry(t *testing.T) {
	done := make(chan error)
	var retries atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := awaitSpawnedDaemon(ctx, backgroundStartSeams{Spawn: func() (spawnedDaemon, error) { return spawnedDaemon{PID: 22, Done: done}, nil }, Identity: func() (unixipc.DaemonIdentity, bool) { return unixipc.DaemonIdentity{PID: 11}, true }, Retry: func(context.Context) error { retries.Add(1); return errors.New("locked") }, Poll: time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || retries.Load() == 0 {
		t.Fatalf("err=%v retries=%d", err, retries.Load())
	}
}
