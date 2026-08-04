package conversations

import (
	"errors"
	"strconv"
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
	name      string                              // adapter name; "dbtool" when empty
	rows      map[string]adapter.ConversationInfo // ref → row
	describes int                                 // DescribeConversation call count
	// zeroLenResume makes the non-resumable verdict a non-nil empty slice
	// instead of nil — a legal adapter return the contract treats the same.
	zeroLenResume bool
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
	d.describes++
	row, ok := d.rows[ref]
	if !ok {
		return nil, errors.New("no such row")
	}
	row.Ref = ref
	return &row, nil
}

// ResumeCommand carries the adapter's resumability verdict: an empty
// conversation yields no command (adapter.Resumer contract).
func (d *dbAdapter) ResumeCommand(info *adapter.ConversationInfo) []string {
	if info == nil || info.MessageCount == 0 {
		if d.zeroLenResume {
			return []string{}
		}
		return nil
	}
	return []string{"dbtool", "resume", info.ID}
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

func TestScan_DescribeFailurePreservesCachedResumeCommand(t *testing.T) {
	a := &dbAdapter{rows: map[string]adapter.ConversationInfo{
		"row:7": {ID: "conv-7", Slug: "seven", Cwd: "/home/u/proj", MessageCount: 2},
	}}
	idx := New()
	if slug := idx.Scan(a, "row:7"); slug != "seven" {
		t.Fatalf("initial Scan slug = %q, want seven", slug)
	}

	delete(a.rows, "row:7") // transient descriptor failure on the next upsert
	if slug := idx.Scan(a, "row:7"); slug != "" {
		t.Fatalf("failed rescan slug = %q, want empty", slug)
	}
	if cmd := idx.LookupResumeCommand("dbtool", "row:7"); len(cmd) != 3 || cmd[2] != "conv-7" {
		t.Fatalf("failed rescan replaced stale-good command with %v", cmd)
	}

	if !idx.RemoveByRef("dbtool", "row:7") {
		t.Fatal("RemoveByRef returned false after transient failure")
	}
	if cmd := idx.LookupResumeCommand("dbtool", "row:7"); len(cmd) != 0 {
		t.Fatalf("RemoveByRef retained resume command %v", cmd)
	}
}

// TestScan_NonResumableNotIndexed pins the caller side of the Resumer
// contract: a conversation that describes cleanly but whose adapter returns
// no resume command is not indexed and yields no slug. It also pins the
// single-Describe property the CanResume removal bought — Scan must not
// re-describe the ref to ask about resumability.
func TestScan_NonResumableNotIndexed(t *testing.T) {
	a := &dbAdapter{rows: map[string]adapter.ConversationInfo{
		"row:empty": {
			ID:           "conv-empty",
			Title:        "started, never used",
			Slug:         "started-never-used",
			Cwd:          "/home/u/proj",
			MessageCount: 0,
		},
	}}

	idx := New()
	if slug := idx.Scan(a, "row:empty"); slug != "" {
		t.Fatalf("Scan of non-resumable conversation returned slug %q, want empty", slug)
	}
	if idx.Count() != 0 {
		t.Errorf("index Count = %d, want 0", idx.Count())
	}
	if _, ok := idx.Lookup("dbtool", "started-never-used"); ok {
		t.Error("non-resumable conversation is reachable by slug")
	}
	if a.describes != 1 {
		t.Errorf("DescribeConversation called %d times, want exactly 1", a.describes)
	}
}

// TestScan_ResumableDescribesOnce is the positive half of the same property.
func TestScan_ResumableDescribesOnce(t *testing.T) {
	a := &dbAdapter{rows: map[string]adapter.ConversationInfo{
		"row:7": {ID: "conv-7", Slug: "seven", Cwd: "/home/u/proj", MessageCount: 2},
	}}
	idx := New()
	if slug := idx.Scan(a, "row:7"); slug != "seven" {
		t.Fatalf("Scan slug = %q, want seven", slug)
	}
	if a.describes != 1 {
		t.Errorf("DescribeConversation called %d times, want exactly 1", a.describes)
	}
}

// TestLookupResumeCommandDoesNotRedescribe is the list-path scaling guard.
// Conversation sources pay the descriptor I/O once when indexing; arbitrarily
// many session renders are pure cache reads rather than transcript reads.
func TestLookupResumeCommandDoesNotRedescribe(t *testing.T) {
	const sessions = 1000
	a := &dbAdapter{rows: make(map[string]adapter.ConversationInfo, sessions)}
	idx := New()
	for i := range sessions {
		n := strconv.Itoa(i)
		ref := "row:" + n
		a.rows[ref] = adapter.ConversationInfo{ID: "conv-" + n, Slug: "session-" + n, Cwd: "/home/u/proj", MessageCount: 2}
		idx.Scan(a, ref)
	}
	if a.describes != sessions {
		t.Fatalf("indexing caused %d DescribeConversation calls, want %d", a.describes, sessions)
	}

	// This loop models conversion of 1000 distinct dead session rows.
	for i := range sessions {
		n := strconv.Itoa(i)
		cmd := idx.LookupResumeCommand("dbtool", "row:"+n)
		if len(cmd) != 3 || cmd[2] != "conv-"+n {
			t.Fatalf("LookupResumeCommand(%d) = %v, want cached resume command", i, cmd)
		}
		cmd[0] = "mutated"
	}
	if a.describes != sessions {
		t.Fatalf("rendering %d dead sessions caused descriptor I/O: calls=%d, want %d indexing calls", sessions, a.describes, sessions)
	}

	idx.RemoveByRef("dbtool", "row:7")
	if cmd := idx.LookupResumeCommand("dbtool", "row:7"); len(cmd) != 0 {
		t.Fatalf("removed ref retained resume command %v", cmd)
	}
}

// TestScan_ZeroLengthResumeCommandNotIndexed pins the len-based reading of the
// Resumer contract: an adapter may signal "not resumable" with a non-nil empty
// slice, and Scan must exclude it exactly as it excludes nil. A consumer that
// checked `cmd == nil` instead of `len(cmd) == 0` would index a row whose
// resume command cannot be executed.
func TestScan_ZeroLengthResumeCommandNotIndexed(t *testing.T) {
	a := &dbAdapter{zeroLenResume: true, rows: map[string]adapter.ConversationInfo{
		"row:empty": {
			ID:           "conv-empty",
			Title:        "started, never used",
			Slug:         "started-never-used",
			Cwd:          "/home/u/proj",
			MessageCount: 0,
		},
	}}
	if got := a.ResumeCommand(&adapter.ConversationInfo{}); got == nil || len(got) != 0 {
		t.Fatalf("fake must return a non-nil zero-length command, got %#v", got)
	}

	idx := New()
	if slug := idx.Scan(a, "row:empty"); slug != "" {
		t.Fatalf("Scan of zero-length resume command returned slug %q, want empty", slug)
	}
	if idx.Count() != 0 {
		t.Errorf("index Count = %d, want 0", idx.Count())
	}
	if _, ok := idx.Lookup("dbtool", "started-never-used"); ok {
		t.Error("zero-length-resume conversation is reachable by slug")
	}
}
