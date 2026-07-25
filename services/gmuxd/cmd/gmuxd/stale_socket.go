package main

// Stale runner socket reaping.
//
// A runner that dies without unlinking its canonical pathname (SIGKILL, OOM,
// power loss, or any version that predates the ownership lease) leaves a file
// that answers every connection attempt with ECONNREFUSED. Discovery then
// dials it, fails, and logs, once per endpoint per tick, forever.
//
// Deleting such a file is the dangerous half. The pathname is reusable, so
// every judgement of the form "nothing was listening a moment ago" is a
// TOCTOU: a fresh runner can bind the pathname between the probe and the
// unlink, and the unlink then silently disconnects it. The daemon therefore
// never reaps on the strength of a probe. It takes the runner's own ownership
// lease (packages/socklease) non-blocking and holds it across the identity
// check, the liveness probe and the unlink. Acquiring the lease is proof that
// no lease-aware owner is alive; holding it is proof that none can appear
// mid-sequence.
//
// Everything else is declined. The predicate below never deletes when:
//
//   - the pathname is outside the trusted socket directory,
//   - no lock file exists (the owner predates the lease protocol -- absence
//     of proof of death is not proof of death),
//   - the lease is held (the owner is alive),
//   - the pathname is not a socket, or changed identity under the sequence,
//   - the probe returns anything other than ECONNREFUSED: a timeout, a
//     permission error, or a successful connection all mean "not provably
//     abandoned".

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/socklease"
)

// staleProbeTimeout bounds the connect used to confirm that nothing is
// listening. It runs while the lease is held, so it also bounds how long a
// replacement runner could be kept waiting for the lease.
const staleProbeTimeout = 250 * time.Millisecond

// reapOutcome describes what the reaper did, for transition logging.
type reapOutcome struct {
	Reaped bool
	// Reason is a short, allocation-free-ish explanation of a decline. Empty
	// when the socket was reaped.
	Reason string
}

// trustedSocketDir reports whether ep is a direct child of the canonical
// runner socket directory.
//
// Legacy socket directories (paths.LegacySessionSocketDirs) are deliberately
// excluded even though discovery still enumerates them: they are read-mostly
// compatibility shims for runners that predate this daemon, exactly the
// population that holds no lease, and the daemon has no business deleting
// files in a directory whose ownership rules it does not define.
func trustedSocketDir(ep string) bool {
	dir, err := filepath.Abs(filepath.Dir(ep))
	if err != nil {
		return false
	}
	trusted, err := filepath.Abs(paths.SessionSocketDir())
	if err != nil {
		return false
	}
	if dir != trusted {
		return false
	}
	// The canonical pathname is not enough on its own: a lease inside a
	// directory other users can write in proves nothing (see socklease's threat
	// model). Require the premise here too, rather than assuming the runner
	// established it.
	return socklease.RequireOwnedDir(dir) == nil
}

// reapBarrier is a barrier hook for the reaper's schedule tests: it runs at each
// named point inside the leased inspect-and-unlink sequence, which is the window
// a racing runner must be excluded from. It is unset in production.
//
// It is deliberately awkward in two ways, both of which are the point.
//
// It receives the endpoint, because the reaper is not a single-threaded thing: a
// discovery scan reaps in per-endpoint workers, the stress test runs three
// reapers at once, and other tests leave reapers running against their own
// pathnames. A hook that fired for any endpoint would be driven by whichever
// reaper reached the phase first, so a test's barrier would perturb a pathname
// it does not own -- or, worse, fire before its own reaper arrived. Tests filter
// on their exact endpoint (see onReapPhase).
//
// It is behind a mutex, because those same reapers read it from goroutines that
// can outlive the statement that installed it. A plain variable is a data race
// the moment a background scan overlaps a test's install or cleanup, and the
// hook is copied out before it is called so a barrier that blocks -- which is
// what a barrier is for -- never holds the lock.
var reapBarrier struct {
	mu sync.Mutex
	fn func(ep, phase string)
}

func reapPhase(ep, phase string) {
	reapBarrier.mu.Lock()
	fn := reapBarrier.fn
	reapBarrier.mu.Unlock()
	if fn != nil {
		fn(ep, phase)
	}
}

// setReapBarrier installs the barrier hook, replacing any previous one.
func setReapBarrier(fn func(ep, phase string)) {
	reapBarrier.mu.Lock()
	reapBarrier.fn = fn
	reapBarrier.mu.Unlock()
}

