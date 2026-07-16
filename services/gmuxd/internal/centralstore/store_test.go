package centralstore

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
	_ "modernc.org/sqlite"
)

func TestOpenFreshAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")

	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := store.database.Stats().MaxOpenConnections, 1; got != want {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, want)
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != 3 {
		t.Fatalf("schema version = %d, %v; want 3", version, err)
	}
	if err := store.queries.PutMetadata(ctx, db.PutMetadataParams{Key: "kept", Value: "yes"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if value, err := store.queries.GetMetadata(ctx, "kept"); err != nil || value != "yes" {
		t.Fatalf("reopened value = %q, %v; want yes", value, err)
	}
}

func TestOpenUsesOwnerOnlyModes(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "new", "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Perform a write so the WAL and SHM files are materialized; their modes
	// are then asserted unconditionally.
	if err := store.queries.PutMetadata(ctx, db.PutMetadataParams{Key: "wal", Value: "force"}); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, dir, 0o700)
	assertPerm(t, filepath.Join(dir, databaseName), 0o600)
	assertPerm(t, filepath.Join(dir, databaseName+"-wal"), 0o600)
	assertPerm(t, filepath.Join(dir, databaseName+"-shm"), 0o600)
}

func TestOpenTightensLooseExistingModes(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A restored backup or a crash between driver file creation and chmod
	// leaves a loose-mode database; every open must repair it.
	if err := os.WriteFile(filepath.Join(dir, databaseName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertPerm(t, dir, 0o700)
	assertPerm(t, filepath.Join(dir, databaseName), 0o600)
}

func assertPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestConfiguredConnectionSettingsAndForeignKeys(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	conn, err := store.database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var foreignKeys, busyTimeout int
	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || journalMode != "wal" || busyTimeout != 5000 {
		t.Fatalf("configured pragmas: foreign_keys=%d journal_mode=%q busy_timeout=%d", foreignKeys, journalMode, busyTimeout)
	}

	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE fk_parent (id INTEGER PRIMARY KEY);
CREATE TEMP TABLE fk_child (parent_id INTEGER REFERENCES fk_parent(id));`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO fk_child (parent_id) VALUES (99)"); err == nil {
		t.Fatal("orphan insert succeeded; foreign keys are not enforced")
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	database, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	files := fstest.MapFS{"00001_broken.sql": {Data: []byte(`-- +goose Up
CREATE TABLE should_rollback (id INTEGER PRIMARY KEY);
INSERT INTO table_that_does_not_exist VALUES (1);
-- +goose Down
DROP TABLE should_rollback;
`)}}
	if err := migrate(context.Background(), database, files); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var count int
	if err := database.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'should_rollback'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left its table behind")
	}
}

func TestTransactionUsesBoundQueriesWithoutSecondConnection(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	qtx := store.queries.WithTx(tx)
	if err := qtx.PutMetadata(ctx, db.PutMetadataParams{Key: "tx", Value: "bound"}); err != nil {
		t.Fatal(err)
	}
	if value, err := qtx.GetMetadata(ctx, "tx"); err != nil || value != "bound" {
		t.Fatalf("value = %q, %v", value, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentUseWithOneConnection(t *testing.T) {
	store := openTestStore(t)
	const workers = 32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := store.database.BeginTx(ctx, nil)
			if err != nil {
				errs <- err
				return
			}
			qtx := store.queries.WithTx(tx)
			if err := qtx.PutMetadata(ctx, db.PutMetadataParams{Key: "shared", Value: "valid"}); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			if _, err := qtx.GetMetadata(ctx, "shared"); err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
