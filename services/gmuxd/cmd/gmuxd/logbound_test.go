package main

// logbound_test.go — mutation-grade tests for daemon log bounding.
//
// The property that matters is not "the file gets smaller". It is that no
// output is ever lost, including output from the writers this package cannot
// synchronise with: direct writes to the inherited descriptor, runtime panic
// traces, and child processes. Every test below writes directly to the
// descriptor, in the windows a rotation actually has, and then insists on
// finding those bytes in one of the two files an operator can read.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// logHarness owns a real log file installed on a real descriptor. Bounding
// replaces what a descriptor points at, so these tests cannot use an
// arbitrary *os.File: they need the process's own fd 2. Each test therefore
// borrows fd 2, and puts it back afterwards.
type logHarness struct {
	path    string
	archive string
	saved   int
	b       *boundedLog
}

func newLogHarness(t *testing.T, limit int64) *logHarness {
	t.Helper()
	dir := t.TempDir()
	h := &logHarness{path: filepath.Join(dir, "gmuxd.log"), archive: filepath.Join(dir, "gmuxd.log.1")}

	// Save the real stderr so the test framework keeps working afterwards.
	saved, err := syscall.Dup(syscall.Stderr)
	if err != nil {
		t.Fatalf("dup stderr: %v", err)
	}
	h.saved = saved

	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if err := dupOnto(int(f.Fd()), syscall.Stderr); err != nil {
		t.Fatalf("install log on fd 2: %v", err)
	}
	_ = f.Close()
	// A daemon inherits fd 2 from its launcher, and an inherited descriptor
	// carries no close-on-exec flag. dupOnto sets one, so clear it again:
	// otherwise the harness would be handing the code under test a descriptor
	// that is already in the state it is supposed to establish.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(syscall.Stderr), syscall.F_SETFD, 0); errno != 0 {
		t.Fatalf("clear close-on-exec on fd 2: %v", errno)
	}

	t.Cleanup(func() {
		if h.b != nil {
			h.b.Stop()
		}
		_ = dupOnto(h.saved, syscall.Stderr)
		_ = syscall.Close(h.saved)
	})

	h.b = installBoundedLog(os.Stderr, h.path, limit)
	if h.b == nil {
		t.Fatal("installBoundedLog declined the process's own log file")
	}
	return h
}

// direct writes straight to the descriptor, the way a panic trace or an
// inherited child does: no mutex, no knowledge of this package.
func (h *logHarness) direct(t *testing.T, s string) {
	t.Helper()
	if _, err := syscall.Write(syscall.Stderr, []byte(s)); err != nil {
		t.Fatalf("direct write: %v", err)
	}
}

func (h *logHarness) log(t *testing.T, s string) {
	t.Helper()
	if _, err := h.b.Write([]byte(s)); err != nil {
		t.Fatalf("log write: %v", err)
	}
}

func (h *logHarness) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// somewhere asserts that a marker survived, in either readable file.
func (h *logHarness) somewhere(t *testing.T, marker string) {
	t.Helper()
	if strings.Contains(h.read(t, h.path), marker) || strings.Contains(h.read(t, h.archive), marker) {
		return
	}
	t.Fatalf("%q was lost by the rotation: it is in neither the current log nor the archive", marker)
}

func fill(n int) string { return strings.Repeat("x", n) + "\n" }

// breakRotation makes creating the fresh log impossible, in a way the
// rotation's own recovery cannot clear: a non-empty directory where the new
// file must go. It stands in for the whole family of filesystem refusals
// (ENOSPC, EROFS, EDQUOT) that a daemon must survive.
func breakRotation(t *testing.T, h *logHarness) {
	t.Helper()
	if err := os.Mkdir(h.path+".new", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.path+".new", "occupant"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(h.path + ".new") })
}

// fixRotation undoes breakRotation.
func fixRotation(t *testing.T, h *logHarness) {
	t.Helper()
	if err := os.RemoveAll(h.path + ".new"); err != nil {
		t.Fatal(err)
	}
}

// Mutation: remove the rotation call from Write.
func TestBoundedLogRotatesAndKeepsOneArchive(t *testing.T) {
	h := newLogHarness(t, 512)
	for i := range 20 {
		h.log(t, fmt.Sprintf("line-%02d %s", i, fill(64)))
	}

	if size := len(h.read(t, h.path)); int64(size) >= 512 {
		t.Fatalf("current log is %d bytes, want it bounded below 512", size)
	}
	if h.read(t, h.archive) == "" {
		t.Fatal("nothing was archived")
	}
	if names := logDirEntries(t, h); len(names) != 2 {
		t.Fatalf("log directory holds %v, want exactly the log and one archive", names)
	}
}

