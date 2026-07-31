package centralstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrDatabaseMissing marks a Verify call against a directory that holds no
// database file. Bootstrap treats it as a fresh install and skips
// verification (design §1 phase 1a).
var ErrDatabaseMissing = errors.New("centralstore: database file missing")

// ErrIntegrity marks a `PRAGMA quick_check` failure — the database pages are
// structurally damaged. Distinct from open/schema-read failures so callers
// (and tests) can pin quick_check as the detection mechanism.
var ErrIntegrity = errors.New("centralstore: integrity check failed")

// ErrSchemaTooNew marks a database created by a newer gmux binary. There is
// deliberately no downgrade path: install a binary that supports the recorded
// version or restore a compatible pre-migration backup.
var ErrSchemaTooNew = errors.New("centralstore: database schema is newer than this binary")

// SchemaTooNewError carries both sides of an incompatible schema comparison.
type SchemaTooNewError struct {
	DatabaseVersion int64
	EmbeddedVersion int64
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("%v: database version %d, embedded head %d; install a newer gmux binary or restore a compatible pre-migration backup", ErrSchemaTooNew, e.DatabaseVersion, e.EmbeddedVersion)
}

func (e *SchemaTooNewError) Unwrap() error { return ErrSchemaTooNew }

// ErrForeignKeyIntegrity marks rows that violate declared foreign keys.
var ErrForeignKeyIntegrity = errors.New("centralstore: foreign key check failed")

// Verify opens the database read-only and checks its integrity without
// touching the writer's world: `PRAGMA quick_check` plus a schema-version
// sanity read. It is the bootstrap phase-1a gate — it must be safe to run
// while an incumbent daemon is still committing (WAL, one writer): a
// read-only connection never migrates, never journals, and busy_timeout
// bounds lock waits.
//
// A verification failure means the caller must not proceed to takeover: the
// incumbent (if any) keeps serving and the files are left untouched for
// diagnosis.
func Verify(ctx context.Context, dir string) error {
	if dir == "" {
		return errors.New("centralstore: empty state directory")
	}
	path := DatabasePath(dir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrDatabaseMissing, path)
	} else if err != nil {
		return fmt.Errorf("centralstore: stat database: %w", err)
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("centralstore: absolute database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?mode=ro&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("centralstore: open database read-only: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("centralstore: connect read-only: %w", err)
	}
	if err := quickCheck(ctx, database); err != nil {
		return err
	}
	// Schema-version sanity read: the migration bookkeeping must be present
	// and readable. A DB without it was never migrated by this daemon and is
	// not ours to serve.
	version, err := databaseVersion(ctx, database)
	if err != nil {
		return err
	}
	if version < 1 {
		return errors.New("centralstore: database carries no applied migrations")
	}
	head, err := EmbeddedSchemaVersion()
	if err != nil {
		return err
	}
	return ensureSchemaCompatible(version, head)
}

func databaseVersionReadOnly(ctx context.Context, path string) (int64, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("centralstore: absolute database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?mode=ro&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, fmt.Errorf("centralstore: open database schema preflight: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	return databaseVersion(ctx, database)
}

func databaseVersion(ctx context.Context, database *sql.DB) (int64, error) {
	var exists int
	if err := database.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", gooseVersionTable).Scan(&exists); err != nil {
		return 0, fmt.Errorf("centralstore: schema version preflight: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}
	// Goose's current-version semantics inspect history newest-first: only the
	// newest row for each version counts, and a latest is_applied=0 row marks
	// that version rolled back. Return the first still-applied version in that
	// history, rather than MAX(version_id), which includes rolled-back mentions.
	var version int64
	query := `SELECT COALESCE((
		SELECT history.version_id
		FROM ` + gooseVersionTable + ` AS history
		WHERE history.is_applied = 1
		  AND NOT EXISTS (
			SELECT 1 FROM ` + gooseVersionTable + ` AS newer
			WHERE newer.version_id = history.version_id AND newer.id > history.id
		  )
		ORDER BY history.id DESC
		LIMIT 1
	), 0)`
	if err := database.QueryRowContext(ctx, query).Scan(&version); err != nil {
		return 0, fmt.Errorf("centralstore: schema version read: %w", err)
	}
	return version, nil
}

func ensureSchemaCompatible(version, head int64) error {
	if version > head {
		return &SchemaTooNewError{DatabaseVersion: version, EmbeddedVersion: head}
	}
	return nil
}

// quickCheck runs `PRAGMA quick_check` and fails unless it reports exactly
// "ok" (ADR 0026 §10: open, integrity, or migration failure stops startup).
func quickCheck(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("centralstore: quick_check: %w", err)
	}
	defer rows.Close()
	var findings []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("centralstore: quick_check scan: %w", err)
		}
		findings = append(findings, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("centralstore: quick_check: %w", err)
	}
	if len(findings) == 1 && findings[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrIntegrity, strings.Join(findings, "; "))
}

func foreignKeyCheck(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("centralstore: foreign_key_check: %w", err)
	}
	defer rows.Close()
	var findings []string
	for rows.Next() {
		var table, parent string
		var rowID, fkID sql.NullInt64
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return fmt.Errorf("centralstore: foreign_key_check scan: %w", err)
		}
		findings = append(findings, fmt.Sprintf("table %q row %d references missing parent %q (fk %d)", table, rowID.Int64, parent, fkID.Int64))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("centralstore: foreign_key_check: %w", err)
	}
	if len(findings) != 0 {
		return fmt.Errorf("%w: %s", ErrForeignKeyIntegrity, strings.Join(findings, "; "))
	}
	return nil
}
