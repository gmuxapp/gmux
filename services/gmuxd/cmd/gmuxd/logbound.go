package main

// Bounding the daemon log.
//
// gmuxd's log is its stderr: the launcher opens the file and the child
// inherits it as fd 1 and fd 2. Everything that reports anything shares that
// one descriptor -- the log package, direct fmt.Fprintf(stderr, ...) error
// paths, runtime panic traces, and any child process that inherits it. Only
// one of those writers can be made to take a mutex. Bounding the file
// therefore has to be safe against writes it does not control, arriving at any
// instant, including while the bounding is in progress.
//
// Three designs fail that test:
//
//   - Copy the file aside and truncate it in place. Any write between the
//     copy's last read and the truncate is in neither file: silently deleted.
//     Exactly the panic trace you needed.
//   - Rename the file aside and leave fd 2 alone. Every future direct write
//     lands in the archive, and disappears when the archive is next replaced.
//   - Open a second descriptor onto the same file. One inode, two offsets.
//
// What works is to move the *descriptor*, not the bytes:
//
//  1. rename the current log to the archive. fd 2 still points at that inode,
//     so writes racing this step land in the archive -- retained, not lost.
//  2. create a fresh current log.
//  3. dup3 the fresh descriptor over fd 2, atomically. Every writer that
//     resolves fd 2 after this point -- including the ones that never heard
//     of this package -- writes to the new file.
//
// A write can land on either side of step 3, and both sides are readable
// files. No write is ever dropped.
//
// The bound is honest rather than absolute: rotation is checked before each
// log-package write, and by a periodic sweep for the writers that bypass it,
// so the current log can exceed the limit by whatever arrives between two
// checks. The one thing it may not do is exceed it silently and forever, which
// is what a rotation triggered only by our own writes would allow.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/socklease"
)

// sweepInterval bounds how long the log can stay oversized when nothing is
// writing through the log package -- a daemon whose only output is a child
// process spewing on the inherited descriptor, say. It is a variable so tests
// can drive the sweeper without waiting for it.
var sweepInterval = 30 * time.Second

const (
	// defaultLogLimit is the size at which the current log is archived. One
	// archive is kept, so the daemon's logs occupy about twice this.
	defaultLogLimit = 8 << 20
	// rotateRetryDelay throttles retries after a failed rotation, so a
	// persistent filesystem problem cannot turn every log line into two.
	rotateRetryDelay = time.Minute
	// writeChunk caps a single write so one enormous log line cannot carry
	// the file arbitrarily past the limit before the next check.
	writeChunk = 1 << 20
	// maxWriteInterrupts bounds consecutive EINTR retries that made no
	// progress, so a signal storm cannot spin inside the logger's mutex.
	maxWriteInterrupts = 64
)

// boundedLog keeps the process's own log file bounded.
//
// It owns no descriptor of its own: it writes to fd 2, the descriptor every
// other writer in the process also uses, and rotation replaces what fd 2
// points at rather than what the file contains.
type boundedLog struct {
	mu      sync.Mutex
	fd      int // always 2; kept explicit because that is the whole design
	dupFDs  []int
	path    string
	archive string
	limit   int64

	// nextAttempt and failureNote implement transition reporting for rotation
	// failures: a permanently unwritable directory costs one line and one
	// attempt per minute, not one of each per log line.
	nextAttempt time.Time
	failureNote string
	// disabled stops rotation after a failure that left fd 2 pointing at a
	// file this logger no longer controls. Logging continues; bounding does
	// not, because the next rotation could unlink the inode being written to.
	disabled bool

	stop     chan struct{}
	stopOnce sync.Once
	sweeping sync.WaitGroup
}

// testRotatePhase is a barrier hook for the rotation tests: it runs at each
// named phase of a rotation so a test can write directly to the descriptor in
// the exact window it wants. It is nil in production.
var testRotatePhase func(phase string)

// dupOntoFn is the descriptor move, indirected so a test can make the one
// step that cannot be provoked by the filesystem fail. It is dupOnto in
// production.
var dupOntoFn = dupOnto

// writeFn is the write syscall, indirected for the same reason: a short write
// or an EINTR on a regular file is legal, rare, and unreachable from a test
// otherwise. It is syscall.Write in production.
var writeFn = syscall.Write

func rotatePhase(phase string) {
	if testRotatePhase != nil {
		testRotatePhase(phase)
	}
}

