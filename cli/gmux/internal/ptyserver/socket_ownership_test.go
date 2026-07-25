package ptyserver

// socket_ownership_test.go — mutation-grade tests for canonical socket
// pathname ownership (see packages/socklease for the protocol itself).
//
// The invariants under test:
//
//   - BindSocket owns the pathname for as long as it holds the lease, and no
//     second runner — lease-aware or not — can bind underneath it.
//   - Every removal of the pathname is leased and identity-checked, so an
//     exiting runner can never unlink a replacement runner's socket.
//   - /kill hands ownership over before it answers, because the daemon spawns
//     the restart replacement as soon as it sees the exit event.
//   - An ordinary exit leaves no pathname and no lock file behind.
//
// Each test names the production mutation it is designed to catch.

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/socklease"
)

// mustBind calls BindSocket and fails the test on error.
func mustBind(t *testing.T, sockPath string) *BoundSocket {
	t.Helper()
	b, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket(%s): %v", sockPath, err)
	}
	t.Cleanup(func() {
		_ = b.Listener.Close()
		_ = b.ReleaseOwnership()
	})
	return b
}

// mustIdent returns the identity of a socket pathname, failing if it is not a
// socket.
func mustIdent(t *testing.T, path string) socklease.Ident {
	t.Helper()
	id, ok := socklease.StatSocket(path)
	if !ok {
		t.Fatalf("%s is not a socket", path)
	}
	return id
}

// runToCompletion starts a server around a trivial command and waits for the
// PTY to finish, leaving the socket bound and the server ready for Shutdown.
func runToCompletion(t *testing.T, sockPath string) *Server {
	t.Helper()
	srv, err := New(Config{
		Command:    []string{"true"},
		Cwd:        "/tmp",
		Listener:   mustBind(t, sockPath),
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	select {
	case <-srv.PTYDone():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the child to exit")
	}
	return srv
}

// Mutation: drop the socklease.Acquire call (or ignore ErrHeld) in BindSocket.
func TestBindSocketExcludesSecondRunner(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	first := mustBind(t, sockPath)

	_, err := BindSocket(sockPath)
	if !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("second BindSocket = %v, want ErrSocketInUse", err)
	}
	// The first runner's socket is untouched by the rejected attempt.
	if _, ok := socklease.StatSocket(sockPath); !ok {
		t.Fatal("rejected BindSocket removed the incumbent's socket")
	}
	_ = first
}

// Mixed-version guard. A runner from before the lease protocol holds no lease,
// so the lease alone says "free" while a live listener still answers. Binding
// anyway would unlink a live older runner's socket and strand it.
//
// Mutation: delete the refusal probe in BindSocket.
func TestBindSocketRejectsLiveUnleasedSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "legacy.sock")

	// A "pre-lease runner": a live listener with no lock file anywhere.
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	before := mustIdent(t, sockPath)

	if _, err := BindSocket(sockPath); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("BindSocket over a live unleased socket = %v, want ErrSocketInUse", err)
	}
	after := mustIdent(t, sockPath)
	if after != before {
		t.Fatal("BindSocket rebound a pathname owned by a live unleased runner")
	}
	// The rejected attempt must not leave a lock file behind either: the
	// daemon reads its absence as "owner is not lease-aware, never reap".
	if _, err := os.Lstat(socklease.LockPath(sockPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected BindSocket left a lock file: %v", err)
	}
}

// crashedLeaseAwareSocket reproduces what a SIGKILLed runner leaves behind: a
// socket pathname nobody listens on, and its lock file, unheld (the kernel
// drops flock on death but never unlinks the file). It returns the stale
// socket's identity.
func crashedLeaseAwareSocket(t *testing.T, sockPath string) socklease.Ident {
	t.Helper()
	lease, err := socklease.Acquire(sockPath)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	id := mustIdent(t, sockPath)
	_ = ln.Close()
	// Drop the lease the way process death does, leaving the lock file.
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := os.WriteFile(socklease.LockPath(sockPath), nil, 0o600); err != nil {
		t.Fatalf("restore lock file: %v", err)
	}
	return id
}

