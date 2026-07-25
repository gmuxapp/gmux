package socklease

// Mutation-grade tests for the socket ownership lease.
//
// Each test names the production mutation it is designed to catch, so a
// reviewer can delete the referenced line and watch exactly that test fail.

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func tempSock(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// listenUnix binds a real Unix socket and disables Go's unlink-on-close so the
// test controls every pathname removal explicitly, exactly like the runner.
func listenUnix(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = ul.Close() })
	return ul
}

// Mutation: delete the syscall.Flock call in acquire.
func TestAcquireExcludesSecondHolder(t *testing.T) {
	sock := tempSock(t, "a.sock")
	l1, err := Acquire(sock)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l1.Release()

	if _, err := Acquire(sock); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire = %v, want ErrHeld", err)
	}
	if _, err := AcquireExisting(sock); !errors.Is(err, ErrHeld) {
		t.Fatalf("second AcquireExisting = %v, want ErrHeld", err)
	}
}

// Mutation: make Release skip the flock LOCK_UN / Close.
func TestAcquireSucceedsAfterRelease(t *testing.T) {
	sock := tempSock(t, "a.sock")
	l1, err := Acquire(sock)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release must be idempotent: %v", err)
	}
}

// Mutation: give AcquireExisting the O_CREATE flag. A pathname whose owner
// predates the lease protocol must stay un-leased, because the daemon treats a
// missing lock file as "not provably dead".
func TestAcquireExistingNeverCreatesLockFile(t *testing.T) {
	sock := tempSock(t, "legacy.sock")
	if _, err := AcquireExisting(sock); !errors.Is(err, ErrNoLockFile) {
		t.Fatalf("AcquireExisting = %v, want ErrNoLockFile", err)
	}
	if _, err := os.Lstat(LockPath(sock)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("AcquireExisting created a lock file: %v", err)
	}
}

// Mutation: drop the os.Remove of the lock file in Release. Without it the
// socket directory accumulates one lock file per session ever started, which
// the daemon then re-reads on every discovery tick.
func TestReleaseUnlinksLockFile(t *testing.T) {
	sock := tempSock(t, "a.sock")
	l, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Lstat(l.LockPath()); err != nil {
		t.Fatalf("lock file missing while lease held: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Lstat(l.LockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file survived Release: %v", err)
	}
}

func TestLockFileIsOwnerOnly(t *testing.T) {
	sock := tempSock(t, "a.sock")
	l, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	fi, err := os.Lstat(l.LockPath())
	if err != nil {
		t.Fatalf("lstat lock: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("lock file mode = %o, want 600", perm)
	}
}

// The lease must not survive an exec: the runner's PTY child would otherwise
// inherit the descriptor and hold the socket pathname hostage for as long as
// the child lives, long after the runner itself exited.
func TestLeaseDescriptorIsCloseOnExec(t *testing.T) {
	sock := tempSock(t, "a.sock")
	l, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, l.f.Fd(), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("fcntl F_GETFD: %v", errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("lease descriptor is not close-on-exec; an exec'd child would inherit the lease")
	}
}

// Mutation: delete the verify-after-lock branch in acquire (the sameFile
// check). This is the schedule that makes unlinking lock files safe:
//
//  1. holder H owns the lease on inode X.
//  2. acquirer A opens X (still the file the lock pathname names).
//  3. H releases: X is unlinked, the lock is dropped.
//  4. replacement R creates a fresh lock file Y and takes the lease.
//  5. A's flock on X now succeeds -- X is an orphaned inode nobody else
//     locks -- so without the verify, A and R would both believe they own the
//     pathname.
func TestAcquireRejectsReplacedLockFile(t *testing.T) {
	sock := tempSock(t, "a.sock")

	holder, err := Acquire(sock)
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}

	var replacement *Lease
	defer func() {
		if replacement != nil {
			_ = replacement.Release()
		}
	}()

	fired := false
	testAfterOpen = func() {
		if fired {
			return // only perturb the first attempt; the retry must succeed... or not
		}
		fired = true
		if err := holder.Release(); err != nil {
			t.Errorf("holder Release: %v", err)
		}
		replacement, err = Acquire(sock)
		if err != nil {
			t.Errorf("replacement Acquire: %v", err)
		}
	}
	defer func() { testAfterOpen = nil }()

	got, err := Acquire(sock)
	if err == nil {
		_ = got.Release()
		t.Fatal("Acquire returned a lease for a lock file that had been replaced; " +
			"the verify-after-lock check is missing and two owners can coexist")
	}
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire = %v, want ErrHeld (the replacement owns the lease)", err)
	}
	if !fired {
		t.Fatal("barrier hook never ran")
	}
}

