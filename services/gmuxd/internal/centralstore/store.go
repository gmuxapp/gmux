// Package centralstore provides private SQLite schema and ordering primitives.
// It is intentionally not wired into gmuxd and is not yet an authoritative
// lifecycle or project-membership boundary.
package centralstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

const databaseName = "state.db"

// DatabaseName is the central store filename within the state directory.
// Admin/offline tooling must use this instead of duplicating the name.
const DatabaseName = databaseName

// DatabasePath returns the central store path in dir.
func DatabasePath(dir string) string { return filepath.Join(dir, DatabaseName) }

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store owns the database handle. Its generated models and queries remain
// private implementation details.
//
// The store maintains two connection pools:
//   - database (write pool): single connection for all mutations. SQLite
//     serializes writers anyway; a pool of 1 avoids SQLITE_BUSY retries.
//   - readDB (read pool): up to 4 connections opened in WAL read-only mode
//     (query_only pragma) for ReadSnapshot, Session, ListSessions, and other
//     pure-read methods. WAL mode allows concurrent readers alongside the
//     single writer, so read-only queries never block mutations and vice
//     versa. This separation eliminates the connection-pool-level
//     serialization that caused lifecycle starvation under REST read load
//     (see §2a incident report).
type Store struct {
	database *sql.DB
	readDB   *sql.DB
	queries  *db.Queries
	readQ    *db.Queries

	// beforePlacementFinalize is a test-only fault-injection seam. Production
	// construction leaves it nil.
	beforePlacementFinalize func() error

	// betweenSnapshotQueries is a test-only seam invoked inside ReadSnapshot's
	// read transaction, between component queries, to prove the transaction
	// isolates the composition from concurrent writers. Production
	// construction leaves it nil.
	betweenSnapshotQueries func()

	// beforeBackupLink is a test-only seam for manufacturing the target-
	// appears-after-Stat race. Production construction leaves it nil.
	beforeBackupLink func()
}

// Open creates (when absent), configures, and migrates the database in dir.
func Open(ctx context.Context, dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("centralstore: empty state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("centralstore: create state directory: %w", err)
	}
	// MkdirAll is a no-op on an existing directory; tighten its mode
	// unconditionally so ancillary files (WAL/SHM) are not advertised by a
	// previously loose directory.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("centralstore: secure state directory: %w", err)
	}

	path := DatabasePath(dir)
	// Pre-create the database file with owner-only permissions so the driver
	// never creates it with a umask-derived mode (removing the
	// created-then-chmod TOCTOU window), then tighten unconditionally so a
	// pre-existing loose-mode file (backup restore, crash before chmod) is
	// repaired on every open.
	handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("centralstore: create database file: %w", err)
	}
	if err := handle.Close(); err != nil {
		return nil, fmt.Errorf("centralstore: create database file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("centralstore: secure database: %w", err)
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("centralstore: absolute database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("centralstore: open database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	// Read-only pool: WAL readers never block the single writer and vice
	// versa. query_only(ON) prevents accidental mutation. 4 connections
	// covers REST handlers + the composer without excessive file-handle
	// overhead.
	readDSN := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?_pragma=journal_mode(WAL)&_pragma=query_only(ON)&_pragma=busy_timeout(5000)"
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("centralstore: open read database: %w", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)

	closeOnError := func(openErr error) (*Store, error) {
		_ = readDB.Close()
		_ = database.Close()
		return nil, openErr
	}
	if err := database.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("centralstore: connect: %w", err))
	}
	if err := readDB.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("centralstore: connect read pool: %w", err))
	}
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return closeOnError(fmt.Errorf("centralstore: load migrations: %w", err))
	}
	if err := migrate(ctx, database, migrations); err != nil {
		return closeOnError(err)
	}
	// Post-migration integrity gate (ADR 0026 §10): a corrupt page that
	// migration didn't touch must still stop startup fail-closed.
	if err := quickCheck(ctx, database); err != nil {
		return closeOnError(err)
	}
	return &Store{database: database, readDB: readDB, queries: db.New(database), readQ: db.New(readDB)}, nil
}

// gooseVersionTable is goose's migration-bookkeeping table. The provider
// below uses goose's default name; Verify's read-only schema sanity read
// queries the same table through this constant — if the provider is ever
// configured with a custom table name, change both here.
const gooseVersionTable = "goose_db_version"

func migrate(ctx context.Context, database *sql.DB, files fs.FS) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, files)
	if err != nil {
		return fmt.Errorf("centralstore: configure migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("centralstore: migrate: %w", err)
	}
	return nil
}

// Close releases both connection pools. When the read pool is the same
// handle as the write pool (OpenReadOnly), only one close is performed.
func (s *Store) Close() error {
	if s.readDB != s.database {
		if err := s.readDB.Close(); err != nil {
			_ = s.database.Close()
			return err
		}
	}
	return s.database.Close()
}

// SchemaVersion returns the current embedded migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return 0, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.database, migrations)
	if err != nil {
		return 0, err
	}
	return provider.GetDBVersion(ctx)
}