// A pathname whose lease-aware owner crashed is reclaimable: we hold that
// owner's lease, so it is provably gone.
func TestBindSocketReplacesStaleSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "stale.sock")
	stale := crashedLeaseAwareSocket(t, sockPath)

	b := mustBind(t, sockPath)
	fresh := mustIdent(t, sockPath)
	if fresh.Same(stale) {
		t.Fatal("BindSocket reused the stale socket instead of rebinding")
	}
	// The bound socket is pinned, and the pin claims the pathname: that is what
	// authorises this runner -- and only this runner -- to unlink it later.
	if !b.pin.StillNamesIt() {
		t.Fatal("BoundSocket's pin does not claim the socket it bound")
	}
	if b.pin.Ident() != fresh {
		t.Fatalf("BoundSocket pin holds %s, bound socket is %s", b.pin.Ident(), fresh)
	}
}

// Mutation: remove the CreatedLockFile gate, so an occupant with no lease
// history is probed and unlinked like a lease-aware leftover.
//
// A pathname occupied with no lock file anywhere means no lease-aware
// generation ever owned it. Its occupant is therefore outside the protocol,
// excluded by nothing, and possibly *mid-startup right now* -- the one case a
// probe cannot see and a lease cannot serialise. It is never removed, whatever
// it is; the caller falls back to a fresh session id (ADR 0003).
func TestBindSocketRefusesUnleasedOccupant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{"stale socket from a pre-lease runner", func(t *testing.T, path string) {
			ln, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			_ = ln.Close()
		}},
		{"live socket from a pre-lease runner", func(t *testing.T, path string) {
			ln, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
		}},
		{"a file that is not a socket at all", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sockPath := filepath.Join(t.TempDir(), "occupied.sock")
			tc.prepare(t, sockPath)
			before, err := os.Lstat(sockPath)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := BindSocket(sockPath); !errors.Is(err, ErrSocketInUse) {
				t.Fatalf("BindSocket = %v, want ErrSocketInUse", err)
			}

			after, err := os.Lstat(sockPath)
			if err != nil {
				t.Fatalf("the occupant was removed: %v", err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("the occupant was replaced")
			}
			// And no lock file is left implying a lease-aware owner that
			// never existed -- the next attempt must reach the same verdict.
			if _, err := os.Lstat(socklease.LockPath(sockPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a lock file was left behind for an unleased pathname: %v", err)
			}
		})
	}
}

// Mutation: delete SetUnlinkOnClose(false) in BindSocket. Go's default unlinks
// the pathname from Close(), i.e. outside the lease and without an identity
// check — exactly the unconditional removal this design forbids.
func TestListenerCloseDoesNotUnlinkPathname(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	b := mustBind(t, sockPath)
	before := mustIdent(t, sockPath)

	if err := b.Listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	after, ok := socklease.StatSocket(sockPath)
	if !ok {
		t.Fatal("listener.Close() unlinked the pathname; removals must be leased and identity-checked")
	}
	if after != before {
		t.Fatal("pathname changed identity across listener.Close()")
	}
}

// Mutation: delete releaseSocketOwnership from Shutdown. The kernel does not
// unlink a bound pathname when a process exits, so the pathname would linger
// forever and every daemon scan would dial it and log a refusal.
func TestShutdownReleasesPathnameAndLease(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := runToCompletion(t, sockPath)

	if _, ok := socklease.StatSocket(sockPath); !ok {
		t.Fatal("socket missing before Shutdown")
	}
	srv.Shutdown()

	if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket pathname survived Shutdown: %v", err)
	}
	if _, err := os.Lstat(socklease.LockPath(sockPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file survived Shutdown: %v; the socket directory would grow one file per session", err)
	}
	// The lease is free: a replacement runner binds immediately.
	mustBind(t, sockPath)
}

func TestShutdownIsIdempotent(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := runToCompletion(t, sockPath)
	srv.Shutdown()
	srv.Shutdown()
	if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket pathname present after double Shutdown: %v", err)
	}
}

