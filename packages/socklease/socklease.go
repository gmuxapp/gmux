// Package socklease implements the ownership protocol for a canonical Unix
// socket pathname shared by the gmux runner (which binds the socket) and the
// gmux daemon (which reaps sockets left behind by dead runners).
//
// # Why a lease
//
// AF_UNIX pathname ownership is not tied to the listening socket: closing (or
// crashing with) a listener leaves the pathname behind, and any process may
// unlink a pathname another process is listening on. Every "is this socket
// stale?" answer derived purely from the filesystem or from a probe is a
// TOCTOU: between the observation and the unlink, the pathname can be rebound
// by a fresh, live runner, and the unlink then silently disconnects it.
//
// The lease closes that window with an advisory whole-file lock (flock) on a
// side-car file, sockPath+".lock":
//
//   - A runner acquires the lease before it removes any stale pathname and
//     before it listens, and holds it for as long as it owns the pathname.
//   - The daemon acquires the same lease, non-blocking, before it inspects or
//     unlinks a suspected-stale pathname, and holds it across the whole
//     inspect-and-unlink sequence.
//
// Because flock is released by the kernel when the holding process dies
// (including SIGKILL and panics), a successful non-blocking acquisition is
// proof that no lease-aware owner is alive. Because the lease is held across
// identity check and unlink, no lease-aware owner can appear in the middle of
// that sequence.
//
// # Lock file lifetime
//
// The lock file is created by the owner and unlinked by the owner when it
// releases the lease, so the socket directory does not accumulate one file per
// session ever started. Unlinking a lock file is only safe if lockers detect
// that the file they locked is no longer the file the pathname names --
// otherwise two processes could hold locks on two different inodes for the
// same pathname. Acquire therefore verifies, after locking, that the locked
// inode is still the one named by the lock pathname, and retries when it is
// not (see acquire).
//
// # Mixed versions
//
// A runner from before this protocol creates no lock file. AcquireExisting
// (used by the daemon) never creates one, and reports ErrNoLockFile instead,
// so the daemon can decline to touch a pathname whose owner is not
// lease-aware: absence of proof of death is not proof of death.
//
// # Threat model
//
// This is a cooperative protocol between processes of one user inside one
// private directory, and it is not more than that. It assumes:
//
//   - the socket directory is owned by the calling user and carries no group
//     or other permissions (RequireOwnedDir establishes this at the point of
//     creation, and the runner refuses to bind otherwise);
//   - every process that manipulates a pathname in that directory follows this
//     protocol, or predates it and is therefore handled conservatively.
//
// Within those assumptions the protocol is sound. Outside them it is not, and
// two places are worth naming rather than glossing:
//
//   - Release verifies that the lock pathname still names the descriptor it is
//     about to unlink. That is a check followed by an act, and no portable
//     primitive makes it one operation: there is no unlink conditional on an
//     inode. It therefore dominates a substitution that has already happened
//     -- the reachable case, where some other actor replaced the pathname
//     earlier -- and does not defend against an actor racing that exact
//     instruction. Linux's renameat2 and openat2 offer nothing that closes it,
//     and O_NOFOLLOW plus the post-lock inode verification is as far as the
//     acquisition side can go on both Linux and Darwin.
//   - Socket identity is device+inode. A same-user actor with write access to
//     the directory can hard-link a bound socket and restore it later, which
//     is why identity alone never authorises anything destructive: the daemon
//     additionally requires the runner's own incarnation nonce to agree across
//     two calls.
//
// So: no claim is made that this protocol is safe against a hostile process
// running as the same user with write access to the socket directory. Such a
// process can already unlink and rebind every socket in it, with or without
// this package.
package socklease

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"
)

// Suffix is appended to the canonical socket pathname to form the lock
// pathname. It deliberately keeps the ".sock" part intact so that lock files
// sort next to their socket, and it does not itself end in ".sock" so that
// socket enumeration (which filters on that suffix) never sees a lock file.
const Suffix = ".lock"