// Mutation: make RemoveSocket unlink unconditionally, or check an identity
// instead of a pin.
//
// The three-actor schedule an old owner must never win: a live replacement
// rebound the pathname, and unlinking it would leave a running runner
// unreachable. Note what this test does *not* do: it does not require the
// replacement to have a different inode. It cannot -- see
// TestPinSurvivesImmediateInodeReuse -- and that is the whole reason the guard
// is a pin.
func TestRemoveSocketLeavesReboundPathname(t *testing.T) {
	sock := tempSock(t, "a.sock")

	old := listenUnix(t, sock)
	lease, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	pin, err := lease.PinSocket()
	if err != nil {
		t.Fatalf("PinSocket: %v", err)
	}
	defer pin.Close()

	// The pathname is rebound by a replacement runner.
	_ = old.Close()
	if err := os.Remove(sock); err != nil {
		t.Fatalf("remove: %v", err)
	}
	replacement := listenUnix(t, sock)
	replacementID, ok := StatSocket(sock)
	if !ok {
		t.Fatal("replacement socket not found")
	}

	if err := lease.RemoveSocket(pin); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("RemoveSocket = %v, want ErrIdentityChanged (inode reused: %v)",
			err, replacementID.Dev == pin.Ident().Dev && replacementID.Ino == pin.Ident().Ino)
	}
	after, ok := StatSocket(sock)
	if !ok {
		t.Fatal("RemoveSocket unlinked the replacement's live socket")
	}
	if after != replacementID {
		t.Fatal("the replacement's pathname was disturbed")
	}
	_ = replacement
}

// The property the whole Ident/Pin distinction exists for, driven against
// immediate inode reuse rather than around it.
//
// A pathname is unlinked and rebound in the same breath, many times. Some
// filesystems hand back the inode just freed -- GitHub's runners do, this
// project's development machines do not -- so the replacement's device and
// inode may equal the original's exactly. The identity comparison this code
// used to make would then say "unchanged" and unlink a live socket. The pin says
// otherwise every single time, because it holds the file rather than describing
// it.
//
// Mutation: compare identities in RemoveSocket instead of consulting the pin.
// On a filesystem that recycles, this test then unlinks live sockets; on one
// that does not, it still passes -- which is exactly why the guard must not be
// an identity comparison, and why this test reports what it observed.
func TestPinSurvivesImmediateInodeReuse(t *testing.T) {
	sock := tempSock(t, "reuse.sock")
	lease, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()

	const rounds = 200
	reused, exact := 0, 0
	for round := range rounds {
		original := listenUnix(t, sock)
		pin, err := lease.PinSocket()
		if err != nil {
			t.Fatalf("round %d: PinSocket: %v", round, err)
		}
		if pin.Exact() {
			exact++
		}
		originalID := pin.Ident()

		// The replacement, as abruptly as a filesystem will allow.
		_ = original.Close()
		if err := os.Remove(sock); err != nil {
			t.Fatalf("round %d: remove: %v", round, err)
		}
		replacement := listenUnix(t, sock)
		replacementID, ok := StatSocket(sock)
		if !ok {
			t.Fatalf("round %d: replacement socket not found", round)
		}
		if replacementID.Dev == originalID.Dev && replacementID.Ino == originalID.Ino {
			reused++
			// On a recycling filesystem the identities may be equal in full,
			// stamp included: the kernel's file-timestamp clock is coarse, so
			// two creations microseconds apart share it. That is the case the
			// pin exists for, so assert it rather than skip it.
			if replacementID.Same(originalID) && !pin.Exact() {
				t.Fatalf("round %d: this platform can neither pin nor distinguish a recycled inode; "+
					"the removal guard has nothing to stand on", round)
			}
		}

		// The guard: the pathname no longer names the pinned file, whatever the
		// numbers say.
		if pin.StillNamesIt() {
			t.Fatalf("round %d: the pin still claims the pathname after a rebind (original %s, now %s)",
				round, originalID, replacementID)
		}
		if err := lease.RemoveSocket(pin); !errors.Is(err, ErrIdentityChanged) {
			t.Fatalf("round %d: RemoveSocket = %v, want ErrIdentityChanged", round, err)
		}
		if _, ok := StatSocket(sock); !ok {
			t.Fatalf("round %d: a live replacement's socket was unlinked", round)
		}

		_ = pin.Close()
		_ = replacement.Close()
		if err := os.Remove(sock); err != nil {
			t.Fatalf("round %d: cleanup: %v", round, err)
		}
	}
	t.Logf("%d/%d rounds reused the inode immediately; %d/%d pins were exact handles",
		reused, rounds, exact, rounds)
}