// The three-actor schedule that every earlier iteration of this design failed:
// an exiting runner must never unlink a pathname a replacement runner rebound.
//
//	A: /kill releases ownership (pathname unlinked, lease dropped)
//	B: replacement runner binds the same pathname
//	A: finishes draining and calls Shutdown
//
// Mutation: drop the identity check in BoundSocket.ReleaseOwnership (or the
// once-guard that keeps Shutdown from repeating /kill's removal). Either way
// A's Shutdown unlinks B's live socket and B becomes unreachable while alive.
func TestShutdownAfterOwnershipHandoverLeavesReplacementAlone(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := runToCompletion(t, sockPath)

	// A hands ownership over, exactly as the /kill handler does.
	srv.releaseSocketOwnership("test")

	// B takes it.
	replacement := mustBind(t, sockPath)
	replacementID := mustIdent(t, sockPath)
	// No skip if the inode was recycled: that is the case this guard exists
	// for, and A's pin is what makes it decidable.
	reused := replacementID.Dev == srv.bound.pin.Ident().Dev && replacementID.Ino == srv.bound.pin.Ident().Ino
	t.Logf("replacement reused the inode: %v", reused)

	// A finishes.
	srv.Shutdown()

	after, ok := socklease.StatSocket(sockPath)
	if !ok {
		t.Fatal("the old runner's Shutdown unlinked the replacement's live socket")
	}
	if after != replacementID {
		t.Fatalf("pathname identity changed: got %+v, want %+v", after, replacementID)
	}
	// And the replacement still holds its lease.
	if _, err := BindSocket(sockPath); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("replacement lost its lease: BindSocket = %v", err)
	}
	_ = replacement
}

// The lease excludes lease-aware runners, but not everything: a stray `rm`,
// or a runner from before the lease protocol, can still rebind the pathname
// under a live owner. The exiting owner must then leave it alone — unlinking
// on the strength of "this is my pathname" would disconnect a live listener.
//
// Mutation: replace lease.RemoveSocket(b.pin) with a bare os.Remove in
// BoundSocket.ReleaseOwnership, or make it compare identities instead of
// consulting the pin.
func TestShutdownLeavesUnleasedRebindAlone(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := runToCompletion(t, sockPath)

	// Somebody outside the protocol replaces the pathname with a live socket.
	if err := os.Remove(sockPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	intruder, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer intruder.Close()
	intruderID := mustIdent(t, sockPath)
	// Deliberately no skip when the inode is recycled: an intruder that carries
	// the very same device and inode is exactly the case a pin decides and an
	// identity comparison gets wrong.
	t.Logf("intruder reused the inode: %v",
		intruderID.Dev == srv.bound.pin.Ident().Dev && intruderID.Ino == srv.bound.pin.Ident().Ino)

	srv.Shutdown()

	after, ok := socklease.StatSocket(sockPath)
	if !ok {
		t.Fatal("Shutdown unlinked a pathname that no longer named its own socket")
	}
	if after != intruderID {
		t.Fatalf("pathname identity changed: got %+v, want %+v", after, intruderID)
	}
}

// The lease must not outlive the runner through its PTY child. If the lock
// descriptor were inherited across exec, a child that ignores SIGHUP would
// keep the pathname leased long after the runner exited, and the restart
// replacement would be pushed onto a fresh session id.
//
// Mutation: clear FD_CLOEXEC on the lease descriptor (or pass it in
// ExtraFiles).
func TestPTYChildDoesNotInheritLease(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv, err := New(Config{
		// Ignore SIGHUP so the child certainly outlives the runner's teardown.
		Command:    []string{"bash", "-c", "trap '' HUP; sleep 30"},
		Cwd:        "/tmp",
		Listener:   mustBind(t, sockPath),
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	childPID := srv.cmd.Process.Pid
	defer func() { _ = syscall.Kill(-childPID, syscall.SIGKILL) }()

	srv.Shutdown()

	// The child is still running...
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Skipf("child exited before the check (%v); inheritance cannot be observed", err)
	}
	// ...and yet the lease is free.
	b, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket while the PTY child is alive = %v; the child inherited the lease", err)
	}
	_ = b.Listener.Close()
	_ = b.ReleaseOwnership()
}

// legacyRunner is a runner from before the lease protocol: it takes a socket
// pathname the old way (blind unlink, then listen) and holds no lease.
type legacyRunner struct {
	ln    *net.UnixListener
	ident socklease.Ident
}

func startLegacyRunner(t *testing.T, sockPath string) *legacyRunner {
	t.Helper()
	_ = os.Remove(sockPath) // exactly what the pre-lease BindSocket did
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("legacy listen: %v", err)
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ul.Close() })
	return &legacyRunner{ln: ul, ident: mustIdent(t, sockPath)}
}