var (
	// ErrHeld reports that another live process owns the lease.
	ErrHeld = errors.New("socklease: lease is held by another process")
	// ErrNoLockFile reports that no lock file exists for the pathname, which
	// means its owner (if any) predates this protocol. Only AcquireExisting
	// returns it; Acquire creates the file.
	ErrNoLockFile = errors.New("socklease: no lock file (owner is not lease-aware)")
	// ErrIdentityChanged reports that the socket pathname no longer names the
	// file the caller expected, so it was left untouched.
	ErrIdentityChanged = errors.New("socklease: socket identity changed")
	// ErrNotRefused reports that a connect attempt to a socket pathname was
	// not actively refused, so nothing about the pathname's owner was proved.
	ErrNotRefused = errors.New("socklease: socket was not actively refused")
	// ErrSocketLive is the one ErrNotRefused case that *did* prove something:
	// somebody accepted the connection. Callers distinguish it because "a live
	// owner is there" and "I learned nothing" lead to different decisions
	// about the pathname's lease history.
	ErrSocketLive = fmt.Errorf("%w: connection accepted (a live owner)", ErrNotRefused)
	// ErrProbeTimeout is the ambiguous case worth naming separately: the
	// connect neither completed nor was refused. It proves nothing -- a wedged
	// owner with a full accept backlog looks exactly like this -- and it is a
	// distinct sentinel so that a test can pin the classification instead of
	// pinning a message.
	ErrProbeTimeout = fmt.Errorf("%w: probe timed out (owner may be wedged, not dead)", ErrNotRefused)
)

// ProbeRefused returns nil only when connecting to the socket at path is
// actively refused, which for a Unix socket means the pathname exists and no
// process holds a listening socket for it.
//
// Every other outcome is an error, deliberately: a successful connect means
// somebody is alive there, and a timeout or a permission failure means nothing
// was learned at all. Both callers -- the runner clearing a leftover before it
// binds, and the daemon reaping an abandoned pathname -- use this one
// predicate, because a timeout that reads as "nothing answers" on one side and
// "learned nothing" on the other is exactly the asymmetry that gets a live
// socket unlinked.
func ProbeRefused(path string, timeout time.Duration) error {
	// Connecting to a regular file reports ECONNREFUSED on Linux, which would
	// make "refused" mean "not a socket" as well as "no listener". Callers
	// already establish the type; establishing it here too keeps the predicate
	// meaningful on its own.
	if _, isSocket := StatSocket(path); !isSocket {
		return fmt.Errorf("%w: %s is not a socket", ErrNotRefused, path)
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err == nil {
		_ = conn.Close()
		return ErrSocketLive
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	// A dial that ran out of time reports a net.Error with Timeout() true, not
	// os.ErrDeadlineExceeded -- that sentinel belongs to deadlines on an
	// established connection. Both are checked because both are reachable
	// depending on where the budget expires.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() || errors.Is(err, os.ErrDeadlineExceeded) {
		return ErrProbeTimeout
	}
	return fmt.Errorf("%w: %v", ErrNotRefused, err)
}

// ProbeRefusedPinned probes the pathname a pin holds, and exists so that
// "probe, then decide what you probed" cannot be written.
//
// The ordering is load-bearing and easy to get backwards. A lease excludes
// lease-aware runners, not the pre-protocol ones, so an unleased runner can take
// the pathname over at any instant -- including between a probe and a pin. Pin
// first and a takeover is harmless: the pin holds the file the probe found dead,
// the pathname no longer resolves to it, and the removal declines. Probe first
// and the pin lands on the newcomer, which is then unlinked alive and unprobed.
//
// Taking the pin as a parameter makes the wrong order unexpressible: there is
// nothing to pass. A caller that tries anyway passes nil and is refused.
func ProbeRefusedPinned(pin *Pin, timeout time.Duration) error {
	if pin == nil {
		return fmt.Errorf("%w: probe without a pin (probe-then-pin is not a valid order)", ErrNotRefused)
	}
	return ProbeRefused(pin.Path(), timeout)
}

// RequireOwnedDir creates dir with mode 0700 if needed and establishes the
// premise the whole protocol rests on: the directory is ours and nobody else's
// to write in.
//
// A directory owned by another user is refused outright -- a lease inside it
// proves nothing, because its owner can replace any pathname at will. A
// directory of ours that carries group or other permissions is tightened; if
// it cannot be tightened, that is also refused. Both are reported rather than
// silently accepted, because the alternative is a protocol whose stated
// assumptions are false on the machine it is running on.
func RequireOwnedDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("socklease: create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("socklease: stat %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socklease: %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if uid := geteuidFn(); int(st.Uid) != uid {
			return fmt.Errorf("socklease: %s is owned by uid %d, not %d; refusing to trust it",
				dir, st.Uid, uid)
		}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("socklease: %s is mode %o and cannot be restricted: %w",
				dir, fi.Mode().Perm(), err)
		}
	}
	return nil
}