// installBoundedLog bounds the daemon log if -- and only if -- the process's
// stderr really is that log file. A foreground `gmuxd serve` on a terminal, or
// one whose output the operator redirected somewhere else, is left completely
// alone: silently retargeting or rotating a destination somebody chose would
// be a worse bug than an unbounded log.
//
// It returns nil when bounding does not apply. The caller must Stop the
// returned logger.
func installBoundedLog(stderr io.Writer, logPath string, limit int64) *boundedLog {
	f, ok := stderr.(*os.File)
	if !ok {
		return nil
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	pi, err := os.Lstat(logPath)
	if err != nil || !os.SameFile(fi, pi) {
		return nil
	}
	if int(f.Fd()) != syscall.Stderr {
		// Bounding works by replacing what fd 2 points at. A log file that is
		// not fd 2 would leave every direct stderr write behind.
		return nil
	}
	// The mode argument of OpenFile only applies on creation, so a log file
	// that predates this (or was created by something more permissive) can
	// still be world-readable while carrying session titles and command
	// lines. Fixing it is best-effort: refusing to bound an unbounded log
	// because we could not also tighten its mode would trade a small
	// disclosure for a disk-filling one, so the failure is loud instead.
	if fi.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			log.Printf("gmuxd: WARNING: %s is mode %o and cannot be restricted (%v); "+
				"it records session titles, working directories and command lines",
				logPath, fi.Mode().Perm(), err)
		}
	}
	b := &boundedLog{
		fd:      syscall.Stderr,
		path:    logPath,
		archive: logPath + ".1",
		limit:   limit,
		stop:    make(chan struct{}),
	}
	// stdout usually names the same file; keep it following the current log
	// too, so a child that writes to stdout does not keep the archive alive.
	if so, soErr := os.Stdout.Stat(); soErr == nil && os.SameFile(so, fi) {
		b.dupFDs = append(b.dupFDs, syscall.Stdout)
	}
	// No descendant may inherit a log descriptor.
	//
	// Rotation moves *this* process's descriptors; it cannot move another
	// process's. A child holding an inherited descriptor would keep writing to
	// the archive after one rotation and to an unlinked inode after the next,
	// losing its output silently. Rather than trying to track such writers,
	// make them impossible: mark the log descriptors close-on-exec, so a child
	// only ever has the log if someone deliberately wired it in (see
	// productionRunnerSpawner, which wires /dev/null explicitly).
	//
	// This is re-applied on every rotation. dupOnto already marks the
	// descriptor it installs, so the re-application is redundant by design:
	// two independent mechanisms establish the same flag, and a regression in
	// either one leaves the invariant standing.
	b.markCloseOnExec()
	log.SetOutput(b)
	b.sweeping.Add(1)
	go b.sweep()
	return b
}

// markCloseOnExec marks the log descriptors close-on-exec. Failures are
// reported rather than fatal: an inherited descriptor is a diagnostic hazard,
// not a reason to refuse to run.
func (b *boundedLog) markCloseOnExec() {
	for _, fd := range append([]int{b.fd}, b.dupFDs...) {
		if err := setCloseOnExec(fd); err != nil {
			log.Printf("gmuxd: cannot mark fd %d close-on-exec (%v); a child could inherit the log", fd, err)
		}
	}
}

// Stop ends the background sweep. It does not close fd 2.
func (b *boundedLog) Stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() { close(b.stop) })
	b.sweeping.Wait()
}

// sweep bounds the log even when nothing writes through the log package: the
// writers this design exists to preserve -- panic traces, direct stderr
// output, inherited children -- never call Write.
func (b *boundedLog) sweep() {
	defer b.sweeping.Done()
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.mu.Lock()
			b.rotateIfLargeLocked()
			b.mu.Unlock()
		}
	}
}

// Write appends to the log, rotating first when the write would carry the file
// past the limit, and splitting writes too large to be bounded any other way.
func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	deferred := b.writeLocked(p)
	b.mu.Unlock()
	// Any complaint the write path collected is emitted after the lock is
	// released: it goes through the log package, whose output is this writer.
	for _, note := range deferred.notes {
		log.Print(note)
	}
	return deferred.written, deferred.err
}

// writeResult carries what Write learned out of the critical section.
type writeResult struct {
	written int
	err     error
	notes   []string
}

func (b *boundedLog) writeLocked(p []byte) writeResult {
	var res writeResult
	interrupted := 0
	for len(p) > 0 {
		res.notes = append(res.notes, b.rotateIfLargeLocked()...)
		chunk := p
		if len(chunk) > writeChunk {
			chunk = chunk[:writeChunk]
		}
		n, err := writeFn(b.fd, chunk)
		// The write(2) convention is that n is -1 whenever errno is set, and a
		// partial write interrupted by a signal is reported as a *successful*
		// short write rather than EINTR. So a byte count is only ever
		// meaningful when it is positive, and it is never safe to index with
		// one that is not: `p[n:]` with n = -1 panics, which in this function
		// means a panic inside the logger, under the logger's own mutex, in a
		// daemon whose contract says a broken write degrades to a large log
		// rather than a dead one.
		if n < 0 {
			n = 0
		}
		res.written += n
		p = p[n:]

		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				if n == 0 {
					// No progress at all. Retry, but not forever: a signal
					// storm must not turn one log line into an infinite loop
					// holding this mutex.
					interrupted++
					if interrupted > maxWriteInterrupts {
						res.err = err
						return res
					}
					continue
				}
				interrupted = 0
				continue
			}
			res.err = err
			return res
		}
		if n == 0 {
			// No error and no progress. Retrying would spin forever, and
			// reporting success would let the caller believe a truncated
			// diagnostic was written in full.
			res.err = io.ErrShortWrite
			return res
		}
		// A short write is progress, not completion: carry on with the
		// remainder, which is what io.Writer's contract requires of us before
		// we may report success.
		interrupted = 0
	}
	// Leave the file bounded for whatever writes next, including writers that
	// never reach this function.
	res.notes = append(res.notes, b.rotateIfLargeLocked()...)
	return res
}

