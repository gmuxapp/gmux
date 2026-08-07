package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/socklease"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func TestRunnerMetaWireJSONFieldTable(t *testing.T) {
	want := []struct{ field, tag string }{
		{"ID", "id"}, {"Incarnation", "incarnation"},
		{"Adapter", "adapter"}, {"DriveMode", "drive_mode"}, {"Kind", "kind"},
		{"Alive", "alive"}, {"CreatedAt", "created_at"},
		{"StartedAt", "started_at"}, {"ExitCode", "exit_code"},
		{"ExitedAt", "exited_at"}, {"PID", "pid"},
		{"RunnerVersion", "runner_version"}, {"BinaryHash", "binary_hash"},
		{"ConversationRef", "conversation_file"}, {"CWD", "cwd"},
		{"WorkspaceRoot", "workspace_root"}, {"Slug", "slug"},
		{"ShellTitle", "shell_title"}, {"AdapterTitle", "adapter_title"},
		{"Subtitle", "subtitle"}, {"Command", "command"}, {"Remotes", "remotes"},
		{"ParentSessionID", "parent_session_id"}, {"Status", "status"}, {"Unread", "unread"},
		{"UnreadToken", "unread_token"}, {"TerminalCols", "terminal_cols"}, {"TerminalRows", "terminal_rows"},
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
	status := typeOf.Field(23).Type.Elem()
	if status.NumField() != 3 ||
		status.Field(0).Name != "Active" || status.Field(0).Tag.Get("json") != "active" ||
		status.Field(1).Name != "Error" || status.Field(1).Tag.Get("json") != "error" ||
		status.Field(2).Name != "Interrupted" || status.Field(2).Tag.Get("json") != "interrupted" {
		t.Fatalf("status wire fields=%v, want explicit active/error/interrupted JSON fields", status)
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
	ep, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "1li6tis6", Adapter: "pi", ConversationRef: "conv-ref", CWD: "/gone", Command: []string{"old"}, TerminalCols: &cols, TerminalRows: &rows})
	if err != nil || ep != "fake.sock" {
		t.Fatalf("endpoint=%q err=%v", ep, err)
	}
	if got.ResumeID != "1li6tis6" || got.CWD != "/fallback" || got.InitialCols != cols || got.Rows != rows || !reflect.DeepEqual(got.Command, []string{"pi", "--resume", "conv-ref"}) {
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
	ep, err = spawner.Spawn(context.Background(), centralstore.Session{ID: "1li6tis6", Adapter: "pi", ConversationRef: "conv-ref", CWD: "/gone", TerminalCols: &cols, TerminalRows: &rows})
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

func TestProductionSpawnerExecTransfersPreparedLease(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	endpoint := filepath.Join(paths.SessionSocketDir(), "12iswpkq.sock")
	dir := t.TempDir()
	script := filepath.Join(dir, "lease-runner")
	body := fmt.Sprintf(`#!/usr/bin/python3
import os, socket, time
fd = int(os.environ["_GMXINTERNAL_SOCKET_LEASE_FD"])
os.fstat(fd)
s = socket.socket(socket.AF_UNIX)
s.bind(%q)
s.listen(1)
time.sleep(30)
`, endpoint)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	spawner := &productionRunnerSpawner{
		GmuxBin:        script,
		ResolveDir:     func(centralstore.Session) (string, error) { return dir, nil },
		ResolveCommand: func(centralstore.Session) []string { return []string{"x"} },
		ReadyTimeout:   2 * time.Second,
	}
	ep, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "12iswpkq"})
	if err != nil {
		t.Fatal(err)
	}
	defer spawner.CleanupSpawn(context.Background(), ep)
	if ep != endpoint {
		t.Fatalf("endpoint=%q, want %q", ep, endpoint)
	}
	if contender, err := socklease.AcquireExisting(endpoint); !errors.Is(err, socklease.ErrHeld) {
		if err == nil {
			_ = contender.ReleaseKeepingLockFile()
		}
		t.Fatalf("exec child did not retain lease: %v", err)
	}
}

func TestProductionSpawnerLiveSocketAbortsBeforeLaunch(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	endpoint := filepath.Join(paths.SessionSocketDir(), "184lbyqm.sock")
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	launched := false
	spawner := &productionRunnerSpawner{
		ResolveDir:     func(centralstore.Session) (string, error) { return t.TempDir(), nil },
		ResolveCommand: func(centralstore.Session) []string { return []string{"x"} },
		Launch: func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error) {
			launched = true
			return runnerLaunchResult{}, nil
		},
	}
	if _, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "184lbyqm"}); !errors.Is(err, socklease.ErrSocketLive) {
		t.Fatalf("Spawn error=%v, want live-socket refusal", err)
	}
	if launched {
		t.Fatal("live endpoint allowed child launch")
	}
	conn, err := net.Dial("unix", endpoint)
	if err != nil {
		t.Fatalf("live endpoint was unlinked: %v", err)
	}
	conn.Close()
	if _, err := os.Stat(endpoint + socklease.Suffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fabricated lease history retained: %v", err)
	}
}