// logDirEntries lists the log directory.
func logDirEntries(t *testing.T, h *logHarness) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(h.path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Mutation: rotate by copy-and-truncate, or by rename without moving the
// descriptor.
//
// A direct write in *any* window of a rotation must survive. Copy-truncate
// loses the ones between the copy and the truncate; a rename that leaves fd 2
// behind loses every later one when the archive is next replaced.
func TestDirectWritesSurviveEveryRotationPhase(t *testing.T) {
	for _, phase := range []string{"lease-held", "before-archive", "archived", "current-replaced", "descriptor-moved"} {
		t.Run(phase, func(t *testing.T) {
			h := newLogHarness(t, 512)
			marker := "PANIC-AT-" + phase
			fired := false
			testRotatePhase = func(p string) {
				if p != phase || fired {
					return
				}
				fired = true
				h.direct(t, marker+"\n")
			}
			t.Cleanup(func() { testRotatePhase = nil })

			// Exactly one rotation, so the archive still holds whatever the
			// rotation displaced.
			h.log(t, fill(600))
			if !fired {
				t.Fatalf("no rotation reached the %q phase", phase)
			}
			h.somewhere(t, marker)
		})
	}
}

// Mutation: dup the fresh descriptor only over fd 2 but never at all (i.e.
// drop the dup), or archive by copy so fd 2 keeps naming the current file.
//
// After a rotation, writers that never call into this package must be writing
// to the *current* log. If they are still on the archive, their output
// vanishes at the next rotation.
func TestDirectWritesFollowTheCurrentLogAfterRotation(t *testing.T) {
	h := newLogHarness(t, 512)
	for i := range 20 {
		h.log(t, fmt.Sprintf("line-%02d %s", i, fill(64)))
	}
	h.direct(t, "AFTER-ROTATION\n")

	if !strings.Contains(h.read(t, h.path), "AFTER-ROTATION") {
		t.Fatal("a direct write after rotation did not reach the current log; " +
			"the descriptor was left pointing at the archive")
	}
}

// Two rotations, with direct writes in each generation. Only one archive is
// kept, so the oldest generation is expected to be gone -- but nothing from
// the two most recent may be missing.
func TestTwoRotationsLoseNothingFromTheLastTwoGenerations(t *testing.T) {
	h := newLogHarness(t, 512)

	rotations := 0
	testRotatePhase = func(p string) {
		if p == "descriptor-moved" {
			rotations++
			h.direct(t, fmt.Sprintf("GEN-%d-DIRECT\n", rotations))
		}
	}
	t.Cleanup(func() { testRotatePhase = nil })

	h.log(t, fill(600))
	h.log(t, fill(600))
	if rotations != 2 {
		t.Fatalf("%d rotations happened, want exactly 2", rotations)
	}
	// The write made right after the second rotation is in the current log;
	// the one from the first is in the archive.
	h.somewhere(t, "GEN-2-DIRECT")
	h.somewhere(t, "GEN-1-DIRECT")
}

// Mutation: let a rotation failure close, replace or nil the descriptor.
//
// A daemon that dies, or stops logging, because it could not rotate is
// strictly worse than one with a large log.
func TestRotationFailureKeepsTheLoggerAlive(t *testing.T) {
	h := newLogHarness(t, 512)
	breakRotation(t, h)

	h.log(t, fill(600))
	h.log(t, "line-after-failure\n")
	h.direct(t, "STILL-WRITING\n")

	current := h.read(t, h.path)
	if !strings.Contains(current, "cannot rotate") {
		t.Fatal("a rotation failure was never reported")
	}
	if !strings.Contains(current, "STILL-WRITING") {
		t.Fatal("direct writes stopped reaching the log after a failed rotation")
	}
	if !strings.Contains(current, "line-after-failure") {
		t.Fatal("log-package writes stopped after a failed rotation")
	}

	// Reported once per distinct failure, not once per attempt.
	for range 3 {
		h.b.mu.Lock()
		h.b.nextAttempt = time.Now().Add(-time.Second)
		h.b.mu.Unlock()
		h.log(t, fill(600))
	}
	if got := strings.Count(h.read(t, h.path), "cannot rotate"); got != 1 {
		t.Fatalf("rotation failure reported %d times, want 1", got)
	}
}

// Mutation: drop the retry throttle.
func TestRotationFailureIsThrottled(t *testing.T) {
	h := newLogHarness(t, 512)
	breakRotation(t, h)
	h.log(t, fill(600))

	h.b.mu.Lock()
	next := h.b.nextAttempt
	h.b.mu.Unlock()
	if next.IsZero() || time.Until(next) <= 0 {
		t.Fatal("a failed rotation did not back off")
	}

	// Unblock rotation. The next write must still not attempt one.
	fixRotation(t, h)
	h.log(t, fill(600))
	if h.read(t, h.archive) != "" {
		t.Fatal("rotation was retried inside the backoff window")
	}

	// And it resumes once the window closes.
	h.b.mu.Lock()
	h.b.nextAttempt = time.Now().Add(-time.Second)
	h.b.mu.Unlock()
	h.log(t, fill(600))
	if h.read(t, h.archive) == "" {
		t.Fatal("rotation did not resume after the backoff window")
	}
}

// Mutation: create the archive with O_TRUNC before the new log is ready (the
// copy-based design), or replace the archive non-atomically.
//
// A failed rotation must not cost the previous archive: it is the older half
// of the diagnostics, and the failure is often exactly when it is wanted.
func TestFailedRotationPreservesThePreviousArchive(t *testing.T) {
	h := newLogHarness(t, 512)
	// One good rotation, so there is an archive worth protecting.
	h.log(t, "first-generation "+fill(600))
	if !strings.Contains(h.read(t, h.archive), "first-generation") {
		t.Fatal("the first rotation did not archive the first generation")
	}

	// Now break rotation and drive another one.
	breakRotation(t, h)
	h.log(t, "second-generation "+fill(600))

	if !strings.Contains(h.read(t, h.archive), "first-generation") {
		t.Fatal("a failed rotation destroyed the previous archive")
	}
}

// ENOSPC and friends: a rename that cannot happen must leave the log usable
// and the archive intact. Simulated with a read-only directory, which is the
// closest reachable analogue of a filesystem that refuses writes.
func TestRotationSurvivesAnUnwritableDirectory(t *testing.T) {
	h := newLogHarness(t, 512)
	dir := filepath.Dir(h.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	h.log(t, "line-before "+fill(600))
	h.log(t, "line-after\n")
	h.direct(t, "SURVIVED\n")

	current := h.read(t, h.path)
	if !strings.Contains(current, "SURVIVED") || !strings.Contains(current, "line-after") {
		t.Fatal("logging did not survive an unwritable log directory")
	}
}

// Mutation: drop the rotation lease (and the size re-check under it).
//
// Two daemons can briefly overlap during a handover -- the incumbent is still
// draining while its replacement has taken ownership. Both bound the same
// file. Without a lease the second one walks into the middle of the first
// one's rotation: it finds the current log already renamed, deletes the fresh
// log the first one prepared, and leaves the directory (and the surviving
// descriptor) in a state nobody planned.
//
// The barrier parks the first rotation mid-flight, which is the interleaving
// a bare goroutine race reaches only occasionally.
func TestConcurrentRotatorsSerialiseOnTheLogLease(t *testing.T) {
	h := newLogHarness(t, 512)
	// A second bounded logger over the same file, as an overlapping daemon
	// would have.
	other := &boundedLog{
		fd:      syscall.Stderr,
		path:    h.path,
		archive: h.archive,
		limit:   512,
		stop:    make(chan struct{}),
	}

	parked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	testRotatePhase = func(p string) {
		if p != "archived" {
			return
		}
		once.Do(func() {
			close(parked)
			<-release
		})
	}
	t.Cleanup(func() { testRotatePhase = nil })

	// Drive the first rotation off the test goroutine, since it parks
	// mid-flight while holding its own lock.
	var wg sync.WaitGroup
	wg.Add(1)
	writeErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		_, err := h.b.Write([]byte(fill(600)))
		writeErr <- err
	}()

	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("the first rotation never reached its parked phase")
	}

	// The overlapping daemon tries to rotate the same file, right now.
	other.mu.Lock()
	other.rotateIfLargeLocked()
	other.mu.Unlock()

	close(release)
	wg.Wait()
	if err := <-writeErr; err != nil {
		t.Fatalf("write during a contended rotation: %v", err)
	}

	// Exactly one rotation: the log directory holds the log and one archive,
	// and no leftover .new file.
	names := logDirEntries(t, h)
	if len(names) != 2 {
		t.Fatalf("log directory holds %v, want exactly the log and one archive", names)
	}
	h.direct(t, "AFTER-CONTENTION\n")
	if !strings.Contains(h.read(t, h.path), "AFTER-CONTENTION") {
		t.Fatal("the descriptor does not name the current log after a contended rotation")
	}
}