// acquireRetries bounds the verify-after-lock loop. Each iteration requires a
// concurrent owner to have released (and unlinked) a lock file between our
// open and our flock, so exhausting the budget is not reachable without an
// adversarial rebind loop.
const acquireRetries = 16

// acquireRetryDelay is the poll interval of AcquireWait.
const acquireRetryDelay = 5 * time.Millisecond

// LockPath returns the lock pathname used for sockPath.
func LockPath(sockPath string) string { return sockPath + Suffix }

// testAfterOpen is a barrier hook for the verify-after-lock test: it runs
// between the open and the flock in acquire, which is the only window in which
// the lock file can be replaced under a locker. It is nil in production and
// costs one nil check per acquisition.
var testAfterOpen func()

// testBeforeAcquireRetry is a barrier hook for the bounded-wait test: it runs
// before each AcquireWait retry sleeps. It is nil in production.
var testBeforeAcquireRetry func(attempt int)

// geteuidFn is the effective uid, indirected so a test can pin the ownership
// check without needing a directory owned by somebody else -- and, crucially,
// without ever chmod'ing a directory it does not own. It is os.Geteuid in
// production.
var geteuidFn = os.Geteuid

// Lease is a held exclusive advisory lock over one socket pathname. It is not
// safe for concurrent use by multiple goroutines; the owner is expected to be
// a single lifecycle (bind ... release).
type Lease struct {
	f        *os.File
	sockPath string
	created  bool
}

// Acquire takes the lease for sockPath, creating the lock file (mode 0600) if
// needed. It returns ErrHeld when another live process owns the lease.
//
// The returned lease must be released with Release exactly once. The lock file
// descriptor is close-on-exec, so an exec'd child (the runner's PTY child, for
// instance) never inherits the lease and cannot keep it alive past the owner.
func Acquire(sockPath string) (*Lease, error) {
	return acquire(sockPath, true)
}

// AcquireWait is Acquire with a bounded wait for a lease that is currently
// held. It exists for one specific contention: the daemon's stale-socket
// reaper holds the lease for the length of its inspection (a stat, a connect
// and an unlink), and a runner starting inside that window would otherwise
// conclude the pathname is taken and fall back to a fresh session id --
// silently losing a resume identity to a bookkeeping sweep.
//
// The wait is short and unconditional: a lease held by a genuinely live runner
// is still reported as ErrHeld, just budget later.
func AcquireWait(sockPath string, budget time.Duration) (*Lease, error) {
	deadline := time.Now().Add(budget)
	for attempt := 0; ; attempt++ {
		lease, err := acquire(sockPath, true)
		if !errors.Is(err, ErrHeld) || !time.Now().Before(deadline) {
			return lease, err
		}
		if testBeforeAcquireRetry != nil {
			testBeforeAcquireRetry(attempt)
		}
		time.Sleep(acquireRetryDelay)
	}
}

// AcquireExisting takes the lease for sockPath but never creates the lock
// file: it returns ErrNoLockFile when the file is absent. The daemon's reaper
// uses it so that a socket owned by a runner that predates this protocol -- and
// therefore holds no lease -- can never be mistaken for an abandoned one.
func AcquireExisting(sockPath string) (*Lease, error) {
	return acquire(sockPath, false)
}