// And the positive case: a pin that still names its file authorises the unlink.
func TestRemoveSocketRemovesOwnPathname(t *testing.T) {
	sock := tempSock(t, "a.sock")
	listenUnix(t, sock)
	lease, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	pin, err := lease.PinSocket()
	if err != nil {
		t.Fatalf("PinSocket: %v", err)
	}
	defer pin.Close()

	if !pin.StillNamesIt() {
		t.Fatal("a pin taken on an untouched socket does not claim its pathname")
	}
	if err := lease.RemoveSocket(pin); err != nil {
		t.Fatalf("RemoveSocket: %v", err)
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still present: %v", err)
	}
	// Idempotent: a second removal of a vanished pathname is not an error.
	if err := lease.RemoveSocket(pin); err != nil {
		t.Fatalf("second RemoveSocket: %v", err)
	}
}

// Mutation: let RemoveSocket act without a pin, or with one for another
// pathname. "I don't know what is there" must never authorise an unlink.
func TestRemoveSocketRejectsAnAbsentOrForeignPin(t *testing.T) {
	sock := tempSock(t, "a.sock")
	listenUnix(t, sock)
	lease, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()

	if err := lease.RemoveSocket(nil); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("RemoveSocket(nil) = %v, want ErrIdentityChanged", err)
	}
	other := tempSock(t, "other.sock")
	listenUnix(t, other)
	foreign, err := PinSocket(other)
	if err != nil {
		t.Fatalf("PinSocket: %v", err)
	}
	defer foreign.Close()
	if err := lease.RemoveSocket(foreign); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("RemoveSocket(foreign pin) = %v, want ErrIdentityChanged", err)
	}
	for _, p := range []string{sock, other} {
		if _, ok := StatSocket(p); !ok {
			t.Fatalf("%s was unlinked", p)
		}
	}
}

// A hardlink is the other way a pathname can stop being the only name for a
// file, and on a recycling filesystem it is indistinguishable by number.
//
// Mutation: accept any link count in Pin.StillNamesIt.
func TestPinRejectsAHardlinkedFile(t *testing.T) {
	sock := tempSock(t, "a.sock")
	listenUnix(t, sock)
	pin, err := PinSocket(sock)
	if err != nil {
		t.Fatalf("PinSocket: %v", err)
	}
	defer pin.Close()
	if !pin.Exact() {
		t.Skip("this platform cannot pin a socket; link counts are not observable")
	}
	if !pin.StillNamesIt() {
		t.Fatal("a fresh pin does not claim its pathname")
	}

	alias := sock + ".alias"
	if err := os.Link(sock, alias); err != nil {
		t.Skipf("this filesystem does not support hard-linking a socket: %v", err)
	}
	defer os.Remove(alias)
	if pin.StillNamesIt() {
		t.Fatal("a pin claims a pathname that is no longer the file's only name")
	}
}

// Mutation: drop the ModeSocket check (or switch Lstat to Stat) in StatSocket.
// Both would let the daemon unlink a regular file or follow a symlink out of
// the trusted socket directory.
func TestStatSocketRejectsNonSockets(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "regular.sock")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := StatSocket(regular); ok {
		t.Error("a regular file was reported as a socket")
	}

	real := filepath.Join(dir, "real.sock")
	listenUnix(t, real)
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, ok := StatSocket(link); ok {
		t.Error("a symlink to a socket was reported as a socket (Lstat became Stat?)")
	}

	if _, ok := StatSocket(filepath.Join(dir, "missing.sock")); ok {
		t.Error("a missing path was reported as a socket")
	}

	dirPath := filepath.Join(dir, "sub.sock")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ok := StatSocket(dirPath); ok {
		t.Error("a directory was reported as a socket")
	}
}