// reapStaleSocket removes ep if -- and only if -- it is provably an abandoned
// runner socket. It returns whether it unlinked anything and why not.
func reapStaleSocket(ep string) reapOutcome {
	if !trustedSocketDir(ep) {
		return reapOutcome{Reason: "outside the trusted socket directory"}
	}
	lease, err := socklease.AcquireExisting(ep)
	switch {
	case errors.Is(err, socklease.ErrNoLockFile):
		return reapOutcome{Reason: "no ownership lease file (runner predates the lease protocol)"}
	case errors.Is(err, socklease.ErrHeld):
		return reapOutcome{Reason: "ownership lease is held (a live runner, or another reaper mid-sequence)"}
	case err != nil:
		return reapOutcome{Reason: fmt.Sprintf("ownership lease unavailable: %v", err)}
	}
	// From here the lease is ours: no lease-aware runner can bind this
	// pathname until we release it.
	//
	// How we give it back matters. The lock file is the only evidence that a
	// pathname's leftover socket belonged to a lease-aware generation, and
	// that evidence is what makes the leftover reclaimable at all -- both by
	// this reaper on a later pass and by a runner resuming that session id.
	// Removing it while merely *inspecting* would convert a recoverable
	// crashed-runner socket into a permanently unreclaimable one: every future
	// attempt would see no lock file, conclude "not provably ours", and refuse.
	//
	// So the lock file is erased only when we learned something that makes it
	// false or pointless -- the pathname is gone, is not a socket, or is held
	// by a live unleased occupant -- or when the reap succeeded and there is
	// nothing left to reclaim. Every "learned nothing" decline puts it back
	// exactly as it was found.
	learnedNothing := true
	defer func() {
		if learnedNothing {
			_ = lease.ReleaseKeepingLockFile()
			return
		}
		_ = lease.Release()
	}()
	reapPhase(ep, "lease-held")

	// Pin the socket before probing it. The unlink below has to be conditional
	// on *the file we probed*, and a pathname's device and inode do not settle
	// that: on a filesystem that recycles inode numbers, a live replacement can
	// present the very same pair as the leftover it replaced.
	before, pinErr := socklease.PinSocket(ep)
	isSocket := pinErr == nil
	if isSocket {
		defer before.Close()
	}
	if !isSocket {
		// Gone, or never a socket: no leftover to reclaim, so the lock file is
		// pointless. Removing it is also how lock files left by a runner that
		// died between creating one and binding get cleaned up.
		learnedNothing = false
		return reapOutcome{Reason: "not a socket"}
	}
	probeErr := probeRefused(ep, before)
	if probeErr != nil {
		// Only one of these outcomes teaches us anything about the occupant:
		// somebody answered, which proves the pathname is held by a runner
		// without a lease, so the lease history describes a dead generation
		// rather than the occupant. A timeout or any other failure teaches us
		// nothing at all.
		learnedNothing = !errors.Is(probeErr, socklease.ErrSocketLive)
		return reapOutcome{Reason: probeErr.Error()}
	}
	reapPhase(ep, "before-remove")
	if err := lease.RemoveSocket(before); err != nil {
		// The pathname changed under us: whatever is there now is not what we
		// probed, and we know nothing about it.
		return reapOutcome{Reason: fmt.Sprintf("declined removal: %v", err)}
	}
	learnedNothing = false
	return reapOutcome{Reaped: true}
}

// probeRefused reports whether nothing is listening at the pathname the pin
// holds, using the same predicate the runner uses before it clears a leftover of
// its own: only an explicit refusal counts, and a timeout is a wedged owner
// rather than a dead one. Sharing the predicate is the point -- an asymmetry here
// is how a live socket gets unlinked by one side and spared by the other.
//
// It takes the pin rather than the pathname because pin-then-probe is the
// load-bearing order, and a signature is a better guardian of an order than a
// comment: there is nothing to pass if the pin has not been taken yet.
func probeRefused(ep string, pin *socklease.Pin) error {
	err := socklease.ProbeRefusedPinned(pin, staleProbeTimeout)
	// The barrier fires here, at the end of the probe step, rather than at the
	// call site. That placement is deliberate: it is the only point a caller
	// cannot get in front of. A caller that pinned *after* probing would have
	// its pin land after this line, so a test can take the pathname over here
	// and watch such a caller pin -- and then unlink -- the newcomer.
	reapPhase(ep, "probed")
	return err
}
