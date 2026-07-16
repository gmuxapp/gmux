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

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store owns the database handle. Its generated models and queries remain
// private implementation details.
type Store struct {
	database *sql.DB
	queries  *db.Queries

	// beforePlacementFinalize is a test-only fault-injection seam. Production
	// construction leaves it nil.
	beforePlacementFinalize func() error

	// betweenSnapshotQueries is a test-only seam invoked inside ReadSnapshot's
	// read transaction, between component queries, to prove the transaction
	// isolates the composition from concurrent writers. Production
	// construction leaves it nil.
	betweenSnapshotQueries func()
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

	path := filepath.Join(dir, databaseName)
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

	closeOnError := func(openErr error) (*Store, error) {
		_ = database.Close()
		return nil, openErr
	}
	if err := database.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("centralstore: connect: %w", err))
	}
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return closeOnError(fmt.Errorf("centralstore: load migrations: %w", err))
	}
	if err := migrate(ctx, database, migrations); err != nil {
		return closeOnError(err)
	}
	return &Store{database: database, queries: db.New(database)}, nil
}

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

// Close releases the database connection.
func (s *Store) Close() error { return s.database.Close() }

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
