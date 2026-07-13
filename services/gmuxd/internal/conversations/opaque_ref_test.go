package conversations

import (
	"errors"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// dbAdapter is a minimal non-file adapter: its conversation refs are opaque
// row keys ("row:<id>"), not paths, and metadata comes from an in-memory
// "table". It exercises the ADR 0022 contract the index promises: a
// DB-backed adapter needs only ConversationDescriber (+ Resumer) — no
// directory layout, no stat, no path semantics anywhere in the daemon.
type dbAdapter struct {
	name string                              // adapter name; "dbtool" when empty
	rows map[string]adapter.ConversationInfo // ref → row
}

func (d *dbAdapter) Name() string {
	if d.name == "" {
		return "dbtool"
	}
	return d.name
}
func (d *dbAdapter) Discover() bool                    { return true }
func (d *dbAdapter) Match(_ []string) bool             { return false }
func (d *dbAdapter) Env(_ adapter.EnvContext) []string { return nil }

func (d *dbAdapter) DescribeConversation(ref string) (*adapter.ConversationInfo, error) {
	row, ok := d.rows[ref]
	if !ok {
		return nil, errors.New("no such row")
	}
	row.Ref = ref
	return &row, nil
}

func (d *dbAdapter) ResumeCommand(info *adapter.ConversationInfo) []string {
	return []string{"dbtool", "resume", info.ID}
}

func (d *dbAdapter) CanResume(ref string) bool {
	_, ok := d.rows[ref]
	return ok
}

var (
	_ adapter.ConversationDescriber = (*dbAdapter)(nil)
	_ adapter.Resumer               = (*dbAdapter)(nil)
)

// TestScan_OpaqueRefAdapter proves the index seam is ref-opaque end to end:
// a non-file adapter's row-key refs flow through Scan, land in the Info
// unmodified alongside adapter-provided freshness, resolve for URL lookup,
// and are removable by the same ref a source's Remove event would carry.
func TestScan_OpaqueRefAdapter(t *testing.T) {
	activity := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	a := &dbAdapter{rows: map[string]adapter.ConversationInfo{
		"row:42": {
			ID:           "conv-42",
			Title:        "fix the flaky test",
			Slug:         "fix-the-flaky-test",
			Cwd:          "/home/u/proj",
			Created:      activity.Add(-time.Hour),
			LastActivity: activity,
			MessageCount: 3,
		},
	}}

	idx := New()
	slug := idx.Scan(a, "row:42")
	if slug != "fix-the-flaky-test" {
		t.Fatalf("Scan slug = %q, want fix-the-flaky-test", slug)
	}

	info, ok := idx.Lookup("dbtool", slug)
	if !ok {
		t.Fatal("indexed conversation not found by (adapter, slug)")
	}
	if info.Ref != "row:42" {
		t.Errorf("Info.Ref = %q, want the opaque ref back unmodified", info.Ref)
	}
	if !info.LastActivity.Equal(activity) {
		t.Errorf("Info.LastActivity = %v, want adapter-provided %v", info.LastActivity, activity)
	}
	if got, want := len(info.ResumeCommand), 3; got != want {
		t.Fatalf("ResumeCommand = %v, want dbtool resume conv-42", info.ResumeCommand)
	}
	if info.ResumeCommand[2] != "conv-42" {
		t.Errorf("ResumeCommand resumes by %q, want conv-42", info.ResumeCommand[2])
	}

	// A source Remove event carries the same ref; the index must match it.
	if !idx.RemoveByRef("dbtool", "row:42") {
		t.Fatal("RemoveByRef(row:42) = false, want removal by opaque ref")
	}
	if _, ok := idx.Lookup("dbtool", slug); ok {
		t.Error("conversation still indexed after RemoveByRef")
	}
}

// TestScan_DescribeFailureNotIndexed: an unresolvable ref (deleted row,
// unreadable file) must not be indexed — mirrors the parse-failure behavior
// the file adapters had.
func TestScan_DescribeFailureNotIndexed(t *testing.T) {
	idx := New()
	if slug := idx.Scan(&dbAdapter{}, "row:missing"); slug != "" {
		t.Fatalf("Scan of unresolvable ref returned slug %q, want empty", slug)
	}
	if idx.Count() != 0 {
		t.Errorf("index count = %d, want 0", idx.Count())
	}
}