// Mutation: keep rotating after the descriptor move fails.
//
// If fd 2 could not be moved it still names the archive. Logging survives --
// that file exists and is readable -- but a further rotation would rename the
// fresh log over that archive and unlink the inode being written to, turning
// a recoverable failure into silent loss. Bounding stops instead.
func TestRotationStopsAfterAFailedDescriptorMove(t *testing.T) {
	h := newLogHarness(t, 512)
	dupOntoFn = func(int, int) error { return errors.New("simulated dup failure") }
	t.Cleanup(func() { dupOntoFn = dupOnto })

	h.log(t, "first-generation "+fill(600))
	h.direct(t, "STILL-READABLE\n")

	// fd 2 now names the archive, and that file must keep receiving writes.
	if !strings.Contains(h.read(t, h.archive), "STILL-READABLE") {
		t.Fatal("writes stopped landing anywhere readable after a failed descriptor move")
	}

	// Further rotations must not happen: the archive holding the live
	// descriptor may not be replaced.
	dupOntoFn = dupOnto
	h.b.mu.Lock()
	h.b.nextAttempt = time.Time{}
	h.b.mu.Unlock()
	h.log(t, "second-generation "+fill(600))
	h.direct(t, "STILL-READABLE-LATER\n")

	if !strings.Contains(h.read(t, h.archive), "STILL-READABLE-LATER") {
		t.Fatal("a rotation after a failed descriptor move unlinked the inode being written to")
	}
}

