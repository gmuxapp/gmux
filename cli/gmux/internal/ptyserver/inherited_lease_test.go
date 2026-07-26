package ptyserver

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/packages/socklease"
)

func TestBindSocketWithInheritedLeaseTransfersWithoutReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.sock")
	parent, err := socklease.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	childFD, err := parent.DuplicateForExec()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.ReleaseForTransfer(); err != nil {
		t.Fatal(err)
	}
	bound, err := BindSocketWithInheritedLease(path, childFD)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = bound.ReleaseOwnership()
		_ = bound.Close()
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("inherited lease did not bind canonical socket: %v", err)
	}
	conn.Close()
	if _, err := socklease.AcquireExisting(path); err != socklease.ErrHeld {
		t.Fatalf("child did not retain inherited lease: %v", err)
	}
}
