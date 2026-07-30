package main

// stale_socket_race_test.go — the schedule the whole ownership design exists
// to survive.
//
// Every earlier iteration of stale-socket reaping in this codebase was broken
// by the same three-actor race: the daemon decides a pathname is abandoned, a
// fresh runner binds that pathname, and the daemon's unlink lands on the live
// runner's socket. Reviews reproduced it against a stat/SameFile predicate and
// again against a rename-claim protocol.
//
// This test runs the real production reaper flat out against a binder that
// behaves like a runner — leasing, clearing stale files, listening, serving,
// and exiting either cleanly or by "crashing" — and asserts the one invariant
// that matters: while a runner is live at a pathname, nothing removes or
// replaces it.
//
// Run with -race.

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/socklease"
)

// The deterministic half of the same property: the reaper holds the lease for
// its entire inspect-and-unlink sequence, so a runner cannot appear inside it.
// Stress alone cannot pin this -- the window is microseconds wide, and reviews
// needed thousands of rounds to hit the equivalent window in the predicate
// this replaced.
//
// Mutation: drop the lease from reapStaleSocket (stat, probe, SameFile,
// unlink). The binder below then acquires the pathname in the middle of the
// reaper's sequence, which is exactly the schedule that unlinked live runners.
// onReapPhase installs the reaper's barrier for one endpoint and one test.
//
// Filtering on the endpoint is not tidiness: reapers run concurrently -- three
// of them in TestReaperNeverUnlinksALiveRunner, one per endpoint inside a
// discovery scan -- so an unfiltered barrier is driven by whichever reaper
// reaches the phase first. That is how this hook produced a CI failure: the
// barrier for one test's pathname fired for a different reaper's pathname and
// perturbed the filesystem before the test's own reaper had arrived.
//
// fired reports whether the barrier ever ran, so a test can insist that the
// schedule it is asserting about actually happened.
func onReapPhase(t *testing.T, ep, phase string, fn func()) (fired func() bool) {
	t.Helper()
	var ran atomic.Bool
	var once sync.Once
	setReapBarrier(func(gotEP, gotPhase string) {
		if gotEP != ep || gotPhase != phase {
			return
		}
		once.Do(func() {
			ran.Store(true)
			fn()
		})
	})
	t.Cleanup(func() { setReapBarrier(nil) })
	return ran.Load
}

func TestReaperHoldsTheLeaseAcrossItsWholeSequence(t *testing.T) {
	dir := socketDir(t)
	ep := crashedRunnerSocket(t, dir, "1x4wh8j3.sock")

	entered := make(chan struct{})
	release := make(chan struct{})
	onReapPhase(t, ep, "lease-held", func() {
		close(entered)
		<-release
	})

	done := make(chan reapOutcome, 1)
	go func() { done <- reapStaleSocket(ep) }()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper never entered its leased window")
	}

	// A runner starting right now must be excluded until the reaper is done.
	if _, err := socklease.Acquire(ep); !errors.Is(err, socklease.ErrHeld) {
		close(release)
		t.Fatalf("a runner acquired the pathname inside the reaper's window: %v", err)
	}

	close(release)
	if outcome := <-done; !outcome.Reaped {
		t.Fatalf("the reap did not complete: %s", outcome.Reason)
	}
	// And once it is done, the pathname is free for the next runner.
	lease, err := socklease.Acquire(ep)
	if err != nil {
		t.Fatalf("the pathname was not released after the reap: %v", err)
	}
	_ = lease.Release()
}