// onBindPhase installs BindSocket's barrier for one pathname and one test.
//
// Scoping on the pathname is not tidiness: binds run concurrently, in this
// package's goroutines and in other tests', so an unfiltered barrier is driven
// by whichever bind reaches the phase first -- the same defect the daemon's reap
// barrier produced in CI. fired reports whether the barrier ever ran, so a test
// must prove the schedule it asserts about actually happened.
func onBindPhase(t *testing.T, sockPath, phase string, fn func()) (fired func() bool) {
	t.Helper()
	var ran atomic.Bool
	var once sync.Once
	setBindBarrier(func(gotPath, gotPhase string) {
		if gotPath != sockPath || gotPhase != phase {
			return
		}
		once.Do(func() {
			ran.Store(true)
			fn()
		})
	})
	t.Cleanup(func() { setBindBarrier(nil) })
	return ran.Load
}

// The mixed-version property, driven into every window BindSocket has.
//
// A pre-lease runner is excluded by nothing: it holds no lease, and it can
// start at any instant, including between our probe and our unlink. The only
// safe rule is to never unlink a pathname we cannot prove belonged to a
// lease-aware generation -- and to re-check identity even when we can, because
// the pathname may have changed hands since we looked.
//
// Mutation: any of the three guards -- the CreatedLockFile gate, the probe, or
// the identity-checked removal -- lets one of these subtests unlink a live
// pre-lease runner's socket.
func TestBindSocketNeverUnlinksALivePreLeaseRunner(t *testing.T) {
	for _, tc := range []struct {
		name string
		// phase names the BindSocket window in which the pre-lease runner
		// takes the pathname.
		phase string
		// leaseHistory seeds a lock file (and a stale socket) so BindSocket
		// takes the "a lease-aware generation owned this" branch.
		leaseHistory bool
	}{
		{name: "starts while we hold a fresh lease", phase: "lease-held"},
		{name: "starts while we hold an inherited lease", phase: "lease-held", leaseHistory: true},
		{name: "takes over after our bind fails", phase: "address-in-use", leaseHistory: true},
		{name: "takes over just before our probe", phase: "before-probe", leaseHistory: true},
		// The window the pin exists for: the probe has already answered
		// "nothing here", and the pathname changes hands before the unlink.
		// Getting this one wrong -- by pinning after the probe rather than
		// before it -- unlinks a live, unprobed runner.
		{name: "takes over right after our probe", phase: "probed", leaseHistory: true},
		{name: "takes over between our probe and our unlink", phase: "before-remove", leaseHistory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sockPath := filepath.Join(t.TempDir(), "contested.sock")
			if tc.leaseHistory {
				crashedLeaseAwareSocket(t, sockPath)
			}

			var legacy *legacyRunner
			fired := onBindPhase(t, sockPath, tc.phase, func() {
				legacy = startLegacyRunner(t, sockPath)
			})

			b, err := BindSocket(sockPath)
			if !fired() {
				t.Fatalf("BindSocket never reached the %q phase", tc.phase)
			}
			if err == nil {
				_ = b.Listener.Close()
				_ = b.ReleaseOwnership()
				t.Fatalf("BindSocket bound over a live pre-lease runner")
			}
			if !errors.Is(err, ErrSocketInUse) {
				t.Fatalf("BindSocket = %v, want ErrSocketInUse", err)
			}

			// The invariant: the pre-lease runner still owns its pathname.
			current, ok := socklease.StatSocket(sockPath)
			if !ok {
				t.Fatal("the live pre-lease runner's pathname was unlinked")
			}
			if current != legacy.ident {
				t.Fatalf("the pathname was rebound under a live pre-lease runner: %+v -> %+v",
					legacy.ident, current)
			}
			// ...and it is still reachable, which is the point of not
			// unlinking it.
			conn, dialErr := net.DialTimeout("unix", sockPath, 2*time.Second)
			if dialErr != nil {
				t.Fatalf("the pre-lease runner is no longer reachable at its pathname: %v", dialErr)
			}
			_ = conn.Close()
		})
	}
}