// Mutation: drop the chunking, or check the size only after writing.
//
// One enormous log line must not carry the file arbitrarily past the limit and
// leave it there.
func TestOneHugeWriteDoesNotEscapeTheBound(t *testing.T) {
	const limit = 4 << 20
	h := newLogHarness(t, limit)
	h.log(t, strings.Repeat("y", 12<<20)+"\n")

	// Neither file may hold the whole write: without chunking the log grows
	// to 12 MiB before anything notices, and the archive keeps it there.
	if size := int64(len(h.read(t, h.path))); size > limit+writeChunk {
		t.Fatalf("current log is %d bytes after one huge write, want at most limit+%d", size, writeChunk)
	}
	if size := int64(len(h.read(t, h.archive))); size > limit+writeChunk {
		t.Fatalf("archive is %d bytes after one huge write, want at most limit+%d", size, writeChunk)
	}
}

// Mutation: remove the background sweep.
//
// A daemon whose only output comes from writers that bypass the log package --
// an inherited child, a stream of panics -- must still end up bounded.
func TestSweepBoundsALogNobodyIsLoggingTo(t *testing.T) {
	h := newLogHarness(t, 512)
	h.direct(t, fill(600))

	// The sweep would do this on its own timer; drive one iteration rather
	// than sleeping for it.
	h.b.mu.Lock()
	h.b.rotateIfLargeLocked()
	h.b.mu.Unlock()

	if h.read(t, h.archive) == "" {
		t.Fatal("a log grown entirely by direct writes was never rotated")
	}
	if size := len(h.read(t, h.path)); size != 0 {
		t.Fatalf("current log is %d bytes after rotation, want 0", size)
	}
}

// Mutation: remove the background sweep goroutine (or its Stop join).
//
// Nothing here writes through the log package: this is the daemon whose only
// output comes from an inherited child or a panic, which is exactly the case
// a write-triggered bound cannot see.
func TestSweepRotatesWithoutAnyLogPackageWrites(t *testing.T) {
	restore := sweepInterval
	sweepInterval = 5 * time.Millisecond
	t.Cleanup(func() { sweepInterval = restore })

	h := newLogHarness(t, 512)
	h.direct(t, fill(600))

	deadline := time.Now().Add(5 * time.Second)
	for h.read(t, h.archive) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the background sweep never rotated a log grown entirely by direct writes")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Stop must join the sweeper; a second Stop must be harmless.
	h.b.Stop()
	h.b.Stop()
}

