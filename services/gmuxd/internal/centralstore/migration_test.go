package centralstore

// Tests for the durable exit-invariant migration (00005). These tests exercise
// the SQL migration directly — upgrade remediation of pre-existing violations
// and the trigger-level rejection of new violations — which cannot be observed
// through the public API because Go-level validation fires first.

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// openRawDB opens (or creates) a SQLite database file at path with the same
// pragmas used by Open, but without running any migrations.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String() +
		"?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// migrateUpTo applies goose migrations from the embedded FS up to (and
// including) targetVersion.
func migrateUpTo(t *testing.T, database *sql.DB, targetVersion int64) {
	t.Helper()
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), targetVersion); err != nil {
		t.Fatalf("UpTo(%d): %v", targetVersion, err)
	}
}

// minimalSessionInsert returns an INSERT statement for local_sessions that sets
// only the NOT NULL columns, plus exit_code and exited_at_ms as specified.
func minimalSessionInsert(id string, exitCode, exitedAtMS string) string {
	return `INSERT INTO local_sessions
		(id, adapter, command_json, cwd, remotes_json, created_at_ms, exit_code, exited_at_ms)
		VALUES ('` + id + `', 'shell', '[]', '/', '{}', 1000, ` + exitCode + `, ` + exitedAtMS + `)`
}

// TestMigration005_CleanupAndTriggerEnforcement is the primary non-vacuous
// test for migration 00005. It:
//
//  1. Creates a v4 database and inserts a row that violates the exit invariant
//     (exit_code IS NOT NULL, exited_at_ms IS NULL) — this is valid pre-v5.
//  2. Applies migration v5 and verifies the remediation UPDATE cleared exit_code.
//  3. Verifies the insert trigger rejects a new bad INSERT.
//  4. Verifies the update trigger rejects a bad UPDATE on an existing row.
//  5. Verifies that compliant rows (both fields set, or both clear) are accepted.
func TestMigration005_CleanupAndTriggerEnforcement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	if f, err := os.Create(dbPath); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}

	database := openRawDB(t, dbPath)

	// Step 1: migrate to v4 and insert a violating row.
	migrateUpTo(t, database, 4)

	if _, err := database.Exec(minimalSessionInsert("bad-row", "42", "NULL")); err != nil {
		t.Fatalf("v4 pre-condition: insert violating row: %v", err)
	}
	// Sanity: the row really is there with the wrong state before migration.
	var code sql.NullInt64
	if err := database.QueryRow(`SELECT exit_code FROM local_sessions WHERE id = 'bad-row'`).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if !code.Valid || code.Int64 != 42 {
		t.Fatalf("pre-migration: exit_code = %v, want 42", code)
	}

	// Step 2: apply migration v5 — remediation must clear exit_code.
	migrateUpTo(t, database, 5)

	if err := database.QueryRow(`SELECT exit_code FROM local_sessions WHERE id = 'bad-row'`).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code.Valid {
		t.Fatalf("post-migration: exit_code = %v, want NULL (cleanup failed)", code.Int64)
	}

	// Step 3: insert trigger — bad INSERT must be rejected.
	_, err := database.Exec(minimalSessionInsert("trigger-insert-reject", "1", "NULL"))
	if err == nil {
		t.Fatal("insert trigger: expected ABORT, got nil")
	}
	if !strings.Contains(err.Error(), "exit_code requires exited_at_ms") {
		t.Fatalf("insert trigger: unexpected error: %v", err)
	}

	// Step 4: update trigger — bad UPDATE on an existing clean row must be rejected.
	if _, err := database.Exec(minimalSessionInsert("clean-row", "NULL", "NULL")); err != nil {
		t.Fatalf("insert clean row: %v", err)
	}
	_, err = database.Exec(`UPDATE local_sessions SET exit_code = 7, exited_at_ms = NULL WHERE id = 'clean-row'`)
	if err == nil {
		t.Fatal("update trigger: expected ABORT, got nil")
	}
	if !strings.Contains(err.Error(), "exit_code requires exited_at_ms") {
		t.Fatalf("update trigger: unexpected error: %v", err)
	}

	// Step 5a: valid INSERT — both fields set is accepted.
	if _, err := database.Exec(minimalSessionInsert("valid-row", "0", "2000")); err != nil {
		t.Fatalf("valid insert (both set): %v", err)
	}

	// Step 5b: valid INSERT — both fields NULL is accepted.
	if _, err := database.Exec(minimalSessionInsert("null-row", "NULL", "NULL")); err != nil {
		t.Fatalf("valid insert (both null): %v", err)
	}

	// Step 5c: valid UPDATE — setting both fields together is accepted.
	if _, err := database.Exec(`UPDATE local_sessions SET exit_code = 1, exited_at_ms = 3000 WHERE id = 'null-row'`); err != nil {
		t.Fatalf("valid update (both set): %v", err)
	}

	// Step 5d: valid UPDATE — clearing exit_code while exited_at_ms remains NULL is accepted.
	if _, err := database.Exec(`UPDATE local_sessions SET exit_code = NULL WHERE id = 'clean-row'`); err != nil {
		t.Fatalf("valid update (clear exit_code): %v", err)
	}
}