// CreatedLockFile reports whether this acquisition created the lock file
// rather than finding one.
//
// It is the runner's only evidence about who owns a pathname it could not
// bind. A pre-existing lock file means some lease-aware generation owned this
// pathname before, and every lease-aware actor is excluded while this lease is
// held -- so cleaning up after that generation is safe. A lock file this call
// had to create means the opposite: whatever occupies the pathname predates
// the protocol, is excluded by nothing, and must never be unlinked.
func (l *Lease) CreatedLockFile() bool { return l.created }

func acquire(sockPath string, create bool) (*Lease, error) {
	lockPath := LockPath(sockPath)
	for range acquireRetries {
		// Open in two steps so the caller can distinguish "found a lock file"
		// from "had to make one", and never through a symlink: O_NOFOLLOW
		// turns a symlink planted at the lock pathname into ELOOP rather than
		// a lock on -- and later an unlink of -- some unrelated file.
		f, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		created := false
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				return nil, ErrNoLockFile
			}
			f, err = os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
			if errors.Is(err, fs.ErrExist) {
				continue // someone created it first: reopen and lock that one
			}
			created = true
		}
		if err != nil {
			return nil, fmt.Errorf("socklease: open %s: %w", lockPath, err)
		}
		if testAfterOpen != nil {
			testAfterOpen()
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return nil, ErrHeld
			}
			return nil, fmt.Errorf("socklease: flock %s: %w", lockPath, err)
		}
		// Verify the file we locked is still the file the lock pathname names.
		// A previous owner may have unlinked it (and a new owner created a
		// replacement) between our open and our flock; locking an unlinked
		// inode excludes nobody.
		if current, ok := sameFile(f, lockPath); ok && current {
			return &Lease{f: f, sockPath: sockPath, created: created}, nil
		}
		_ = f.Close() // releases the lock on the dead inode
	}
	return nil, fmt.Errorf("socklease: %s: lock file replaced repeatedly", lockPath)
}

// sameFile reports whether the open file and the pathname name the same inode.
// ok is false when the comparison could not be made at all.
func sameFile(f *os.File, path string) (same, ok bool) {
	fi, err := f.Stat()
	if err != nil {
		return false, false
	}
	pi, err := os.Lstat(path)
	if err != nil {
		return false, true // vanished: definitely not the same file
	}
	return os.SameFile(fi, pi), true
}

// SockPath returns the socket pathname this lease owns.
func (l *Lease) SockPath() string { return l.sockPath }

// LockPath returns the lock pathname backing this lease.
func (l *Lease) LockPath() string { return LockPath(l.sockPath) }

// DuplicateForExec duplicates the held lease descriptor for an explicitly
// coordinated child process. The duplicate refers to the same flock-owning
// open file description, so the lease remains continuously held while parent
// ownership is transferred to the child across exec.
func (l *Lease) DuplicateForExec() (*os.File, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_FCNTL, l.f.Fd(), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, fmt.Errorf("socklease: duplicate lease: %w", errno)
	}
	return os.NewFile(fd, l.f.Name()), nil
}

// AdoptInherited adopts a descriptor inherited from a coordinating parent.
// It verifies that the descriptor still names the current sidecar before
// allowing the child to use it as its lease.
func AdoptInherited(sockPath string, f *os.File) (*Lease, error) {
	return adoptInherited(sockPath, f, nil)
}

func adoptInherited(sockPath string, f *os.File, beforeLock func()) (*Lease, error) {
	if f == nil {
		return nil, errors.New("socklease: adopt nil lease descriptor")
	}
	if same, ok := sameFile(f, LockPath(sockPath)); !ok || !same {
		_ = f.Close()
		return nil, fmt.Errorf("socklease: inherited lease no longer names %s", LockPath(sockPath))
	}
	if beforeLock != nil {
		beforeLock()
	}
	// Reassert the lock rather than trusting the environment-selected fd. On
	// the duplicated open-file description inherited from the parent this is a
	// no-op; on an unlocked descriptor it establishes real exclusivity before
	// the caller can act as an owner.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("socklease: lock inherited lease: %w", err)
	}
	// The sidecar may have been replaced between the first identity check and
	// flock. Verify after locking, just like acquire(), so adoption can never
	// return ownership of an unlinked predecessor while another process owns
	// the current pathname.
	if same, ok := sameFile(f, LockPath(sockPath)); !ok || !same {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("socklease: inherited lease changed before lock: %w", ErrIdentityChanged)
	}
	return &Lease{f: f, sockPath: sockPath}, nil
}