// Mutation: drop the chmod, or the SameFile / fd-2 gates.
func TestInstallBoundedLogGatesAndPermissions(t *testing.T) {
	t.Run("tightens a permissive log", func(t *testing.T) {
		h := newLogHarness(t, 512)
		if err := os.Chmod(h.path, 0o644); err != nil {
			t.Fatal(err)
		}
		// Re-install over the same descriptor.
		if b := installBoundedLog(os.Stderr, h.path, 512); b == nil {
			t.Fatal("declined the process's own log")
		} else {
			t.Cleanup(b.Stop)
		}
		fi, err := os.Stat(h.path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("log mode = %o, want 600", perm)
		}
	})

	t.Run("declines a file that is not the daemon log", func(t *testing.T) {
		dir := t.TempDir()
		other := filepath.Join(dir, "operator-chosen.log")
		f, err := os.OpenFile(other, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if b := installBoundedLog(f, filepath.Join(dir, "gmuxd.log"), 512); b != nil {
			b.Stop()
			t.Fatal("bounded a file that is not the daemon log")
		}
	})

	t.Run("declines a pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if b := installBoundedLog(w, filepath.Join(t.TempDir(), "gmuxd.log"), 512); b != nil {
			b.Stop()
			t.Fatal("bounded a pipe")
		}
	})

	t.Run("declines a log that is not fd 2", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "gmuxd.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		// Same file, right pathname, wrong descriptor: bounding would leave
		// every direct stderr write behind.
		if b := installBoundedLog(f, path, 512); b != nil {
			b.Stop()
			t.Fatal("bounded a log that is not the process's stderr")
		}
	})

	t.Run("declines a non-file writer", func(t *testing.T) {
		if b := installBoundedLog(&strings.Builder{}, filepath.Join(t.TempDir(), "gmuxd.log"), 512); b != nil {
			b.Stop()
			t.Fatal("bounded a non-file writer")
		}
	})
}

// Mutation: give the launcher back its O_TRUNC (or drop O_APPEND).
//
// The launcher opens the log before the child has checked for a healthy
// incumbent, and most invocations that get this far are autostarts that will
// bounce straight off one: truncating destroys the running daemon's log every
// time a health probe is slow.
func TestLauncherOpensLogAppendOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gmuxd.log")
	incumbent := "the incumbent daemon's history\n"
	if err := os.WriteFile(path, []byte(incumbent), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := openDaemonLog(path)
	if err != nil {
		t.Fatalf("openDaemonLog: %v", err)
	}
	defer f.Close()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != incumbent {
		t.Fatalf("opening the log truncated it; content = %q", content)
	}
	// O_APPEND: writes land at the end even after the file has been replaced
	// or truncated behind this descriptor's back.
	if _, err := f.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second\n" {
		t.Fatalf("log content = %q, want %q (a non-appending descriptor leaves a hole)", content, "second\n")
	}
	if fi, statErr := os.Stat(path); statErr != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v (err %v), want 600", fi.Mode().Perm(), statErr)
	}
}

// ── inheritance invariant ───────────────────────────────────────────────────

func fdIsCloseOnExec(t *testing.T, fd int) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("fcntl F_GETFD on fd %d: %v", fd, errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
}

// Rotation moves this process's descriptors. It cannot move a child's, so a
// child that inherited one keeps writing to the archive after one rotation and
// to an unlinked inode after the next. The invariant that makes that
// unreachable is that no descendant holds a log descriptor at all.
//
// Mutation: drop markCloseOnExec at install, or drop O_CLOEXEC/setCloseOnExec
// from dupOnto (which is how the flag would be lost on the second rotation).
func TestDaemonLogDescriptorsAreCloseOnExec(t *testing.T) {
	h := newLogHarness(t, 512)
	if !fdIsCloseOnExec(t, syscall.Stderr) {
		t.Fatal("the log descriptor is not close-on-exec: a child could inherit it")
	}
	// And it stays that way across rotations -- dup2 does not carry the flag.
	for range 2 {
		h.log(t, fill(600))
		if !fdIsCloseOnExec(t, syscall.Stderr) {
			t.Fatal("the log descriptor lost close-on-exec across a rotation")
		}
	}
}