// The daemon's reaper holds the lease across its inspection. A runner that
// starts in that window must wait it out, not fall back to a fresh session id:
// losing a resume identity to a bookkeeping sweep is the exact failure the
// /kill early release exists to prevent, arriving through another door.
//
// Mutation: use socklease.Acquire instead of AcquireWait in BindSocket.
func TestBindSocketWaitsOutAReaperHoldingTheLease(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "contested.sock")
	crashedLeaseAwareSocket(t, sockPath)

	// Stand in for the reaper: hold the lease, then let go.
	reaper, err := socklease.AcquireExisting(sockPath)
	if err != nil {
		t.Fatalf("reaper acquire: %v", err)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		// The reaper puts a pathname it merely inspected back as it found it.
		_ = reaper.ReleaseKeepingLockFile()
	}()

	b, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket = %v; it gave up while the reaper held the lease", err)
	}
	defer func() { _ = b.Listener.Close(); _ = b.ReleaseOwnership() }()
	if got := b.sockPath; got != sockPath {
		t.Fatalf("bound %s, want %s", got, sockPath)
	}
}

// Mutation: replace socklease.ProbeRefused with an "any dial failure means
// nothing answers" probe (the old probeSocket).
//
// A live occupant whose socket cannot be connected to is not a dead one. Here
// the socket's mode denies the connect, which is deterministic; a full accept
// backlog is the same situation arriving by accident. The runner and the daemon
// must agree on this, or a socket one side spares is unlinked by the other.
func TestBindSocketDoesNotRemoveALeftoverOnAnAmbiguousProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: no mode denies a connect")
	}
	sockPath := filepath.Join(t.TempDir(), "unreachable.sock")
	// Lease history, so BindSocket is in the branch that may remove.
	crashedLeaseAwareSocket(t, sockPath)
	if err := os.Remove(sockPath); err != nil {
		t.Fatal(err)
	}

	// A live listener whose socket cannot be dialled: the probe learns nothing.
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	defer ln.Close()
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := os.Chmod(sockPath, 0o000); err != nil {
		t.Fatal(err)
	}
	before := mustIdent(t, sockPath)

	// The probe must actually be ambiguous for this test to mean anything.
	probeErr := socklease.ProbeRefused(sockPath, probeTimeout)
	if probeErr == nil {
		t.Fatal("an unreachable live socket was reported as refused")
	}
	if errors.Is(probeErr, socklease.ErrSocketLive) {
		t.Skip("the connect succeeded despite the mode; the probe is not ambiguous here")
	}

	if _, err := BindSocket(sockPath); err == nil {
		t.Fatal("BindSocket bound over a live listener its probe could not reach")
	}
	after, ok := socklease.StatSocket(sockPath)
	if !ok {
		t.Fatal("the unreachable live listener's pathname was unlinked")
	}
	if after != before {
		t.Fatal("the pathname was rebound under a live listener")
	}
}

// Mutation: replace socklease.RequireOwnedDir with a plain MkdirAll in
// BindSocket.
//
// Every guarantee in this package is relative to a directory only this user can
// write in. Binding into one that others can write in is not a lesser
// guarantee, it is none, so the premise is established where the directory is
// created rather than assumed.
func TestBindSocketRequiresAPrivateSocketDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "sess.sock")

	b, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket: %v", err)
	}
	t.Cleanup(func() { _ = b.Listener.Close(); _ = b.ReleaseOwnership() })

	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket directory mode = %o, want 700: a directory others can write in makes every lease in it meaningless", perm)
	}
}
