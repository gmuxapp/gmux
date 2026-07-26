package socklease

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDuplicateForExecIsCloseOnExecExceptForExplicitExtraFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sock")
	lease, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	dup, err := lease.DuplicateForExec()
	if err != nil {
		t.Fatal(err)
	}
	defer dup.Close()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, dup.Fd(), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("lease duplicate can leak into unrelated execs")
	}
}

func TestAdoptInheritedRejectsSidecarReplacedBeforeLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sock")
	seed, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.ReleaseKeepingLockFile(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(LockPath(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var replacement *Lease
	_, err = adoptInherited(path, f, func() {
		if err := os.Rename(LockPath(path), LockPath(path)+".parked"); err != nil {
			t.Fatal(err)
		}
		replacement, err = Acquire(path)
		if err != nil {
			t.Fatal(err)
		}
	})
	defer replacement.Release()
	if !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("adopt replaced sidecar error=%v, want identity change", err)
	}
	if contender, err := AcquireExisting(path); !errors.Is(err, ErrHeld) {
		if err == nil {
			_ = contender.ReleaseKeepingLockFile()
		}
		t.Fatalf("replacement lease was not retained: %v", err)
	}
}

func TestAdoptInheritedEstablishesExclusivityForUnlockedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sock")
	seed, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.ReleaseKeepingLockFile(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(LockPath(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := AdoptInherited(path, f)
	if err != nil {
		t.Fatal(err)
	}
	defer adopted.Release()
	if contender, err := AcquireExisting(path); !errors.Is(err, ErrHeld) {
		if err == nil {
			_ = contender.ReleaseKeepingLockFile()
		}
		t.Fatalf("adopted descriptor was not exclusive: %v", err)
	}
}