// The invariant, observed from a real child process across two rotations.
//
// The child writes to its own stderr throughout. With the invariant intact it
// never had the daemon log to begin with, so nothing it writes can land in an
// inode the second rotation unlinks -- and nothing of the daemon's is lost to
// it either.
//
// Mutation: wire the child's stderr to the daemon's log (`cmd.Stderr =
// os.Stderr`), which is what an unaudited child launch looks like. Its second
// marker then lands in an unnamed inode and this test fails.
func TestNoChildProcessRetainsTheDaemonLogAcrossRotations(t *testing.T) {
	h := newLogHarness(t, 512)

	// A child that writes on demand, the way a long-lived helper does.
	script := `while read -r line; do printf 'CHILD-%s\n' "$line" >&2; printf 'ack\n'; done`
	cmd := exec.Command("sh", "-c", script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Production wiring: explicit, and to nothing. See productionRunnerSpawner.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _, _ = cmd.Process.Wait() })
	acks := bufio.NewReader(stdout)
	tell := func(what string) {
		if _, err := stdin.Write([]byte(what + "\n")); err != nil {
			t.Fatalf("tell child %q: %v", what, err)
		}
		if _, err := acks.ReadString('\n'); err != nil {
			t.Fatalf("child ack for %q: %v", what, err)
		}
	}

	tell("before-any-rotation")
	h.log(t, fill(600)) // rotation 1
	tell("after-first-rotation")
	h.log(t, fill(600)) // rotation 2
	tell("after-second-rotation")

	// The child's output is not in the daemon's log at all: it never had a
	// descriptor for it. That is the invariant -- not "its writes survive
	// rotation", but "it cannot be writing there".
	for _, path := range []string{h.path, h.archive} {
		if strings.Contains(h.read(t, path), "CHILD-") {
			t.Fatalf("a child's output reached %s; it inherited a log descriptor", path)
		}
	}
	// The daemon's own writes are still whole across both rotations.
	h.direct(t, "DAEMON-AFTER-TWO-ROTATIONS\n")
	if !strings.Contains(h.read(t, h.path), "DAEMON-AFTER-TWO-ROTATIONS") {
		t.Fatal("the daemon's own descriptor stopped naming the current log")
	}
}

// A descriptor inherited *before* bounding was installed is the residual this
// invariant does not cover on its own, and the reason the flag is set as early
// as it is: this test pins where the boundary lies, so nobody mistakes the
// invariant for something wider than it is.
func TestDescriptorInheritedBeforeBoundingIsNotTracked(t *testing.T) {
	h := newLogHarness(t, 512)

	// Stand in for a process that got the log before the flag was set: a
	// second descriptor on the same inode, which rotation cannot move.
	pre, err := syscall.Dup(syscall.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(pre)

	h.log(t, fill(600)) // rotation 1: the retained descriptor now names the archive
	if _, err := syscall.Write(pre, []byte("RETAINED-AFTER-ONE\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.read(t, h.archive), "RETAINED-AFTER-ONE") {
		t.Fatal("a retained descriptor's write after one rotation was not in the archive")
	}

	h.log(t, fill(600)) // rotation 2: the archive it names is unlinked
	if _, err := syscall.Write(pre, []byte("RETAINED-AFTER-TWO\n")); err != nil {
		t.Fatal(err)
	}
	inCurrent := strings.Contains(h.read(t, h.path), "RETAINED-AFTER-TWO")
	inArchive := strings.Contains(h.read(t, h.archive), "RETAINED-AFTER-TWO")
	if inCurrent || inArchive {
		t.Skip("this platform kept the retained descriptor reachable; the residual does not apply")
	}
	// Documented, not fixed: nothing can retarget another open file
	// description. The invariant above is what keeps this unreachable in
	// production -- no descendant has one.
	t.Log("confirmed residual: a descriptor retained from before bounding writes to an unnamed inode after the second rotation")
}

// ── error paths that must not wedge the daemon ──────────────────────────────

// Mutation: log the secondary-descriptor failure from inside rotateLocked
// (i.e. while b.mu is held).
//
// The log package's output *is* this writer, so logging under its own mutex
// re-enters Write and blocks forever -- wedging the sweep goroutine and, with
// it, Stop and shutdown. The primary dup must succeed and the secondary fail,
// which is why the seam is per-call rather than global.
func TestSecondaryDescriptorFailureDoesNotDeadlock(t *testing.T) {
	h := newLogHarness(t, 512)
	// Pretend stdout aliases the log, so there is a secondary descriptor.
	h.b.mu.Lock()
	h.b.dupFDs = []int{syscall.Stdout}
	h.b.mu.Unlock()

	var calls int
	dupOntoFn = func(src, dst int) error {
		calls++
		if dst == syscall.Stdout {
			return errors.New("simulated secondary dup failure")
		}
		return dupOnto(src, dst)
	}
	t.Cleanup(func() { dupOntoFn = dupOnto })

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.log(t, fill(600))
		// The complaint is emitted after the lock is released; a further write
		// must also not block.
		h.log(t, "still-writing\n")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the logger deadlocked reporting a secondary descriptor failure")
	}

	stopped := make(chan struct{})
	go func() { defer close(stopped); h.b.Stop() }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop hung: the sweep goroutine is wedged")
	}

	if calls < 2 {
		t.Fatalf("dup calls = %d, want the primary and the secondary", calls)
	}
	current := h.read(t, h.path)
	if !strings.Contains(current, "still-writing") {
		t.Fatal("writes stopped after a secondary descriptor failure")
	}
	if !strings.Contains(current, "still points at the archived log") {
		t.Fatalf("the secondary descriptor failure was never reported: %q", current)
	}
}

