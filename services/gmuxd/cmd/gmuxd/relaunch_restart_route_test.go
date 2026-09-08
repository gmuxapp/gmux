package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionmeta"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// countingSpawner records whether the lifecycle path was entered at all.
type countingSpawner struct{ spawns atomic.Int64 }

func (s *countingSpawner) Spawn(context.Context, centralstore.Session) (string, error) {
	s.spawns.Add(1)
	return "", context.Canceled // never reached in these tests
}

type countingControl struct{ kills atomic.Int64 }

// Terminate records the kill and then fails, so a restart that gets this far
// unwinds immediately instead of waiting out an observed death that this
// harness's fake runner will never report.
func (c *countingControl) Terminate(context.Context, string, string) error {
	c.kills.Add(1)
	return errors.New("terminate refused by the test control")
}

func (c *countingControl) Reap(context.Context, string, string) error { return nil }

// restartRouteHarness registers a live runner for a row the relaunch policy
// refuses (an unresolvable conversation ref) and returns the pieces needed to
// drive POST /v1/sessions/{id}/restart through the real route dispatch.
func restartRouteHarness(t *testing.T, ref string) (*Bootstrap, *countingSpawner, *countingControl) {
	t.Helper()
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cwd := t.TempDir()
	active := true
	cmd := []string{"claude"}
	facts := centralstore.RunnerFacts{Active: &active, CWD: &cwd, ConversationRef: &ref, Command: &cmd}
	runners := &bootstrapRunners{
		metas: map[string]sessioncoord.RunnerMeta{"ep-restart": {PID: 321, Incarnation: "inc-restart", Registration: centralstore.RunnerRegistration{
			ID: productionSessionID, Adapter: "claude", Alive: true, CreatedAt: 1, ObservedAt: 1, Facts: facts,
		}}},
		blocked: map[string]bool{},
	}
	spawner, control := &countingSpawner{}, &countingControl{}
	reg := sessioncoord.NewRegistry()
	coord := sessioncoord.New(reg, runners, st, nil, nil,
		sessioncoord.WithRunnerSpawner(spawner), sessioncoord.WithRunnerControl(control))
	t.Cleanup(coord.Close)
	if _, err := coord.Register(ctx, sessioncoord.RegisterRequest{Endpoint: "ep-restart"}); err != nil {
		t.Fatal(err)
	}
	return &Bootstrap{Store: st, Registry: reg, Coordinator: coord}, spawner, control
}

func postRestart(t *testing.T, boot *Bootstrap) recorded {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+string(productionSessionID)+"/restart", nil)
	handleCentralSessionAction(rec, req, boot, newSSEFanout(), &wire.Converter{}, nil, sessionmeta.New(t.TempDir()), "/usr/bin/gmux", nil)
	return parseRecorded(t, rec)
}

// TestRestartRouteRefusesWithoutEnteringTheLifecycle is the ordering assertion
// at the level where the ordering is written: the handler must refuse a row
// the relaunch policy declines *before* handing it to the coordinator, because
// the coordinator's restart stops the runner first. Moving the guard after
// boot.Coordinator.Restart(...) makes this fail (the terminate lands and the
// live entry disappears), which a pure-function guard test cannot see.
func TestRestartRouteRefusesWithoutEnteringTheLifecycle(t *testing.T) {
	boot, spawner, control := restartRouteHarness(t, filepath.Join(t.TempDir(), "vanished.jsonl"))

	got := postRestart(t, boot)
	if got.code != http.StatusBadRequest || got.errCode() != "not_resumable" {
		t.Fatalf("code=%d body=%v, want 400 not_resumable", got.code, got.body)
	}
	if control.kills.Load() != 0 {
		t.Errorf("terminate issued %d time(s): the session was stopped for a restart that cannot happen", control.kills.Load())
	}
	if spawner.spawns.Load() != 0 {
		t.Errorf("spawn attempted %d time(s) after a refusal", spawner.spawns.Load())
	}
	if _, live := registryRuntime(boot.Registry, productionSessionID); !live {
		t.Error("the live runner must survive a refused restart")
	}
}

// The same route, with a relaunchable row, must reach the lifecycle: otherwise
// the test above would pass for the wrong reason (a handler that refuses
// everything).
func TestRestartRouteEntersTheLifecycleForARelaunchableRow(t *testing.T) {
	boot, _, control := restartRouteHarness(t, "")

	if got := postRestart(t, boot); got.code == http.StatusBadRequest && got.errCode() == "not_resumable" {
		t.Fatalf("a rerunnable row was refused: %v", got.body)
	}
	if control.kills.Load() == 0 {
		t.Error("the lifecycle was never entered for a relaunchable row")
	}
}
