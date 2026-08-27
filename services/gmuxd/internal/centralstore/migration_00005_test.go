package centralstore

import (
	"bytes"
	"context"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestMigration00005CollapsesPromotedRowsOntoParentEdge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	files := fstest.MapFS{}
	for _, name := range []string{
		"00001_initial_schema.sql", "00002_drive_mode.sql",
		"00003_session_parent_provenance.sql", "00004_unread_token.sql",
	} {
		data, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		files["migrations/"+name] = &fstest.MapFile{Data: data}
	}
	v4, err := openWithMigrationFS(ctx, dir, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, parent, launched string
		promoted             int
	}{
		{"parent", "", "", 0}, {"promoted", "parent", "parent", 1}, {"grandchild", "promoted", "promoted", 0},
	} {
		var parent, launched any
		if row.parent != "" {
			parent = row.parent
		}
		if row.launched != "" {
			launched = row.launched
		}
		_, err = v4.database.ExecContext(ctx, `INSERT INTO local_sessions
			(id, adapter, command_json, cwd, remotes_json, created_at_ms, parent_session_id, launched_from_session_id, promoted_to_root)
			VALUES (?, 'shell', '[]', '/', '{}', 1, ?, ?, ?)`, row.id, parent, launched, row.promoted)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = v4.database.ExecContext(ctx, `INSERT INTO project_entries(id, sidebar_order, entry_kind, slug, created_at_ms, updated_at_ms) VALUES (1,0,'owned','p',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = v4.database.ExecContext(ctx, `INSERT INTO project_placements(project_entry_id, local_session_id, sibling_scope, position) VALUES (1,'promoted','r',0),(1,'grandchild','c:l:promoted',0)`); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = v4.database.QueryRowContext(ctx, `SELECT CAST(group_concat(local_session_id||':'||sibling_scope||':'||position,'|') AS BLOB) FROM project_placements ORDER BY id`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = v4.Close(); err != nil {
		t.Fatal(err)
	}

	head, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = head.Close() })
	promoted := mustSession(t, head, "promoted")
	if promoted.ParentSessionID != nil || promoted.LaunchedFromSessionID == nil || *promoted.LaunchedFromSessionID != "parent" {
		t.Fatalf("promoted row = %#v", promoted)
	}
	grandchild := mustSession(t, head, "grandchild")
	if grandchild.ParentSessionID == nil || *grandchild.ParentSessionID != "promoted" {
		t.Fatalf("grandchild edge = %#v", grandchild.ParentSessionID)
	}
	var after []byte
	if err = head.database.QueryRowContext(ctx, `SELECT CAST(group_concat(local_session_id||':'||sibling_scope||':'||position,'|') AS BLOB) FROM project_placements ORDER BY id`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("placements changed: %q -> %q", before, after)
	}
	assertKernelInvariants(t, head)
}