// Mutation: report a short write as a complete one (return nil), or spin on it.
//
// A truncated diagnostic that the caller believes was written in full is worse
// than an error, and the standard logger is exactly such a caller.
func TestShortWritesAreRetriedAndReported(t *testing.T) {
	t.Run("a short write is finished, not abandoned", func(t *testing.T) {
		h := newLogHarness(t, 1<<20)
		var calls int
		writeFn = func(fd int, p []byte) (int, error) {
			calls++
			if len(p) > 4 {
				p = p[:4] // pretend the kernel took only part of it
			}
			return syscall.Write(fd, p)
		}
		t.Cleanup(func() { writeFn = syscall.Write })

		payload := "abcdefghijklmnopqrst\n"
		n, err := h.b.Write([]byte(payload))
		if err != nil {
			t.Fatalf("Write = %v, want nil once the remainder is written", err)
		}
		if n != len(payload) {
			t.Fatalf("wrote %d of %d bytes", n, len(payload))
		}
		if calls < 2 {
			t.Fatalf("write calls = %d: the remainder was not retried", calls)
		}
		if got := h.read(t, h.path); !strings.Contains(got, payload) {
			t.Fatalf("log holds %q, want the whole line", got)
		}
	})

	t.Run("no progress is an error", func(t *testing.T) {
		h := newLogHarness(t, 1<<20)
		writeFn = func(int, []byte) (int, error) { return 0, nil }
		t.Cleanup(func() { writeFn = syscall.Write })

		n, err := h.b.Write([]byte("anything\n"))
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write = (%d, %v), want io.ErrShortWrite", n, err)
		}
		if n != 0 {
			t.Fatalf("reported %d bytes written", n)
		}
	})

	// The kernel's convention, not a convenient one: write(2) reports n = -1
	// whenever errno is set, and a partial write interrupted by a signal comes
	// back as a *successful* short write rather than EINTR. A retry that
	// advances the buffer by the returned count therefore indexes with -1 and
	// panics -- inside the logger, under the logger's own mutex.
	//
	// Mutation: advance the buffer by n in the EINTR branch (`p = p[n:]`).
	t.Run("EINTR follows the syscall convention and is retried", func(t *testing.T) {
		h := newLogHarness(t, 1<<20)
		first := true
		writeFn = func(fd int, p []byte) (int, error) {
			if first {
				first = false
				return -1, syscall.EINTR // exactly what the syscall returns
			}
			return syscall.Write(fd, p)
		}
		t.Cleanup(func() { writeFn = syscall.Write })

		payload := "interrupted\n"
		n, err := h.b.Write([]byte(payload))
		if err != nil || n != len(payload) {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
		}
		if got := h.read(t, h.path); !strings.Contains(got, payload) {
			t.Fatalf("log holds %q, want the whole line", got)
		}
	})

	// Any other error also arrives with n = -1, and must not be counted as
	// written or used to advance the buffer.
	t.Run("a failed write reports nothing written", func(t *testing.T) {
		h := newLogHarness(t, 1<<20)
		writeFn = func(int, []byte) (int, error) { return -1, syscall.EIO }
		t.Cleanup(func() { writeFn = syscall.Write })

		n, err := h.b.Write([]byte("doomed\n"))
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("Write = (%d, %v), want EIO", n, err)
		}
		if n != 0 {
			t.Fatalf("reported %d bytes written for a failed write", n)
		}
	})

	// A relentless signal storm must not spin inside the mutex forever.
	t.Run("endless EINTR gives up", func(t *testing.T) {
		h := newLogHarness(t, 1<<20)
		calls := 0
		writeFn = func(int, []byte) (int, error) {
			calls++
			return -1, syscall.EINTR
		}
		t.Cleanup(func() { writeFn = syscall.Write })

		done := make(chan error, 1)
		go func() { _, err := h.b.Write([]byte("interrupted forever\n")); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, syscall.EINTR) {
				t.Fatalf("Write = %v, want EINTR after the retry budget", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Write spun on EINTR without a bound")
		}
		if calls <= 1 || calls > maxWriteInterrupts+2 {
			t.Fatalf("write attempts = %d, want a bounded retry", calls)
		}
	})
}