func TestReaperNeverUnlinksALiveRunner(t *testing.T) {
	dir := socketDir(t)
	ep := filepath.Join(dir, "1kc5cwpd.sock")

	const rounds = 400
	var (
		reaps       atomic.Int64
		cleanExits  atomic.Int64
		crashExits  atomic.Int64
		leaseDenied atomic.Int64
		stop        atomic.Bool
		wg          sync.WaitGroup
	)

	// Reapers: as many concurrent daemons as the code could ever have, going
	// as fast as they can.
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if reapStaleSocket(ep).Reaped {
					reaps.Add(1)
				}
			}
		}()
	}

	// Binder: one runner lifecycle per round.
	for round := range rounds {
		lease, err := socklease.Acquire(ep)
		if errors.Is(err, socklease.ErrHeld) {
			// A reaper holds the lease for the length of its inspection.
			leaseDenied.Add(1)
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("round %d: Acquire: %v", round, err)
		}

		// Clear whatever stale file is there and bind, exactly as BindSocket
		// does, under the lease.
		_ = os.Remove(ep)
		ln, err := net.Listen("unix", ep)
		if err != nil {
			t.Fatalf("round %d: listen: %v", round, err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		pin, err := lease.PinSocket()
		if err != nil {
			t.Fatalf("round %d: PinSocket: %v", round, err)
		}
		ident := pin.Ident()

		// Serve, so a reaper's probe gets a real answer rather than a refusal.
		accepting := make(chan struct{})
		go func() {
			close(accepting)
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		<-accepting
		time.Sleep(200 * time.Microsecond)

		// THE INVARIANT: the pathname still names this live runner's socket.
		current, ok := socklease.StatSocket(ep)
		if !ok {
			t.Fatalf("round %d: the reaper unlinked a live runner's socket", round)
		}
		if current != ident {
			t.Fatalf("round %d: the pathname was rebound under a live runner: %s -> %s",
				round, ident, current)
		}

		if round%2 == 0 {
			// Clean exit: release ownership the way the runner does.
			_ = ln.Close()
			if err := lease.RemoveSocket(pin); err != nil {
				t.Fatalf("round %d: RemoveSocket: %v", round, err)
			}
			_ = pin.Close()
			if err := lease.Release(); err != nil {
				t.Fatalf("round %d: Release: %v", round, err)
			}
			cleanExits.Add(1)
		} else {
			// Crash: the listener dies and the lease is dropped by the
			// kernel, but nothing unlinks the pathname and nothing removes
			// the lock file. This is the population the reaper exists for.
			_ = ln.Close()
			if err := lease.Release(); err != nil {
				t.Fatalf("round %d: Release: %v", round, err)
			}
			if err := os.WriteFile(socklease.LockPath(ep), nil, 0o600); err != nil {
				t.Fatalf("round %d: restore lock file: %v", round, err)
			}
			_ = pin.Close()
			crashExits.Add(1)
		}
	}
	stop.Store(true)
	wg.Wait()

	if crashExits.Load() == 0 || cleanExits.Load() == 0 {
		t.Fatalf("test did not exercise both exit paths: clean=%d crash=%d",
			cleanExits.Load(), crashExits.Load())
	}
	// The reapers must have had real work, or the invariant above proved
	// nothing about a reaper that never fires.
	if reaps.Load() == 0 {
		t.Fatal("no stale socket was ever reaped; the race was never actually run")
	}
	t.Logf("reaped %d abandoned sockets across %d rounds (%d lease denials)",
		reaps.Load(), rounds, leaseDenied.Load())
}

// The barrier's own contract, because everything above depends on it.
//
// Mutation: pass a constant instead of ep to reapPhase (or drop the parameter).
// Every barrier in this package then belongs to whichever reaper reaches the
// phase first, which is exactly the failure this hook produced in CI: a
// barrier fired for a foreign pathname, rewrote the endpoint its closure had
// captured, and the test's own reaper arrived to find the schedule already
// spent.
func TestReapBarrierReportsTheEndpointItFiredFor(t *testing.T) {
	dir := socketDir(t)
	epA := crashedRunnerSocket(t, dir, "1mw5c5n9.sock")
	epB := crashedRunnerSocket(t, dir, "18wnzse2.sock")

	var mu sync.Mutex
	var seen []string
	setReapBarrier(func(ep, phase string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ep+":"+phase)
	})
	t.Cleanup(func() { setReapBarrier(nil) })

	if outcome := reapStaleSocket(epA); !outcome.Reaped {
		t.Fatalf("reap of A declined: %s", outcome.Reason)
	}
	if outcome := reapStaleSocket(epB); !outcome.Reaped {
		t.Fatalf("reap of B declined: %s", outcome.Reason)
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	// The order is part of the contract: the pin is taken before the probe, so
	// "probed" always follows "lease-held" and precedes "before-remove".
	want := []string{
		epA + ":lease-held", epA + ":probed", epA + ":before-remove",
		epB + ":lease-held", epB + ":probed", epB + ":before-remove",
	}
	if len(got) != len(want) {
		t.Fatalf("barrier fired %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("barrier fired %v, want %v", got, want)
		}
	}
}

// And the filter built on it: a barrier scoped to one endpoint stays out of
// another reaper's way, including one running concurrently.
//
// Mutation: drop the endpoint comparison in onReapPhase.
func TestScopedReapBarrierIgnoresForeignEndpoints(t *testing.T) {
	dir := socketDir(t)
	mine := crashedRunnerSocket(t, dir, "1fiuuc06.sock")
	foreign := crashedRunnerSocket(t, dir, "1tve9qnd.sock")

	fired := onReapPhase(t, mine, "before-remove", func() {})

	// A concurrent reaper works through the foreign pathname while the barrier
	// is installed, exactly as a discovery scan worker or another test's reaper
	// would.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if outcome := reapStaleSocket(foreign); !outcome.Reaped {
			t.Errorf("reap of the foreign pathname declined: %s", outcome.Reason)
		}
	}()
	wg.Wait()

	if fired() {
		t.Fatal("the barrier fired for a foreign endpoint")
	}
	// It still fires for its own.
	if outcome := reapStaleSocket(mine); !outcome.Reaped {
		t.Fatalf("reap of my pathname declined: %s", outcome.Reason)
	}
	if !fired() {
		t.Fatal("the barrier never fired for its own endpoint")
	}
}
