package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// TestResumeDerivationPresentationMatchesSpawn pins the agreement the Resumer
// contract requires: for a dead row that HAS a conversation ref, one pure
// resolver (discovery.ResolveResumeCommandFor) decides resumability, and both
// consumers honor the same verdict — the wire converter must not advertise a
// resume the production spawner would refuse.
//
// The empty pi conversation is the interesting case: it describes cleanly, so
// only the adapter's ResumeCommand can rule it out.
func TestResumeDerivationPresentationMatchesSpawn(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, []byte(`{"type":"session","version":3,"id":"e-1","timestamp":"2026-03-15T10:00:00Z","cwd":"`+dir+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "full.jsonl")
	body := `{"type":"session","version":3,"id":"f-1","timestamp":"2026-03-15T10:00:00Z","cwd":"` + dir + `"}` + "\n" +
		`{"type":"message","id":"u1","timestamp":"2026-03-15T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Exactly the production wiring from serve_central.go.
	resolve := func(row centralstore.Session) []string {
		legacy := centralSessionToLegacy(row)
		return discovery.ResolveResumeCommandFor(legacy.Adapter, legacy.ConversationRef)
	}
	conv := &wire.Converter{ResumeCommand: func(adapterName, ref string) []string {
		return discovery.ResolveResumeCommandFor(adapterName, ref)
	}}

	for _, tc := range []struct {
		name          string
		ref           string
		wantResumable bool
	}{
		{"empty conversation", empty, false},
		{"conversation with a message", full, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := centralstore.Session{
				ID: centralstore.SessionID(fmt.Sprintf("%07d1", len(tc.name))), Adapter: "pi",
				Command: []string{"pi"}, CWD: dir, ConversationRef: tc.ref,
				CreatedAt: 1700000000000, StatusReported: true,
			}

			// Presentation.
			payload := conv.Sessions(&central.SessionsPayload{Sessions: []central.SessionRow{{
				SessionView: centralstore.SessionView{Session: row},
				Resumable:   true,
			}}}, nil, nil)
			if len(payload.Sessions) != 1 {
				t.Fatalf("converted %d rows, want 1", len(payload.Sessions))
			}
			if got := payload.Sessions[0].Resumable; got != tc.wantResumable {
				t.Errorf("wire Resumable = %v, want %v", got, tc.wantResumable)
			}

			// Execution: the production spawner resolves the same way and
			// refuses a row it cannot build a command for.
			spawner := &productionRunnerSpawner{
				GmuxBin:        "/bin/gmux",
				ResolveDir:     func(centralstore.Session) (string, error) { return dir, nil },
				ResolveCommand: resolve,
				Launch: func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error) {
					return runnerLaunchResult{}, errSpawnReached
				},
			}
			_, err := spawner.Spawn(context.Background(), row)
			if err == nil {
				t.Fatal("Spawn returned no error; the fake launcher must be reached or refused")
			}
			spawnAttempted := err == errSpawnReached
			if spawnAttempted != tc.wantResumable {
				t.Errorf("spawn attempted = %v (err %v), want %v — presentation and execution disagree", spawnAttempted, err, tc.wantResumable)
			}
		})
	}
}

// errSpawnReached marks "the spawner accepted the row and tried to launch".
var errSpawnReached = errTestSentinel("spawn reached launcher")

type errTestSentinel string

func (e errTestSentinel) Error() string { return string(e) }
