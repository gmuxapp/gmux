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
	var version int64
	if err := database.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version_id), 0) FROM "+gooseVersionTable).Scan(&version); err != nil {
		return fmt.Errorf("centralstore: schema version read: %w", err)
	}
	if version < 1 {
		return errors.New("centralstore: database carries no applied migrations")
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
