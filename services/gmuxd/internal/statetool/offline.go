package statetool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// LockFileName is the daemon's advisory lock file next to the database
// (design §1 phase 1b). A running daemon holds an exclusive flock on it for
// its lifetime; acquiring the flock is the authoritative ownership signal
// the offline gate checks. S4 must keep the inode in place for the daemon's
// lifetime and on shutdown: never unlink gmuxd.lock, because unlinking a
// flocked file lets another process lock a fresh inode under the same name.
const LockFileName = "gmuxd.lock"

// ErrDaemonOwnsDatabase marks an offline-mode refusal: something (most
// likely a live daemon) appears to own the database, so offline check or
// backup must not proceed.
var ErrDaemonOwnsDatabase = errors.New("statetool: daemon appears to own the database")

// OpenOffline acquires the offline gate for stateDir, in the design's
// order: (1) exclusive non-blocking flock on the advisory lock file —
// failure means a daemon owns the database, refuse; (2) daemonHealthy
// heuristic — a daemon answering its health endpoint means online mode must
// be used instead; (3) a `BEGIN IMMEDIATE` transaction on the main file,
// held open until Close. Only then is the read-only handle opened.
//
// The caller owns Close on success.
func OpenOffline(ctx context.Context, stateDir string, daemonHealthy func() bool) (*OfflineHandle, error) {
	if stateDir == "" {
		return nil, errors.New("statetool: empty state directory")
	}
	dbPath := centralstore.DatabasePath(stateDir)
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("statetool: no database at %s", dbPath)
	} else if err != nil {
		return nil, fmt.Errorf("statetool: stat database: %w", err)
	}

	// (1) Advisory lock — the authoritative ownership signal.
	lockFile, err := os.OpenFile(filepath.Join(stateDir, LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("statetool: open lock file: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("%w: advisory lock %s is held", ErrDaemonOwnsDatabase, LockFileName)
	}
	release := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}

	// (2) Health heuristic: a daemon that answers must be used online.
	if daemonHealthy != nil && daemonHealthy() {
		release()
		return nil, fmt.Errorf("%w: the daemon health endpoint answered — use the online path", ErrDaemonOwnsDatabase)
	}

	// (3) BEGIN IMMEDIATE held across the whole operation: an idle daemon
	// holds no SQLite lock between transactions, so a transient probe
	// proves nothing; a held write transaction guarantees at minimum that
	// no writer can commit mid-operation.
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		release()
		return nil, fmt.Errorf("statetool: absolute database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?mode=rw&_pragma=busy_timeout(2000)"
	holdDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		release()
		return nil, fmt.Errorf("statetool: open write hold: %w", err)
	}
	holdDB.SetMaxOpenConns(1)
	holdConn, err := holdDB.Conn(ctx)
	if err == nil {
		_, err = holdConn.ExecContext(ctx, "BEGIN IMMEDIATE")
	}
	if err != nil {
		if holdConn != nil {
			_ = holdConn.Close()
		}
		_ = holdDB.Close()
		release()
		return nil, fmt.Errorf("%w: cannot hold a write transaction: %v", ErrDaemonOwnsDatabase, err)
	}

	return &OfflineHandle{lockFile: lockFile, holdDB: holdDB, holdConn: holdConn, stateDir: stateDir}, nil
}

// OfflineHandle is the acquired offline gate. StateDir is the directory the
// gate was taken for; open the read-only store against it while the handle
// is held.
type OfflineHandle struct {
	lockFile *os.File
	holdDB   *sql.DB
	holdConn *sql.Conn
	stateDir string
}

// StateDir returns the gated state directory.
func (h *OfflineHandle) StateDir() string { return h.stateDir }

// Close releases the held transaction, the write handle, and the advisory
// lock, in reverse acquisition order.
func (h *OfflineHandle) Close() error {
	var first error
	if _, err := h.holdConn.ExecContext(context.Background(), "ROLLBACK"); err != nil && first == nil {
		first = err
	}
	if err := h.holdConn.Close(); err != nil && first == nil {
		first = err
	}
	if err := h.holdDB.Close(); err != nil && first == nil {
		first = err
	}
	if err := syscall.Flock(int(h.lockFile.Fd()), syscall.LOCK_UN); err != nil && first == nil {
		first = err
	}
	if err := h.lockFile.Close(); err != nil && first == nil {
		first = err
	}
	return first
}