// rotateIfLargeLocked archives and replaces the log when it exceeds the limit.
//
// Every failure path leaves a usable descriptor: fd 2 always names a file that
// exists, so the worst outcome of a broken filesystem is an unbounded log,
// never a dead logger or a panicking daemon.
//
// It returns any diagnostics that must be emitted through the log package;
// the caller emits them *after* releasing b.mu. Logging from in here would
// re-enter Write, which takes the same non-reentrant mutex, and wedge the
// process -- including the sweep goroutine and, through it, shutdown.
func (b *boundedLog) rotateIfLargeLocked() (notes []string) {
	if b.disabled {
		return nil
	}
	if !b.nextAttempt.IsZero() && time.Now().Before(b.nextAttempt) {
		return nil
	}
	// Stat the descriptor, not the pathname: everything that writes directly
	// to stderr shares this file, and a bound that ignored those writes would
	// not be a bound.
	var st syscall.Stat_t
	if err := syscall.Fstat(b.fd, &st); err != nil || st.Size < b.limit {
		return nil
	}
	notes, err := b.rotateLocked()
	if err != nil {
		b.nextAttempt = time.Now().Add(rotateRetryDelay)
		if note := err.Error(); note != b.failureNote {
			b.failureNote = note
			// Straight to the descriptor: going through Write would re-enter
			// rotation, and through the log package would re-enter the mutex.
			_, _ = writeFn(b.fd, fmt.Appendf(nil, "gmuxd: cannot rotate %s: %v\n", b.path, err))
		}
		return notes
	}
	b.nextAttempt = time.Time{}
	b.failureNote = ""
	return notes
}

func (b *boundedLog) rotateLocked() (notes []string, err error) {
	// Serialise against any other daemon process that might still be bounding
	// this same file. Ownership already makes an overlap nearly impossible --
	// bounding installs only after the state lock is held, which an incumbent
	// releases as it exits -- but "nearly" is doing too much work for an
	// operation that renames files, so rotations take a lease of their own.
	lease, err := socklease.AcquireWait(b.path, 2*time.Second)
	if errors.Is(err, socklease.ErrHeld) {
		return nil, errors.New("another process is rotating this log")
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Release() }()

	// Re-check under the lease: the other rotator may have just finished.
	var st syscall.Stat_t
	if err := syscall.Fstat(b.fd, &st); err != nil {
		return nil, err
	}
	if st.Size < b.limit {
		return nil, nil
	}
	rotatePhase("lease-held")

	// A fresh current log, prepared before anything is moved so a failure
	// here changes nothing at all.
	fresh, err := os.OpenFile(b.path+".new", os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Left by a rotation that died between creating it and renaming it.
		_ = os.Remove(b.path + ".new")
		fresh, err = os.OpenFile(b.path+".new", os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	if err != nil {
		return nil, err
	}
	defer fresh.Close()

	// Archive by rename: atomic, so the previous archive is replaced in one
	// step and a failure cannot leave a partial one. fd 2 still points at
	// this inode, so writes racing the rename land in the archive -- retained,
	// which is the entire reason this is a rename and not a copy.
	rotatePhase("before-archive")
	if err := os.Rename(b.path, b.archive); err != nil {
		_ = os.Remove(b.path + ".new")
		return nil, err
	}
	rotatePhase("archived")

	if err := os.Rename(b.path+".new", b.path); err != nil {
		// Put the log back where it was; fd 2 is still attached to it, so
		// this restores both the name and the writer.
		if undo := os.Rename(b.archive, b.path); undo != nil {
			b.disabled = true
			return nil, fmt.Errorf("%v (and the log could not be restored: %v)", err, undo)
		}
		_ = os.Remove(b.path + ".new")
		return nil, err
	}
	rotatePhase("current-replaced")

	// Move the descriptor. From here every writer in the process -- ours and
	// everyone else's -- resolves fd 2 to the fresh file.
	if err := dupOntoFn(int(fresh.Fd()), b.fd); err != nil {
		// fd 2 still names the archive: logging survives, but a further
		// rotation would rename the fresh (empty) log over it and unlink the
		// inode being written to. Stop bounding instead.
		b.disabled = true
		return nil, fmt.Errorf("could not move the log descriptor: %w", err)
	}
	for _, fd := range b.dupFDs {
		if err := dupOntoFn(int(fresh.Fd()), fd); err != nil {
			// Deferred, not logged here: this runs under b.mu, and the log
			// package's output is this writer. A secondary descriptor left on
			// the archive is a diagnostic wart, not a reason to wedge the
			// daemon.
			notes = append(notes, fmt.Sprintf("gmuxd: fd %d still points at the archived log: %v", fd, err))
		}
	}
	b.markCloseOnExec()
	rotatePhase("descriptor-moved")
	return notes, nil
}
