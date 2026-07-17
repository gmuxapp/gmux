package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func TestRunnerMetaWireJSONFieldTable(t *testing.T) {
	want := []struct{ field, tag string }{
		{"ID", "id"}, {"Adapter", "adapter"}, {"Kind", "kind"},
		{"Alive", "alive"}, {"CreatedAt", "created_at"}, {"PID", "pid"},
		{"RunnerVersion", "runner_version"}, {"BinaryHash", "binary_hash"},
		{"ConversationRef", "conversation_file"}, {"CWD", "cwd"},
		{"WorkspaceRoot", "workspace_root"}, {"Slug", "slug"},
		{"ShellTitle", "shell_title"}, {"AdapterTitle", "adapter_title"},
		{"Subtitle", "subtitle"}, {"Command", "command"}, {"Remotes", "remotes"},
		{"Status", "status"}, {"Unread", "unread"},
		{"TerminalCols", "terminal_cols"}, {"TerminalRows", "terminal_rows"},
	}
	typeOf := reflect.TypeOf(runnerMetaWire{})
	if typeOf.NumField() != len(want) {
		t.Fatalf("runnerMetaWire fields=%d, want exact table of %d", typeOf.NumField(), len(want))
	}
	for i, entry := range want {
		field := typeOf.Field(i)
		if field.Name != entry.field || field.Tag.Get("json") != entry.tag {
			t.Errorf("field[%d]=%s json:%q, want %s json:%q", i, field.Name, field.Tag.Get("json"), entry.field, entry.tag)
		}
	}
	status := typeOf.Field(17).Type.Elem()
	if status.NumField() != 2 || status.Field(0).Name != "Working" || status.Field(0).Tag.Get("json") != "working" || status.Field(1).Name != "Error" || status.Field(1).Tag.Get("json") != "error" {
		t.Fatalf("status wire fields=%v, want explicit working/error JSON fields", status)
	}
}

func TestProductionSpawnerLaunchPolicyAndCleanup(t *testing.T) {
	cols, rows := uint16(132), uint16(47)
	var got runnerLaunchRequest
	terminated := false
	spawner := &productionRunnerSpawner{
		GmuxBin: "/bin/gmux",
		ResolveDir: func(row centralstore.Session) (string, error) {
			if row.CWD != "/gone" {
				t.Fatalf("row=%+v", row)
			}
			return "/fallback", nil
		},
		ResolveCommand: func(row centralstore.Session) []string {
			if row.Adapter != "pi" || row.ConversationRef != "conv-ref" {
				t.Fatalf("identity lost: %+v", row)
			}
			return []string{"pi", "--resume", "conv-ref"}
		},
		Launch: func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
			got = req
			return runnerLaunchResult{PID: 77, Endpoint: "fake.sock", Terminate: func(context.Context) error { terminated = true; return nil }}, nil
		},
	}
	ep, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "sess-spawn", Adapter: "pi", ConversationRef: "conv-ref", CWD: "/gone", Command: []string{"old"}, TerminalCols: &cols, TerminalRows: &rows})
	if err != nil || ep != "fake.sock" {
		t.Fatalf("endpoint=%q err=%v", ep, err)
	}
	if got.ResumeID != "sess-spawn" || got.CWD != "/fallback" || got.InitialCols != cols || got.Rows != rows || !reflect.DeepEqual(got.Command, []string{"pi", "--resume", "conv-ref"}) {
		t.Fatalf("launch request=%+v", got)
	}
	spawner.FinalizeSpawn(ep)
	if len(spawner.launched) != 0 || terminated {
		t.Fatalf("successful finalization retained=%d terminated=%v", len(spawner.launched), terminated)
	}
	// Cleanup after ownership transfer is a no-op and must not kill the child.
	if err := spawner.CleanupSpawn(context.Background(), ep); err != nil || terminated {
		t.Fatalf("cleanup after finalize err=%v terminated=%v", err, terminated)
	}
	// Launch again to pin failed-registration cleanup termination.
	ep, err = spawner.Spawn(context.Background(), centralstore.Session{ID: "sess-spawn", Adapter: "pi", ConversationRef: "conv-ref", CWD: "/gone", TerminalCols: &cols, TerminalRows: &rows})
	if err != nil {
		t.Fatal(err)
	}
	if err := spawner.CleanupSpawn(context.Background(), ep); err != nil || !terminated {
		t.Fatalf("cleanup err=%v terminated=%v", err, terminated)
	}
	if err := spawner.CleanupSpawn(context.Background(), ep); err != nil {
		t.Fatalf("idempotent cleanup: %v", err)
	}
}

func TestProductionSpawnerLaunchFailureLeavesNoCleanupHandle(t *testing.T) {
	spawner := &productionRunnerSpawner{ResolveDir: func(centralstore.Session) (string, error) { return "/tmp", nil }, ResolveCommand: func(centralstore.Session) []string { return []string{"x"} }, Launch: func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error) {
		return runnerLaunchResult{}, errors.New("fork failed")
	}}
	if _, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "sess-fail"}); err == nil {
		t.Fatal("launch failure swallowed")
	}
	if err := spawner.CleanupSpawn(context.Background(), "anything"); err != nil {
		t.Fatalf("failure leaked handle: %v", err)
	}
}