// The runner spawner is the launch that matters most for the inheritance
// invariant: a runner outlives the daemon that spawned it, so a runner holding
// a log descriptor would still be writing to an unnamed inode long after two
// rotations.
//
// Mutation: wire the daemon's stderr into the runner
// (`cmd.Stderr = os.Stderr` in launchRunnerProcess).
func TestSpawnedRunnerGetsNoDaemonLogDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor introspection uses /proc")
	}
	h := newLogHarness(t, 1<<20)

	// A stand-in "runner" that reports what its stdio actually is.
	dir := t.TempDir()
	report := filepath.Join(dir, "fds")
	stub := filepath.Join(dir, "gmux")
	// Duplicate the inherited descriptors aside first, so the report's own
	// redirection does not become the thing being reported.
	script := "#!/bin/sh\n" +
		// The spawner also invokes the binary to capture login env; only the
		// actual launch is being observed here.
		"case \"$1\" in __dump-env) exit 0;; esac\n" +
		"exec 3>&1 4>&2\n" +
		"{ readlink /proc/self/fd/3; readlink /proc/self/fd/4; } > " + report + "\n" +
		"echo RUNNER-STDERR-MARKER >&4\n" +
		"echo RUNNER-STDOUT-MARKER >&3\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := launchRunnerProcess(context.Background(), runnerLaunchRequest{
		GmuxBin: stub, Command: []string{"true"}, CWD: dir,
	})
	if err != nil {
		t.Fatalf("launchRunnerProcess: %v", err)
	}
	t.Cleanup(func() { _ = res.Terminate(context.Background()) })

	deadline := time.Now().Add(10 * time.Second)
	var fds string
	for {
		data, readErr := os.ReadFile(report)
		if readErr == nil && strings.Count(string(data), "\n") >= 2 {
			fds = string(data)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stub runner never reported its descriptors (%v)", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, line := range strings.Split(strings.TrimSpace(fds), "\n") {
		if line != os.DevNull {
			t.Errorf("a spawned runner's stdio is %q, want %s: it must not hold anything of the daemon's, "+
				"least of all the log", line, os.DevNull)
		}
	}
	// And nothing it wrote reached the daemon's log.
	for _, path := range []string{h.path, h.archive} {
		if strings.Contains(h.read(t, path), "RUNNER-ST") {
			t.Fatalf("a spawned runner's output reached %s", path)
		}
	}
}

// dupOnto is what keeps the invariant true across rotations, so assert it on
// its own rather than only through a rotation -- where markCloseOnExec would
// mask its omission.
//
// Mutation: drop O_CLOEXEC from Dup3 (or setCloseOnExec from the Darwin path).
func TestDupOntoMarksTheDestinationCloseOnExec(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Reserve a descriptor number to be the destination, and make sure it does
	// *not* already carry the flag, so the assertion is about dupOnto.
	dst, err := syscall.Dup(syscall.Stdin)
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	defer syscall.Close(dst)
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(dst), syscall.F_SETFD, 0); errno != 0 {
		t.Fatalf("clear close-on-exec: %v", errno)
	}
	if fdIsCloseOnExec(t, dst) {
		t.Fatal("could not clear close-on-exec on the destination")
	}

	if err := dupOnto(int(f.Fd()), dst); err != nil {
		t.Fatalf("dupOnto: %v", err)
	}
	if !fdIsCloseOnExec(t, dst) {
		t.Fatal("dupOnto installed a descriptor that a child would inherit")
	}
	// And it really is the same open file.
	var a, b syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Fstat(dst, &b); err != nil {
		t.Fatal(err)
	}
	if a.Ino != b.Ino || a.Dev != b.Dev {
		t.Fatal("dupOnto did not point the destination at the source's file")
	}
}
