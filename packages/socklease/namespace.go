package socklease

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const namespaceLockName = ".gmux-namespace.lock"

// LockNamespace fences creation and reset of socket pathnames in dir. Runners
// hold a shared lock only while acquiring a pathname lease and binding;
// destructive maintenance holds the exclusive lock while proving quiescence
// and removing artifacts.
func LockNamespace(dir string, exclusive bool) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socklease: create namespace: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, namespaceLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("socklease: open namespace lock: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("socklease: lock namespace: %w", err)
	}
	return f, nil
}

// UnlockNamespace releases a lock returned by LockNamespace.
func UnlockNamespace(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
