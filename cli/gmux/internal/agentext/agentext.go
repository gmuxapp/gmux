// Package agentext ships the gmux pi session extension (pi-ext.mjs) and the
// helper the runner uses to materialize it.
//
// pi reports its active session, title, and status authoritatively via its
// own lifecycle events (session_start on every bind; agent_start/agent_end
// for turn status); this extension forwards them to the runner. It is loaded
// via `pi -e <path>`; see pi-ext.mjs for the design comment.
//
// The .mjs source is embedded and materialized to a stable, content-addressed
// path on disk so a single gmux binary self-heals across upgrades and the
// file stays inspectable.
package agentext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "embed"
)

//go:embed pi-ext.mjs
var extSource []byte

var (
	once    = new(sync.Once)
	path    string
	loadErr error
)

// Path materializes the embedded extension to a stable, content-addressed
// file under the user cache dir and returns its absolute path. Subsequent
// calls in the same process return the cached result.
func Path() (string, error) {
	once.Do(func() { path, loadErr = materialize() })
	return path, loadErr
}

func materialize() (string, error) {
	sum := sha256.Sum256(extSource)
	short := hex.EncodeToString(sum[:])[:12]

	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "gmux", "agent-ext")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agentext: mkdir %s: %w", dir, err)
	}
	p := filepath.Join(dir, fmt.Sprintf("pi-ext-%s.mjs", short))
	return materializePath(p, extSource)
}

// materializePath publishes source at its content-addressed path. Detached
// launches are separate gmux processes, so each caller writes a private temp
// and atomically links it into place only if no winner exists. In particular,
// a late publisher must never replace the inode another launch already won.
func materializePath(p string, source []byte) (string, error) {
	if err := validateArtifact(p, source); err == nil {
		return p, nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("agentext: create temporary extension for %s: %w", p, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	// CreateTemp uses 0600. Keep that secure mode: pi reads the extension as
	// the same user, and chmod would bypass a restrictive user umask.
	n, writeErr := tmp.Write(source)
	closeErr := tmp.Close()
	if writeErr != nil {
		return "", fmt.Errorf("agentext: write %s: %w", tmpPath, writeErr)
	}
	if n != len(source) {
		return "", fmt.Errorf("agentext: write %s: short write (%d of %d bytes)", tmpPath, n, len(source))
	}
	if closeErr != nil {
		return "", fmt.Errorf("agentext: close %s: %w", tmpPath, closeErr)
	}

	// A same-directory hard link is an atomic create-if-absent publication.
	// If another process won, validate its artifact rather than replacing it.
	if linkErr := os.Link(tmpPath, p); linkErr == nil {
		if err := validateArtifact(p, source); err != nil {
			return "", err
		}
		return p, nil
	} else if err := validateArtifact(p, source); err == nil {
		return p, nil
	} else if _, statErr := os.Lstat(p); statErr != nil && !os.IsNotExist(statErr) {
		return "", fmt.Errorf("agentext: inspect publication winner %s: %w", p, statErr)
	}

	// A regular but invalid incumbent can be left by a killed writer or local
	// corruption. Repair it only under a process-wide advisory lock. flock is
	// released by the kernel on process death, so there is no stale owner to
	// guess at or lock directory to steal.
	unlock, err := lockArtifact(p + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()

	for attempts := 0; attempts < 3; attempts++ {
		if err := validateArtifact(p, source); err == nil {
			return p, nil
		}
		observed, statErr := os.Lstat(p)
		if os.IsNotExist(statErr) {
			if err := os.Link(tmpPath, p); err != nil {
				continue // a non-cooperating cold-cache publisher may have won
			}
			if err := validateArtifact(p, source); err != nil {
				return "", err
			}
			return p, nil
		}
		if statErr != nil {
			return "", fmt.Errorf("agentext: inspect invalid artifact %s: %w", p, statErr)
		}
		if !observed.Mode().IsRegular() {
			return "", fmt.Errorf("agentext: %s is not a regular file", p)
		}

		// Revalidate pathname identity immediately before removal. Cooperating
		// materializers cannot change it while this lock is held; if an external
		// actor did, retry rather than removing an artifact we did not inspect.
		still, statErr := os.Lstat(p)
		if statErr != nil || !still.Mode().IsRegular() || !os.SameFile(observed, still) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return "", fmt.Errorf("agentext: remove invalid artifact %s: %w", p, err)
		}
		if err := os.Link(tmpPath, p); err != nil {
			continue
		}
		if err := validateArtifact(p, source); err != nil {
			return "", err
		}
		return p, nil
	}
	return "", fmt.Errorf("agentext: artifact %s kept changing during repair", p)
}

const artifactLockTimeout = 5 * time.Second

func lockArtifact(lockPath string) (func(), error) {
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("agentext: open artifact lock %s: %w", lockPath, err)
	}
	closeFD := func() { _ = syscall.Close(fd) }
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		closeFD()
		return nil, fmt.Errorf("agentext: inspect artifact lock %s: %w", lockPath, err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		closeFD()
		return nil, fmt.Errorf("agentext: artifact lock %s is not a regular file", lockPath)
	}

	deadline := time.Now().Add(artifactLockTimeout)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				closeFD()
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			closeFD()
			return nil, fmt.Errorf("agentext: lock artifact %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			closeFD()
			return nil, fmt.Errorf("agentext: timed out locking artifact %s", lockPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// validateArtifact rejects a symlink or any other non-regular final entry.
// It also verifies that the pathname still names the file that was opened;
// cache parent directories may themselves be symlinks.
func validateArtifact(p string, source []byte) error {
	before, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("agentext: inspect %s: %w", p, err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("agentext: %s is not a regular file", p)
	}

	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("agentext: open %s: %w", p, err)
	}
	opened, statErr := f.Stat()
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if statErr != nil {
		return fmt.Errorf("agentext: stat %s: %w", p, statErr)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf("agentext: %s changed while opening", p)
	}
	if readErr != nil {
		return fmt.Errorf("agentext: read %s: %w", p, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("agentext: close %s: %w", p, closeErr)
	}
	after, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("agentext: re-inspect %s: %w", p, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return fmt.Errorf("agentext: %s changed while validating", p)
	}
	if !bytes.Equal(data, source) {
		return fmt.Errorf("agentext: %s has unexpected contents", p)
	}
	return nil
}