func TestProductionSpawnerIdentityChangeAbortsBeforeLaunch(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	endpoint := filepath.Join(paths.SessionSocketDir(), "13kcxk20.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	var replacement net.Listener
	launched := false
	spawner := &productionRunnerSpawner{
		ResolveDir:     func(centralstore.Session) (string, error) { return t.TempDir(), nil },
		ResolveCommand: func(centralstore.Session) []string { return []string{"x"} },
		prepareBeforeRemove: func(path string) {
			if err := os.Rename(path, path+".parked"); err != nil {
				t.Fatal(err)
			}
			replacement, err = net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
		},
		Launch: func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error) {
			launched = true
			return runnerLaunchResult{}, nil
		},
	}
	defer func() {
		if replacement != nil {
			replacement.Close()
		}
	}()
	if _, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "13kcxk20"}); !errors.Is(err, socklease.ErrIdentityChanged) {
		t.Fatalf("Spawn error=%v, want identity change", err)
	}
	if launched {
		t.Fatal("ambiguous replacement allowed child launch")
	}
	conn, err := net.Dial("unix", endpoint)
	if err != nil {
		t.Fatalf("replacement socket was removed: %v", err)
	}
	conn.Close()
}

func TestProductionSpawnerLaunchFailureDropsPreparedHistory(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	spawner := &productionRunnerSpawner{ResolveDir: func(centralstore.Session) (string, error) { return t.TempDir(), nil }, ResolveCommand: func(centralstore.Session) []string { return []string{"x"} }, Launch: func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
		if lease, err := socklease.AcquireExisting(req.Endpoint); !errors.Is(err, socklease.ErrHeld) {
			if err == nil {
				_ = lease.ReleaseKeepingLockFile()
			}
			t.Fatalf("prepared lease not held across launch: %v", err)
		}
		return runnerLaunchResult{}, errors.New("fork failed")
	}}
	if _, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "1c16hjsq"}); err == nil {
		t.Fatal("launch failure swallowed")
	}
	lock := filepath.Join(paths.SessionSocketDir(), "1c16hjsq.sock") + socklease.Suffix
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed launch retained fabricated history: %v", err)
	}
}

func TestProductionSpawnerLaunchFailurePreservesExistingHistory(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", shortSocketDir(t))
	endpoint := filepath.Join(paths.SessionSocketDir(), "1rl73yvr.sock")
	seed, err := socklease.Acquire(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.ReleaseKeepingLockFile(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(endpoint + socklease.Suffix)
	if err != nil {
		t.Fatal(err)
	}
	spawner := &productionRunnerSpawner{
		ResolveDir:     func(centralstore.Session) (string, error) { return t.TempDir(), nil },
		ResolveCommand: func(centralstore.Session) []string { return []string{"x"} },
		Launch: func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
			if lease, err := socklease.AcquireExisting(req.Endpoint); !errors.Is(err, socklease.ErrHeld) {
				if err == nil {
					_ = lease.ReleaseKeepingLockFile()
				}
				t.Fatalf("existing lease not held across launch: %v", err)
			}
			return runnerLaunchResult{}, errors.New("fork failed")
		},
	}
	if _, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "1rl73yvr"}); err == nil {
		t.Fatal("launch failure swallowed")
	}
	after, err := os.Stat(endpoint + socklease.Suffix)
	if err != nil {
		t.Fatalf("pre-existing history removed: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("pre-existing history was replaced")
	}
}

func TestProductionSpawnerLaunchFailureLeavesNoCleanupHandle(t *testing.T) {
	spawner := &productionRunnerSpawner{ResolveDir: func(centralstore.Session) (string, error) { return "/tmp", nil }, ResolveCommand: func(centralstore.Session) []string { return []string{"x"} }, Launch: func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error) {
		return runnerLaunchResult{}, errors.New("fork failed")
	}}
	if _, err := spawner.Spawn(context.Background(), centralstore.Session{ID: "1yg94xqm"}); err == nil {
		t.Fatal("launch failure swallowed")
	}
	if err := spawner.CleanupSpawn(context.Background(), "anything"); err != nil {
		t.Fatalf("failure leaked handle: %v", err)
	}
}
