package statetool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ResetState removes rebuildable daemon state while holding the same advisory
// ownership lock used by gmuxd. The caller must stop the daemon first. Backups,
// configuration, authentication material, and logs in stateDir are preserved.
func ResetState(stateDir string, sessionSocketDirs ...string) error {
	if stateDir == "" {
		return errors.New("statetool: empty state directory")
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("statetool: open lock file: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%w: advisory lock %s is held", ErrDaemonOwnsDatabase, LockFileName)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	// Fence every lease-aware runner bind before inspecting the namespace.
	// Acquire all directory fences and all existing leases before deleting a
	// single byte of durable state, so every refusal is genuinely fail-closed.
	var guards []*os.File
	seenDirs := map[string]bool{}
	allowedDirs := map[string]bool{filepath.Clean(filepath.Join(stateDir, "run", "sessions")): true}
	for _, dir := range paths.LegacySessionSocketDirs() {
		allowedDirs[filepath.Clean(dir)] = true
	}
	for _, dir := range sessionSocketDirs {
		if dir == "" {
			continue
		}
		dir = filepath.Clean(dir)
		if !allowedDirs[dir] {
			return fmt.Errorf("refusing unsafe session socket directory %s", dir)
		}
		if seenDirs[dir] {
			continue
		}
		seenDirs[dir] = true
		guard, err := socklease.LockNamespace(dir, true)
		if err != nil {
			for _, held := range guards {
				socklease.UnlockNamespace(held)
			}
			return err
		}
		guards = append(guards, guard)
	}
	defer func() {
		for _, guard := range guards {
			socklease.UnlockNamespace(guard)
		}
	}()

	type artifact struct {
		socket, lock, registering string
		lease                     *os.File
	}
	var artifacts []artifact
	defer func() {
		for _, a := range artifacts {
			if a.lease != nil {
				_ = syscall.Flock(int(a.lease.Fd()), syscall.LOCK_UN)
				_ = a.lease.Close()
			}
		}
	}()
	for dir := range seenDirs {
		locks, err := filepath.Glob(filepath.Join(dir, "*.sock.lock"))
		if err != nil {
			return fmt.Errorf("list socket leases in %s: %w", dir, err)
		}
		sort.Strings(locks)
		for _, lockPath := range locks {
			id := strings.TrimSuffix(filepath.Base(lockPath), ".sock.lock")
			if !paths.IsValidSessionID(id) {
				continue // suffix alone is not proof that this belongs to gmux
			}
			f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("open socket lease %s: %w", lockPath, err)
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				_ = f.Close()
				return fmt.Errorf("session socket lease remains held: %s", filepath.Base(lockPath))
			}
			socket := strings.TrimSuffix(lockPath, ".lock")
			artifacts = append(artifacts, artifact{socket: socket, lock: lockPath, registering: socket + ".registering", lease: f})
		}
		sockets, err := filepath.Glob(filepath.Join(dir, "*.sock"))
		if err != nil {
			return fmt.Errorf("list sockets in %s: %w", dir, err)
		}
		for _, socket := range sockets {
			id := strings.TrimSuffix(filepath.Base(socket), ".sock")
			if !paths.IsValidSessionID(id) {
				continue
			}
			found := false
			for _, a := range artifacts {
				found = found || a.socket == socket
			}
			if !found {
				artifacts = append(artifacts, artifact{socket: socket, registering: socket + ".registering"})
			}
		}
	}
	db := centralstore.DatabasePath(stateDir)
	for _, path := range []string{db, db + "-wal", db + "-shm"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(stateDir, "sessions")); err != nil {
		return fmt.Errorf("remove retained sessions: %w", err)
	}
	for _, a := range artifacts {
		for _, path := range []string{a.socket, a.lock, a.registering} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
	}
	return nil
}