// Mutation: let Known() ignore the stamp, or let Same() compare only device and
// inode.
//
// An identity without a creation stamp is exactly the number-only identity that
// inode reuse defeats, so it counts as unknown; and two identities read from
// different clocks are not comparable at all.
func TestIdentKnownAndSame(t *testing.T) {
	var zero Ident
	if zero.Known() {
		t.Error("zero Ident must be unknown")
	}
	if zero.Same(zero) {
		t.Error("unknown identities must never compare equal")
	}

	numbersOnly := Ident{Dev: 1, Ino: 2}
	if numbersOnly.Known() {
		t.Error("an identity with no creation stamp must count as unknown")
	}
	if numbersOnly.Same(numbersOnly) {
		t.Error("an unstamped identity must not match itself")
	}

	a := Ident{Dev: 1, Ino: 2, StampSec: 7, StampNsec: 8, Stamp: StampChange}
	if !a.Known() {
		t.Error("a fully observed identity must be known")
	}
	if !a.Same(Ident{Dev: 1, Ino: 2, StampSec: 7, StampNsec: 8, Stamp: StampChange}) {
		t.Error("equal known identities must match")
	}
	for _, other := range []Ident{
		{Dev: 1, Ino: 3, StampSec: 7, StampNsec: 8, Stamp: StampChange}, // recycled number
		{Dev: 1, Ino: 2, StampSec: 8, StampNsec: 8, Stamp: StampChange}, // reincarnation
		{Dev: 1, Ino: 2, StampSec: 7, StampNsec: 9, Stamp: StampChange}, // reincarnation, ns apart
		{Dev: 1, Ino: 2, StampSec: 7, StampNsec: 8, Stamp: StampBirth},  // different clock
		{Dev: 2, Ino: 2, StampSec: 7, StampNsec: 8, Stamp: StampChange}, // different device
	} {
		if a.Same(other) {
			t.Errorf("%s must not match %s", a, other)
		}
	}
	if a.Same(zero) {
		t.Error("known must not match unknown")
	}
	if got := a.String(); got == "" || got == "unknown-identity" {
		t.Errorf("String() = %q", got)
	}
}

// A real socket's identity must be complete: this is the platform helper's
// contract, and the reason an unsupported platform degrades to unknown rather
// than to numbers.
func TestStatSocketReportsACompleteIdentity(t *testing.T) {
	sock := tempSock(t, "a.sock")
	listenUnix(t, sock)
	id, ok := StatSocket(sock)
	if !ok {
		t.Fatal("StatSocket on a real socket reported not-a-socket")
	}
	if !id.Known() {
		t.Fatalf("identity %s is incomplete; inode numbers alone cannot survive reuse", id)
	}
	if id.Stamp == StampNone || (id.StampSec == 0 && id.StampNsec == 0) {
		t.Fatalf("identity %s carries no creation stamp", id)
	}
	if id.Stamp == StampBirth && runtime.GOOS == "linux" {
		t.Error("linux reports a birth time it cannot read without statx")
	}
}

// Mutation: drop the sameFile condition guarding the unlink in Release.
//
// Three actors, and the substitution happens before A releases -- no
// instruction-level race needed:
//
//  1. A holds the lease on lock inode LA.
//  2. Something replaces the lock pathname; B acquires the replacement LB.
//  3. A releases. An unconditional unlink here removes *B's* lock pathname,
//     so C can create a third lock file and take a lease while B still holds
//     one. Two live leases over one socket pathname is precisely the state
//     this package exists to make impossible.
func TestReleaseDoesNotUnlinkASubstitutedLockFile(t *testing.T) {
	sock := tempSock(t, "a.sock")

	a, err := Acquire(sock)
	if err != nil {
		t.Fatalf("A Acquire: %v", err)
	}
	// The lock pathname is substituted out from under A.
	if err := os.Remove(a.LockPath()); err != nil {
		t.Fatal(err)
	}
	b, err := Acquire(sock)
	if err != nil {
		t.Fatalf("B Acquire: %v", err)
	}
	defer b.Release()
	bLock, err := os.Lstat(b.LockPath())
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Release(); err != nil {
		t.Fatalf("A Release: %v", err)
	}

	after, err := os.Lstat(b.LockPath())
	if err != nil {
		t.Fatalf("A's Release unlinked B's lock file: %v", err)
	}
	if !os.SameFile(bLock, after) {
		t.Fatal("A's Release replaced B's lock file")
	}
	// And B's lease still excludes everyone.
	if _, err := Acquire(sock); !errors.Is(err, ErrHeld) {
		t.Fatalf("C acquired a second lease for the same pathname: %v", err)
	}
}

