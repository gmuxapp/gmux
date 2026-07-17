package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

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
