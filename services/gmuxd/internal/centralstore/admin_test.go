package centralstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openAdminStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func exec(t *testing.T, s *Store, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := s.database.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func findingCodes(f []CheckFinding) []string {
	out := make([]string, len(f))
	for i, v := range f {
		out[i] = v.Code
	}
	return out
}

func requireFinding(t *testing.T, findings []CheckFinding, code string) {
	t.Helper()
	for _, f := range findings {
		if f.Code == code {
			return
		}
	}
	t.Fatalf("expected finding %q, got %v", code, findings)
}

func checkFindings(t *testing.T, s *Store) []CheckFinding {
	t.Helper()
	findings, err := s.CheckState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func TestCheckStateCleanDatabase(t *testing.T) {
	s, _ := openAdminStore(t)
	p := addProject(t, s)
	addSession(t, s, "root", "")
	addSession(t, s, "child", "root")
	ctx := context.Background()
	for _, id := range []string{"root", "child"} {
		if _, err := s.PlaceLocalSession(ctx, SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "laptop", URL: "https://laptop:7369", Token: "sec"}, 5); err != nil {
		t.Fatal(err)
	}
	if f := checkFindings(t, s); len(f) != 0 {
		t.Fatalf("clean database reported findings: %v", f)
	}
}

func TestEmbeddedSchemaVersionMatchesOpenedDatabase(t *testing.T) {
	s, _ := openAdminStore(t)
	head, err := EmbeddedSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if head != got || head < 4 {
		t.Fatalf("embedded head %d vs opened %d", head, got)
	}
}

func TestCheckStateSchemaVersionMismatch(t *testing.T) {
	s, _ := openAdminStore(t)
	exec(t, s, "DELETE FROM goose_db_version WHERE version_id = (SELECT MAX(version_id) FROM goose_db_version)")
	requireFinding(t, checkFindings(t, s), "schema_version")
}

func TestCheckStateDenseScopeProducesNoFinding(t *testing.T) {
	s, _ := openAdminStore(t)
	p := addProject(t, s)
	addSession(t, s, "a", "")
	addSession(t, s, "b", "")
	for _, id := range []string{"a", "b"} {
		if _, err := s.PlaceLocalSession(context.Background(), SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range checkFindings(t, s) {
		if f.Code == "scope_not_dense" {
			t.Fatalf("dense 0,1 scope produced finding: %v", f)
		}
	}
}

func TestCheckStateScopeDensityAndMismatch(t *testing.T) {
	s, _ := openAdminStore(t)
	p := addProject(t, s)
	addSession(t, s, "a", "")
	addSession(t, s, "b", "")
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := s.PlaceLocalSession(ctx, SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	// Position gap (0,1 -> 0,5) violates density; a wrong stored scope
	// diverges from the desiredScope recomputation.
	exec(t, s,
		"UPDATE project_placements SET position = 5 WHERE local_session_id = 'b'",
		"UPDATE project_placements SET sibling_scope = 'c:l:ghost', position = 0 WHERE local_session_id = 'a'",
	)
	findings := checkFindings(t, s)
	requireFinding(t, findings, "scope_not_dense")
	requireFinding(t, findings, "scope_mismatch")
}

func TestCheckStateDismissalAndExitContracts(t *testing.T) {
	s, _ := openAdminStore(t)
	p := addProject(t, s)
	addSession(t, s, "a", "")
	if _, err := s.PlaceLocalSession(context.Background(), "a", p); err != nil {
		t.Fatal(err)
	}
	// A dismissed row must not hold a placement; an exit code requires an
	// exit stamp. Manufacture bad state by temporarily dropping the exit
	// invariant trigger (a corrupt on-disk database has no such guarantee).
	exec(t, s,
		"UPDATE local_sessions SET dismissed_at_ms = 10 WHERE id = 'a'",
		"DROP TRIGGER local_sessions_exit_invariant_update",
		"UPDATE local_sessions SET exit_code = 1, exited_at_ms = NULL WHERE id = 'a'",
		"CREATE TRIGGER local_sessions_exit_invariant_update BEFORE UPDATE ON local_sessions WHEN NEW.exit_code IS NOT NULL AND NEW.exited_at_ms IS NULL BEGIN SELECT RAISE(ABORT, 'exit_code requires exited_at_ms'); END",
	)
	findings := checkFindings(t, s)
	requireFinding(t, findings, "dismissed_placement")
	requireFinding(t, findings, "exit_contract")
}

func TestCheckStateLaunchCycle(t *testing.T) {
	s, _ := openAdminStore(t)
	addSession(t, s, "a", "")
	addSession(t, s, "b", "a")
	// The immutability trigger blocks rewrites at runtime; a corrupt
	// database has no such guarantee, so drop it and manufacture a cycle.
	exec(t, s,
		"DROP TRIGGER local_sessions_launch_parent_immutable_update",
		"UPDATE local_sessions SET launch_parent_id = 'b' WHERE id = 'a'",
	)
	requireFinding(t, checkFindings(t, s), "launch_cycle")
}

func TestCheckStateAdapterEmpty(t *testing.T) {
	s, _ := openAdminStore(t)
	addSession(t, s, "a", "")
	exec(t, s,
		"PRAGMA ignore_check_constraints = ON",
		"UPDATE local_sessions SET adapter = '' WHERE id = 'a'",
		"PRAGMA ignore_check_constraints = OFF",
	)
	requireFinding(t, checkFindings(t, s), "adapter_empty")
}

func TestCheckStateForeignKeyViolation(t *testing.T) {
	s, _ := openAdminStore(t)
	p := addProject(t, s)
	exec(t, s,
		"PRAGMA foreign_keys = OFF",
		"INSERT INTO project_placements (project_entry_id, local_session_id, sibling_scope, position) VALUES ("+
			itoa(int64(p))+", 'ghost', 'r', 0)",
		"PRAGMA foreign_keys = ON",
	)
	requireFinding(t, checkFindings(t, s), "foreign_key")
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func TestCheckStateCatalogInvariants(t *testing.T) {
	s, _ := openAdminStore(t)
	ctx := context.Background()
	cat, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{
		owned("a", "/a"),
		{Reference: &ProjectReference{PeerKey: "peer", Slug: "ref"}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	refID := itoa(int64(cat[1].ID))
	exec(t, s,
		// Sidebar order gap.
		"UPDATE project_entries SET sidebar_order = 7 WHERE sidebar_order = 1",
		// Rules and placements on a reference entry (triggers dropped to
		// simulate corruption); FK-off permits the ghost local arm.
		"DROP TRIGGER project_match_rules_owned_insert",
		"INSERT INTO project_match_rules (project_entry_id, rule_order, path) VALUES ("+refID+", 0, '/x')",
		"DROP TRIGGER project_placements_owned_insert",
		"PRAGMA foreign_keys = OFF",
		"INSERT INTO project_placements (project_entry_id, local_session_id, sibling_scope, position) VALUES ("+refID+", 'ghost', 'r', 0)",
		"PRAGMA foreign_keys = ON",
		// Non-dense rule order on the owned entry.
		"UPDATE project_match_rules SET rule_order = 3 WHERE path = '/a'",
	)
	findings := checkFindings(t, s)
	requireFinding(t, findings, "catalog_order")
	requireFinding(t, findings, "non_owned_rules")
	requireFinding(t, findings, "non_owned_placement")
	if len(findingCodes(findings)) < 4 {
		t.Fatalf("expected at least three findings, got %v", findings)
	}
}

func TestCheckStateIntegrityFailureOnCorruptPages(t *testing.T) {
	s, dir := openAdminStore(t)
	addSession(t, s, "a", "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt(t, dir)
	ro, err := OpenReadOnly(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	findings, err := ro.CheckState(context.Background())
	if err == nil {
		requireFinding(t, findings, "integrity")
		return
	}
	// Damaged pages may also surface as an operational error from a later
	// query; either way the corruption never passes silently.
}

func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenReadOnly(ctx, filepath.Join(t.TempDir(), "absent")); !errors.Is(err, ErrDatabaseMissing) {
		t.Fatalf("expected ErrDatabaseMissing, got %v", err)
	}

	s, dir := openAdminStore(t)
	addSession(t, s, "a", "")
	ro, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, err := ro.ListSessions(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("read-only list = %v, %v", rows, err)
	}
	if _, _, err := ro.InsertSession(ctx, NewSession{ID: "b", Adapter: "shell", CWD: "/", CreatedAt: 1}); err == nil {
		t.Fatal("expected mutation on a read-only handle to fail")
	}
}

func TestBackupInto(t *testing.T) {
	ctx := context.Background()
	s, _ := openAdminStore(t)
	addSession(t, s, "a", "")
	if _, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "laptop", URL: "https://laptop:7369", Token: "sec"}, 5); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "nested", "backup.db")
	if err := s.BackupInto(ctx, target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("created backup dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}

	// Never overwrite.
	if err := s.BackupInto(ctx, target); !errors.Is(err, ErrBackupTargetExists) {
		t.Fatalf("expected ErrBackupTargetExists, got %v", err)
	}

	// The backup is a full, openable database: rename it into a state dir
	// layout and read it back (tokens included — a raw backup is the
	// secret-bearing artifact).
	restore := filepath.Join(t.TempDir(), "restore")
	if err := os.MkdirAll(restore, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, filepath.Join(restore, "state.db")); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, restore)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, err := ro.ListSessions(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("restored sessions = %v, %v", rows, err)
	}
	peers, err := ro.ListManualPeers(ctx)
	if err != nil || len(peers) != 1 || peers[0].Token != "sec" {
		t.Fatalf("restored peers = %v, %v", peers, err)
	}
	if f := checkFindings(t, ro); len(f) != 0 {
		t.Fatalf("restored backup reported findings: %v", f)
	}
}

func TestBackupIntoTargetAppearsRaceIsTyped(t *testing.T) {
	s, _ := openAdminStore(t)
	target := filepath.Join(t.TempDir(), "backup.db")
	s.beforeBackupLink = func() {
		if err := os.WriteFile(target, []byte("winner"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BackupInto(context.Background(), target); !errors.Is(err, ErrBackupTargetExists) {
		t.Fatalf("target-appeared race = %v, want ErrBackupTargetExists", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "winner" {
		t.Fatalf("existing target changed: %q, %v", got, err)
	}
}

func TestBackupIntoPreservesPreexistingParentMode(t *testing.T) {
	s, _ := openAdminStore(t)
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "backup.db")
	if err := s.BackupInto(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	parentInfo, _ := os.Stat(parent)
	fileInfo, _ := os.Stat(target)
	if parentInfo.Mode().Perm() != 0o755 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("parent/file modes = %o/%o, want 755/600", parentInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestBackupIntoFromReadOnlyHandle(t *testing.T) {
	// The offline backup path runs VACUUM INTO from a read-only open
	// (design §5); prove the read-only handle supports it.
	ctx := context.Background()
	s, dir := openAdminStore(t)
	addSession(t, s, "a", "")
	ro, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	target := filepath.Join(t.TempDir(), "offline.db")
	if err := ro.BackupInto(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyDatabaseCheckAndExport(t *testing.T) {
	s, _ := openAdminStore(t)
	if f := checkFindings(t, s); len(f) != 0 {
		t.Fatalf("empty database findings = %v", f)
	}
	out, err := s.ExportState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 0 || len(out.Catalog) != 0 || len(out.Placements) != 0 || len(out.Peers) != 0 {
		t.Fatalf("empty export = %+v", out)
	}
}

func TestExportState(t *testing.T) {
	ctx := context.Background()
	s, _ := openAdminStore(t)
	p := addProject(t, s)
	addSession(t, s, "b", "")
	addSession(t, s, "a", "")
	addSession(t, s, "gone", "")
	for _, id := range []string{"a", "b"} {
		if _, err := s.PlaceLocalSession(ctx, SessionID(id), p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpsertLocalPeerPlacement(ctx, LocalPeerSubject{
		PeerKey: "peer-a", SessionID: "remote-child", ParentSessionID: "remote-parent",
	}, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DismissSessionTree(ctx, "gone", 50); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.UpsertManualPeer(ctx, ManualPeerSpec{Name: "laptop", URL: "https://user:pass@laptop:7369", Token: "sec", NodeID: "node-1"}, 5); err != nil {
		t.Fatal(err)
	}

	out, err := s.ExportState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := EmbeddedSchemaVersion()
	if out.SchemaVersion != head {
		t.Fatalf("schema version = %d, want %d", out.SchemaVersion, head)
	}
	// All sessions, dismissed included, sorted by ID.
	if len(out.Sessions) != 3 || out.Sessions[0].ID != "a" || out.Sessions[1].ID != "b" || out.Sessions[2].ID != "gone" {
		t.Fatalf("sessions = %v", out.Sessions)
	}
	if out.Sessions[2].DismissedAt == nil {
		t.Fatal("dismissed session lost its dismissal stamp in export")
	}
	if len(out.Catalog) != 1 || out.Catalog[0].Slug != "p" {
		t.Fatalf("catalog = %v", out.Catalog)
	}
	// The dismissed session holds no placement; the two placed rows ride
	// in scope/position order with the project slug joined.
	if len(out.Placements) != 3 || out.Placements[0].LocalSessionID != "a" || out.Placements[1].LocalSessionID != "b" ||
		out.Placements[0].ProjectSlug != "p" || out.Placements[0].Position != 0 || out.Placements[1].Position != 1 {
		t.Fatalf("placements = %v", out.Placements)
	}
	peerPlacement := out.Placements[2]
	if peerPlacement.LocalPeerKey != "peer-a" || peerPlacement.PeerSessionID != "remote-child" ||
		peerPlacement.PeerParentSessionID != "remote-parent" || peerPlacement.LocalSessionID != "" {
		t.Fatalf("Local-peer placement = %+v", peerPlacement)
	}
	// Peers are pre-redacted: presence only, never the token value.
	if len(out.Peers) != 1 || out.Peers[0].Name != "laptop" || !out.Peers[0].TokenPresent || out.Peers[0].NodeID != "node-1" {
		t.Fatalf("peers = %v", out.Peers)
	}
}