// Mutation: drop O_NOFOLLOW from the lock open.
//
// A symlink at the lock pathname must not become a lease on -- and later an
// unlink of -- whatever it points at.
func TestAcquireRefusesToFollowASymlinkedLockPath(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "a.sock")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("not a lock file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, LockPath(sock)); err != nil {
		t.Fatal(err)
	}

	lease, err := Acquire(sock)
	if err == nil {
		_ = lease.Release()
		t.Fatal("Acquire followed a symlinked lock pathname")
	}
	// Refused at the open, not merely defeated by the post-lock verification:
	// without O_NOFOLLOW the victim is opened and flocked 16 times before the
	// inode comparison rejects it, which is a denial of service dressed up as
	// safety.
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Acquire = %v, want ELOOP (the open must refuse the symlink)", err)
	}
	if _, statErr := os.Lstat(victim); statErr != nil {
		t.Fatalf("the symlink target was disturbed: %v", statErr)
	}
}

// CreatedLockFile is the runner's evidence about who owns a pathname it could
// not bind, so it has to be exactly right in both directions.
//
// Mutation: always report true (or always false) from CreatedLockFile.
func TestCreatedLockFileDistinguishesFoundFromMade(t *testing.T) {
	sock := tempSock(t, "a.sock")

	first, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}
	if !first.CreatedLockFile() {
		t.Error("the first acquisition created the lock file but did not report it")
	}
	// Release keeps nothing behind, so the next acquisition creates it again.
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedLockFile() {
		t.Error("acquisition after a clean release should have created the file again")
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	// A lock file left behind by a crashed owner: found, not made.
	if err := os.WriteFile(LockPath(sock), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Release()
	if third.CreatedLockFile() {
		t.Error("a pre-existing lock file was reported as created by this acquisition")
	}
}

// Mutation: make AcquireWait give up on the first ErrHeld.
//
// The daemon's reaper holds the lease across its inspection; a runner starting
// in that window must wait it out rather than lose its session identity.
func TestAcquireWaitOutlastsAShortLivedHolder(t *testing.T) {
	sock := tempSock(t, "a.sock")
	holder, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	testBeforeAcquireRetry = func(attempt int) {
		if attempt == 0 {
			_ = holder.Release()
			close(released)
		}
	}
	t.Cleanup(func() { testBeforeAcquireRetry = nil })

	lease, err := AcquireWait(sock, 2*time.Second)
	if err != nil {
		t.Fatalf("AcquireWait: %v (it gave up while the holder was releasing)", err)
	}
	defer lease.Release()
	select {
	case <-released:
	default:
		t.Fatal("AcquireWait returned without waiting for the holder")
	}
}

// The wait is bounded: a lease held for good is still reported as held.
func TestAcquireWaitGivesUpOnAPersistentHolder(t *testing.T) {
	sock := tempSock(t, "a.sock")
	holder, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()

	start := time.Now()
	if _, err := AcquireWait(sock, 40*time.Millisecond); !errors.Is(err, ErrHeld) {
		t.Fatalf("AcquireWait = %v, want ErrHeld", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("AcquireWait waited %v, far past its budget", elapsed)
	}
}

// Mutation: make ReleaseKeepingLockFile an alias for Release.
//
// The reaper borrows the lease to inspect a pathname it does not own. When it
// declines without learning anything, the lock file must survive: it is the
// only evidence that the leftover socket belonged to a lease-aware generation,
// and therefore the only thing that will ever let a runner reclaim it.
func TestReleaseKeepingLockFilePreservesLeaseHistory(t *testing.T) {
	sock := tempSock(t, "a.sock")
	owner, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}
	// The owner dies without cleaning up: the lock file stays.
	if err := owner.ReleaseKeepingLockFile(); err != nil {
		t.Fatalf("ReleaseKeepingLockFile: %v", err)
	}
	if _, err := os.Lstat(LockPath(sock)); err != nil {
		t.Fatalf("lock file removed by ReleaseKeepingLockFile: %v", err)
	}

	// A borrower inspects and puts it back untouched.
	borrower, err := AcquireExisting(sock)
	if err != nil {
		t.Fatalf("AcquireExisting: %v", err)
	}
	if borrower.CreatedLockFile() {
		t.Error("AcquireExisting reported creating a file it cannot create")
	}
	if err := borrower.ReleaseKeepingLockFile(); err != nil {
		t.Fatalf("borrower release: %v", err)
	}

	// The evidence survived both, so the next owner still knows this pathname
	// had a lease-aware generation.
	next, err := Acquire(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if next.CreatedLockFile() {
		t.Fatal("lease history was lost: the next owner had to create the lock file")
	}
}

// Mutation: treat any dial failure as a refusal (drop the ECONNREFUSED check).
//
// One predicate, used by the runner before it clears a leftover and by the
// daemon before it reaps one. A timeout must never read as "nothing answers":
// a wedged live owner with a full backlog is not a dead one.
func TestProbeRefusedOnlyAcceptsAnActualRefusal(t *testing.T) {
	dir := t.TempDir()

	t.Run("refused", func(t *testing.T) {
		path := filepath.Join(dir, "stale.sock")
		ln := listenUnix(t, path)
		_ = ln.Close() // pathname stays, nobody listening
		if err := ProbeRefused(path, time.Second); err != nil {
			t.Fatalf("ProbeRefused on a stale socket = %v, want nil", err)
		}
	})

	t.Run("live", func(t *testing.T) {
		path := filepath.Join(dir, "live.sock")
		ln := listenUnix(t, path)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		err := ProbeRefused(path, time.Second)
		if !errors.Is(err, ErrNotRefused) {
			t.Fatalf("ProbeRefused on a live socket = %v, want ErrNotRefused", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		err := ProbeRefused(filepath.Join(dir, "absent.sock"), time.Second)
		if !errors.Is(err, ErrNotRefused) {
			t.Fatalf("ProbeRefused on a missing pathname = %v, want ErrNotRefused", err)
		}
	})

	// Mutation: treat a timeout as a refusal.
	//
	// A connect that neither completes nor is refused proves nothing: a live
	// owner with a full accept backlog looks exactly like this. The deadline is
	// forced to have already passed, which makes the classification -- not the
	// timing -- the thing under test.
	t.Run("timed out", func(t *testing.T) {
		path := filepath.Join(dir, "wedged.sock")
		ln := listenUnix(t, path)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		err := ProbeRefused(path, time.Nanosecond)
		if err == nil {
			t.Fatal("a probe that timed out was reported as a refusal")
		}
		if !errors.Is(err, ErrProbeTimeout) {
			t.Fatalf("ProbeRefused = %v, want ErrProbeTimeout", err)
		}
		if errors.Is(err, ErrSocketLive) {
			t.Fatal("a timeout was classified as a live owner")
		}
	})

	t.Run("not a socket", func(t *testing.T) {
		path := filepath.Join(dir, "regular.sock")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ProbeRefused(path, time.Second)
		if !errors.Is(err, ErrNotRefused) {
			t.Fatalf("ProbeRefused on a regular file = %v, want ErrNotRefused", err)
		}
	})
}

// The protocol's stated premise, enforced where the directory is created.
//
// Mutation: drop the ownership check, or the permission tightening.
func TestRequireOwnedDir(t *testing.T) {
	t.Run("creates a private directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sessions")
		if err := RequireOwnedDir(dir); err != nil {
			t.Fatalf("RequireOwnedDir: %v", err)
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("mode = %o, want 700", perm)
		}
	})

	t.Run("tightens a permissive directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := RequireOwnedDir(dir); err != nil {
			t.Fatalf("RequireOwnedDir: %v", err)
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("mode = %o, want 700: a directory others can write in makes every lease meaningless", perm)
		}
	})

	t.Run("refuses a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sessions")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RequireOwnedDir(path); err == nil {
			t.Fatal("RequireOwnedDir accepted a regular file")
		}
	})

	// Mutation: delete the ownership comparison.
	//
	// The check is driven through the euid seam rather than against a real
	// foreign directory, for two reasons. It has to fail for the *ownership*
	// reason alone -- a world-writable directory like /tmp would also fail on
	// the chmod, so deleting the uid check would still produce an error and
	// prove nothing -- and a test must never chmod a directory it does not
	// own, which is exactly what the mutated code would attempt.
	t.Run("refuses a directory owned by another user", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sessions")
		// Ours, and already private: nothing else here can fail.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		geteuidFn = func() int { return os.Geteuid() + 1 }
		t.Cleanup(func() { geteuidFn = os.Geteuid })

		err := RequireOwnedDir(dir)
		if err == nil {
			t.Fatal("RequireOwnedDir accepted a directory owned by another user")
		}
		if !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("RequireOwnedDir = %v, want it to refuse on ownership", err)
		}
		// The directory is untouched: a foreign directory is refused, never
		// modified.
		fi, statErr := os.Lstat(dir)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("mode = %o, want it unchanged at 700", perm)
		}
	})

	// The same schedule with permissions that *would* need tightening: the
	// ownership check must come first, so a directory that is not ours is never
	// chmod'ed on the way to refusing it.
	t.Run("never modifies a directory owned by another user", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		geteuidFn = func() int { return os.Geteuid() + 1 }
		t.Cleanup(func() { geteuidFn = os.Geteuid })

		if err := RequireOwnedDir(dir); err == nil {
			t.Fatal("RequireOwnedDir accepted a directory owned by another user")
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o755 {
			t.Fatalf("mode = %o: a directory we do not own was modified", perm)
		}
	})
}

