package centralstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func TestReleasedV1MigrationChecksum(t *testing.T) {
	data, err := fs.ReadFile(migrationFiles, "migrations/00001_initial_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	const want = "89d09ac52c85225a3039b3eceda4601d10ddf25584c24f3f2a65fbb725849396"
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
		t.Fatalf("released v1 migration changed: sha256=%s, want %s; add a new migration instead of editing v1", got, want)
	}
}

func TestV2MigrationChecksum(t *testing.T) {
	data, err := fs.ReadFile(migrationFiles, "migrations/00002_drive_mode.sql")
	if err != nil {
		t.Fatal(err)
	}
	const want = "c004c3ea7098fd7348151f0eca97ecac8736dbd575f625e13ce7533bb6faa6e5"
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
		t.Fatalf("v2 migration changed: sha256=%s, want %s; migrations are immutable once merged — add a new one instead", got, want)
	}
}

func TestNewerSchemaRefusedWithoutMutation(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.database.ExecContext(ctx,
		"INSERT INTO centralstore_metadata (key, value) VALUES ('future-guard', 'retained')"); err != nil {
		t.Fatal(err)
	}
	head, err := EmbeddedSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.database.ExecContext(ctx,
		"INSERT INTO "+gooseVersionTable+" (version_id, is_applied) VALUES (?, 1)", head+1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	path := DatabasePath(dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertTooNew := func(label string, err error) {
		t.Helper()
		if !errors.Is(err, ErrSchemaTooNew) {
			t.Fatalf("%s error = %v, want ErrSchemaTooNew", label, err)
		}
		var detail *SchemaTooNewError
		if !errors.As(err, &detail) || detail.DatabaseVersion != head+1 || detail.EmbeddedVersion != head {
			t.Fatalf("%s detail = %#v, want database=%d embedded=%d", label, detail, head+1, head)
		}
	}
	assertTooNew("Verify", Verify(ctx, dir))
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("Open accepted a newer schema")
	} else {
		assertTooNew("Open", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertPerm(t, dir, 0o700)
	if string(after) != string(before) {
		t.Fatal("Verify or Open changed database bytes while refusing a newer schema")
	}

	raw := openReleaseTestDB(t, path)
	defer raw.Close()
	var value string
	if err := raw.QueryRowContext(ctx, "SELECT value FROM centralstore_metadata WHERE key='future-guard'").Scan(&value); err != nil || value != "retained" {
		t.Fatalf("domain value = %q, %v; want retained", value, err)
	}
}

func TestDatabaseVersionMatchesGooseAppliedHistory(t *testing.T) {
	ctx := context.Background()
	head, err := EmbeddedSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		history   []bool
		wantAhead bool
	}{
		{name: "ahead applied", history: []bool{true}, wantAhead: true},
		{name: "ahead applied then rolled back", history: []bool{true, false}},
		{name: "ahead repeatedly applied and rolled back", history: []bool{true, false, true}, wantAhead: true},
		{name: "ahead repeated rollback remains unapplied", history: []bool{true, false, true, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			store, err := Open(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, applied := range tt.history {
				if _, err := store.database.ExecContext(ctx,
					"INSERT INTO "+gooseVersionTable+" (version_id, is_applied) VALUES (?, ?)", head+1, applied); err != nil {
					t.Fatal(err)
				}
			}
			got, err := databaseVersion(ctx, store.database)
			if err != nil {
				t.Fatal(err)
			}
			gooseVersion, err := goose.GetDBVersionContext(ctx, store.database)
			if err != nil {
				t.Fatal(err)
			}
			if got != gooseVersion {
				t.Fatalf("databaseVersion = %d, Goose current version = %d", got, gooseVersion)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			verifyErr := Verify(ctx, dir)
			reopened, openErr := Open(ctx, dir)
			if reopened != nil {
				defer reopened.Close()
			}
			if tt.wantAhead {
				if !errors.Is(verifyErr, ErrSchemaTooNew) || !errors.Is(openErr, ErrSchemaTooNew) {
					t.Fatalf("Verify/Open errors = %v / %v, want ErrSchemaTooNew", verifyErr, openErr)
				}
			} else {
				if errors.Is(verifyErr, ErrSchemaTooNew) || errors.Is(openErr, ErrSchemaTooNew) {
					t.Fatalf("rolled-back ahead history misclassified: Verify/Open = %v / %v", verifyErr, openErr)
				}
				if verifyErr != nil || openErr != nil {
					t.Fatalf("Verify/Open = %v / %v, want success", verifyErr, openErr)
				}
			}
		})
	}
}

func TestSyntheticV1ToV2UpgradeBackupRetentionAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	database := createReleasedV1Database(t, dir)
	defer func() { _ = database.Close() }()
	seedReleasedV1Data(t, database)

	files := releaseTestMigrations(t, map[string]string{
		"00002_upgrade.sql": `-- +goose Up
ALTER TABLE local_sessions ADD COLUMN upgrade_marker TEXT NOT NULL DEFAULT 'from-v1';
CREATE TABLE v2_upgrade_marker (value TEXT NOT NULL) STRICT;
INSERT INTO v2_upgrade_marker VALUES ('applied');
-- +goose Down
DROP TABLE v2_upgrade_marker;
`,
	})
	if _, err := migrateWithSafety(ctx, database, files, migrationSafety{true, 1, 2, dir}); err != nil {
		t.Fatal(err)
	}
	assertReleasedDataRetained(t, database)
	assertDBVersion(t, database, 2)
	if err := quickCheck(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := foreignKeyCheck(ctx, database); err != nil {
		t.Fatal(err)
	}

	backups, err := filepath.Glob(filepath.Join(dir, "backups", "state-pre-migration-v1-to-v2-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, %v; want one", backups, err)
	}
	assertPerm(t, backups[0], 0o600)
	backupDB := openReleaseTestDB(t, backups[0])
	assertDBVersion(t, backupDB, 1)
	assertReleasedDataRetained(t, backupDB)
	if err := backupDB.Close(); err != nil {
		t.Fatal(err)
	}

	// Really close and reopen the upgraded database before proving retained
	// relationships and idempotence at the new head.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openReleaseTestDB(t, DatabasePath(dir))
	assertReleasedDataRetained(t, database)
	if _, err := migrateWithSafety(ctx, database, files, migrationSafety{true, 2, 2, dir}); err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(filepath.Join(dir, "backups", "*.db"))
	if len(after) != 1 {
		t.Fatalf("idempotent reopen created backups: %v", after)
	}
}

func TestV3MigrationBackfillsLaunchProvenance(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	database := createReleasedV1Database(t, dir)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO local_sessions
		(id, adapter, command_json, cwd, remotes_json, created_at_ms, launch_parent_id)
		VALUES ('child', 'shell', '["sh"]', '/', '{}', 2, 'parent')`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var parent, launchedFrom sql.NullString
	if err := store.database.QueryRowContext(ctx, `
		SELECT parent_session_id, launched_from_session_id
		FROM local_sessions WHERE id='child'`).Scan(&parent, &launchedFrom); err != nil {
		t.Fatal(err)
	}
	if !parent.Valid || parent.String != "parent" || !launchedFrom.Valid || launchedFrom.String != "parent" {
		t.Fatalf("migrated parent/provenance = %#v / %#v", parent, launchedFrom)
	}
}

func TestMigrationBackupFailureRefusesUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	database := createReleasedV1Database(t, dir)
	defer database.Close()
	if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := releaseTestMigrations(t, map[string]string{"00002_never.sql": "-- +goose Up\nCREATE TABLE must_not_exist (id INTEGER);\n"})
	_, err := migrateWithSafety(ctx, database, files, migrationSafety{true, 1, 2, dir})
	if err == nil || !strings.Contains(err.Error(), "pre-migration backup") {
		t.Fatalf("error = %v, want backup refusal", err)
	}
	assertDBVersion(t, database, 1)
	assertTableExists(t, database, "must_not_exist", false)
}

func TestAutomaticBackupDirectoryIsPrivateAndNotSymlinked(t *testing.T) {
	ctx := context.Background()
	files := releaseTestMigrations(t, map[string]string{
		"00002_upgrade.sql": "-- +goose Up\nCREATE TABLE backup_dir_probe (id INTEGER);\n",
	})

	t.Run("tightens existing directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		database := createReleasedV1Database(t, dir)
		defer database.Close()
		backupDir := filepath.Join(dir, "backups")
		if err := os.Mkdir(backupDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(backupDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := migrateWithSafety(ctx, database, files, migrationSafety{true, 1, 2, dir}); err != nil {
			t.Fatal(err)
		}
		assertPerm(t, backupDir, 0o700)
	})

	t.Run("refuses symlink before migration", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		database := createReleasedV1Database(t, dir)
		defer database.Close()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "backups")); err != nil {
			t.Fatal(err)
		}
		_, err := migrateWithSafety(ctx, database, files, migrationSafety{true, 1, 2, dir})
		if err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("error = %v, want symlink refusal", err)
		}
		assertDBVersion(t, database, 1)
		assertTableExists(t, database, "backup_dir_probe", false)
		entries, readErr := os.ReadDir(outside)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("outside backup directory = %v, %v; want untouched", entries, readErr)
		}
	})
}

func TestPendingMigrationsAreAtomicPerMigrationAndRetainBackupOnFailure(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	database := createReleasedV1Database(t, dir)
	defer database.Close()
	seedReleasedV1Data(t, database)
	files := releaseTestMigrations(t, map[string]string{
		"00002_success.sql": "-- +goose Up\nCREATE TABLE migration_two (value TEXT);\nINSERT INTO migration_two VALUES ('committed');\n",
		"00003_failure.sql": "-- +goose Up\nCREATE TABLE migration_three (value TEXT);\nINSERT INTO missing_table VALUES ('fail');\n",
	})
	_, err := migrateWithSafety(ctx, database, files, migrationSafety{true, 1, 3, dir})
	if err == nil {
		t.Fatal("migration set unexpectedly succeeded")
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "state-pre-migration-v1-to-v3-*.db"))
	if len(backups) != 1 || !strings.Contains(err.Error(), backups[0]) {
		t.Fatalf("error/backups = %v / %v; want retained backup path in diagnostic", err, backups)
	}
	// Goose commits each migration separately: v2 remains, while the failing
	// v3 transaction leaves neither its schema nor its version row behind.
	assertDBVersion(t, database, 2)
	assertTableExists(t, database, "migration_two", true)
	assertTableExists(t, database, "migration_three", false)
	assertReleasedDataRetained(t, database)
	backupDB := openReleaseTestDB(t, backups[0])
	defer backupDB.Close()
	assertDBVersion(t, backupDB, 1)
}

func TestOpenRejectsCommittedMigrationForeignKeyViolation(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	v1, err := fs.ReadFile(migrationFiles, "migrations/00001_initial_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := fs.ReadFile(migrationFiles, "migrations/00002_drive_mode.sql")
	if err != nil {
		t.Fatal(err)
	}
	v3, err := fs.ReadFile(migrationFiles, "migrations/00003_session_parent_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{
		"migrations/00001_initial_schema.sql":            {Data: v1},
		"migrations/00002_drive_mode.sql":                {Data: v2},
		"migrations/00003_session_parent_provenance.sql": {Data: v3},
		"migrations/00004_unread_token.sql":              {Data: []byte("-- +goose Up\nALTER TABLE local_sessions ADD COLUMN unread_token TEXT NOT NULL DEFAULT '';\n")},
		"migrations/00006_orphan.sql":                    {Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nCREATE TABLE migration_parent (id INTEGER PRIMARY KEY);\nCREATE TABLE migration_child (parent_id INTEGER REFERENCES migration_parent(id));\nPRAGMA foreign_keys=OFF;\nINSERT INTO migration_child VALUES (99);\nPRAGMA foreign_keys=ON;\n")},
	}
	_, err = openWithMigrationFS(ctx, dir, files)
	if !errors.Is(err, ErrForeignKeyIntegrity) {
		t.Fatalf("open error = %v, want ErrForeignKeyIntegrity", err)
	}
	backups, globErr := filepath.Glob(filepath.Join(dir, "backups", "state-pre-migration-v5-to-v6-*.db"))
	if globErr != nil || len(backups) != 1 || !strings.Contains(err.Error(), backups[0]) {
		t.Fatalf("error/backups = %v / %v / %v; want retained path in post-migration diagnostic", err, backups, globErr)
	}
	database := openReleaseTestDB(t, DatabasePath(dir))
	defer database.Close()
	assertDBVersion(t, database, 6)
}

func TestQuickCheckFailureCarriesMigrationBackupPath(t *testing.T) {
	database, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	const backup = "/retained/pre-migration.db"
	err = postMigrationChecks(context.Background(), database, backup)
	if err == nil || !strings.Contains(err.Error(), backup) {
		t.Fatalf("post-migration quick-check error = %v, want retained backup path", err)
	}
}

func releaseTestMigrations(t *testing.T, extra map[string]string) fstest.MapFS {
	t.Helper()
	v1, err := fs.ReadFile(migrationFiles, "migrations/00001_initial_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"00001_initial_schema.sql": {Data: v1}}
	for name, sql := range extra {
		files[name] = &fstest.MapFile{Data: []byte(sql)}
	}
	return files
}

func createReleasedV1Database(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database := openReleaseTestDB(t, DatabasePath(dir))
	v1 := releaseTestMigrations(t, nil)
	if err := migrate(context.Background(), database, v1); err != nil {
		t.Fatal(err)
	}
	return database
}

func openReleaseTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	return database
}

func seedReleasedV1Data(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`INSERT INTO local_sessions (id, adapter, command_json, cwd, remotes_json, created_at_ms) VALUES ('session-v1', 'shell', '["sh"]', '/work/project', '{"origin":"https://example.test/repo"}', 10)`,
		`INSERT INTO project_entries (id, sidebar_order, entry_kind, slug, created_at_ms, updated_at_ms) VALUES (7, 0, 'owned', 'project-v1', 10, 10)`,
		`INSERT INTO project_placements (project_entry_id, local_session_id, sibling_scope, position) VALUES (7, 'session-v1', 'root', 0)`,
		`INSERT INTO manual_peers (name, url, token, node_id, created_at_ms, updated_at_ms) VALUES ('peer-v1', 'https://peer.test', 'secret-v1', 'node-v1', 10, 10)`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func assertReleasedDataRetained(t *testing.T, database *sql.DB) {
	t.Helper()
	var session, project, token string
	if err := database.QueryRow("SELECT id FROM local_sessions WHERE id='session-v1'").Scan(&session); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT slug FROM project_entries WHERE id=7").Scan(&project); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT token FROM manual_peers WHERE name='peer-v1'").Scan(&token); err != nil {
		t.Fatal(err)
	}
	var projectID int64
	var placementSession, scope string
	var position int64
	if err := database.QueryRow(`
		SELECT p.project_entry_id, p.local_session_id, p.sibling_scope, p.position
		FROM project_placements p
		JOIN project_entries e ON e.id = p.project_entry_id
		JOIN local_sessions s ON s.id = p.local_session_id
		WHERE e.id = 7 AND s.id = 'session-v1'`).Scan(&projectID, &placementSession, &scope, &position); err != nil {
		t.Fatal(err)
	}
	if session != "session-v1" || project != "project-v1" || token != "secret-v1" ||
		projectID != 7 || placementSession != "session-v1" || scope != "root" || position != 0 {
		t.Fatalf("retained data/placement = %q %q %q / %d %q %q %d", session, project, token, projectID, placementSession, scope, position)
	}
}

func assertDBVersion(t *testing.T, database *sql.DB, want int64) {
	t.Helper()
	got, err := databaseVersion(context.Background(), database)
	if err != nil || got != want {
		t.Fatalf("database version = %d, %v; want %d", got, err, want)
	}
}

func assertTableExists(t *testing.T, database *sql.DB, name string, want bool) {
	t.Helper()
	var count int
	if err := database.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if got := count != 0; got != want {
		t.Fatalf("table %q exists = %v, want %v", name, got, want)
	}
}