// RemoveSocket unlinks the socket pathname, but only while it still names the
// file the pin holds.
//
// The check is a Pin rather than an identity because an identity is not enough.
// Inode numbers are recycled -- immediately, on some filesystems -- so a live
// replacement can present exactly the device and inode of the dead leftover it
// replaced, and an identity comparison would unlink it. A pin answers the
// question that actually matters: is the file at this pathname still the file
// the caller decided about? Where the platform can hold a handle, that answer
// is exact.
//
// It returns ErrIdentityChanged when the pathname names something else, and nil
// when the pathname is already gone.
func (l *Lease) RemoveSocket(pin *Pin) error {
	if pin == nil {
		return fmt.Errorf("%w: caller passed no pin", ErrIdentityChanged)
	}
	if pin.path != l.sockPath {
		return fmt.Errorf("%w: pin holds %s, lease owns %s", ErrIdentityChanged, pin.path, l.sockPath)
	}
	if _, err := os.Lstat(l.sockPath); errors.Is(err, fs.ErrNotExist) {
		return nil // already gone: idempotent
	}
	if !pin.StillNamesIt() {
		return fmt.Errorf("%w: %s no longer names %s", ErrIdentityChanged, l.sockPath, pin.ident)
	}
	if err := os.Remove(l.sockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("socklease: remove %s: %w", l.sockPath, err)
	}
	return nil
}

// Release unlinks the lock file and drops the lock. It is idempotent.
//
// Unlinking under the lock is what keeps the socket directory bounded, and it
// is safe in both directions:
//
//   - a racing acquirer that opened this inode before the unlink either loses
//     the flock or fails Acquire's post-lock verification and retries, so it
//     can never come away owning a lease over an unlinked inode;
//   - the unlink is conditional on the lock pathname still naming the file
//     this lease holds. Without that condition, a lease whose pathname was
//     substituted -- by anything that does not follow this protocol, or by a
//     hostile write in the socket directory -- would unlink the *replacement's*
//     lock file on its way out, leaving two leases that do not exclude each
//     other: the replacement's, now over an unlinked inode, and whoever
//     creates the pathname next.
//
// The condition is a check-then-act, so it does not defend against an
// adversary racing this exact instruction. It defends against the reachable
// case, where the substitution already happened.
func (l *Lease) Release() error { return l.release(true) }

// ReleaseKeepingLockFile drops the lock but leaves the lock file in place.
//
// It is for a *borrowed* lease: the daemon's reaper takes the lease to inspect
// a pathname it does not own, and when it comes away without having learned
// anything about the occupant -- a probe that timed out, an identity that
// changed mid-inspection -- it must put the pathname back exactly as it found
// it. Removing the lock file there would erase the evidence that the pathname
// once belonged to a lease-aware generation, and that evidence is the only
// thing that ever makes the leftover socket reclaimable (see
// CreatedLockFile).
func (l *Lease) ReleaseKeepingLockFile() error { return l.release(false) }

// ReleaseForTransfer closes this process's descriptor without explicitly
// unlocking it. It is valid only after DuplicateForExec: the inherited
// duplicate keeps the shared open-file-description lock held continuously.
func (l *Lease) ReleaseForTransfer() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *Lease) release(removeLockFile bool) error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	var rmErr error
	if removeLockFile {
		if same, ok := sameFile(f, l.LockPath()); ok && same {
			rmErr = os.Remove(l.LockPath())
		}
		if rmErr != nil && errors.Is(rmErr, fs.ErrNotExist) {
			rmErr = nil
		}
	}
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	return errors.Join(rmErr, unlockErr, closeErr)
}