// The CI condition, forced rather than waited for.
//
// A filesystem that recycles inode numbers immediately produces a state this
// project's development machines cannot: the pathname names a *different* file
// whose device, inode and creation stamp all equal the pinned file's. Nothing a
// test can do to a non-recycling filesystem will produce it -- unlinking and
// rebinding always yields new numbers here, and hardlinking the original back
// yields the same file with a fresh ctime.
//
// So it is constructed: the pin's recorded identity is overwritten with the
// replacement's, which is exactly what the kernel would have handed us on a
// recycling filesystem. The pinned *file* is still the original -- unlinked, and
// therefore no longer named by anything -- and that is what the pin reports.
//
// Mutation: make RemoveSocket compare identities instead of consulting the pin.
// This test then unlinks a live replacement, which is the production defect CI
// caught.
func TestPinDetectsAReplacementThatCarriesTheSameIdentity(t *testing.T) {
	sock := tempSock(t, "recycled.sock")
	original := listenUnix(t, sock)
	lease, err := Acquire(sock)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()
	pin, err := lease.PinSocket()
	if err != nil {
		t.Fatalf("PinSocket: %v", err)
	}
	defer pin.Close()
	if !pin.Exact() {
		t.Skip("this platform cannot hold a handle on a socket; the identity is all there is")
	}

	// A live replacement takes the pathname.
	_ = original.Close()
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	replacement := listenUnix(t, sock)
	replacementID, ok := StatSocket(sock)
	if !ok {
		t.Fatal("replacement socket not found")
	}

	// Force the collision: pretend the kernel handed the replacement the
	// original's numbers and stamp.
	pin.ident = replacementID
	if current, _ := StatSocket(sock); !current.Same(pin.ident) {
		t.Fatal("the forced collision did not take")
	}

	// An identity comparison now says "unchanged". The pin does not.
	if pin.StillNamesIt() {
		t.Fatal("the pin claims a pathname that names a different file with the same identity")
	}
	if err := lease.RemoveSocket(pin); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("RemoveSocket = %v, want ErrIdentityChanged", err)
	}
	after, ok := StatSocket(sock)
	if !ok {
		t.Fatal("a live replacement's socket was unlinked because its identity matched a recycled one")
	}
	if after != replacementID {
		t.Fatal("the replacement's pathname was disturbed")
	}
	_ = replacement.Close()
}
