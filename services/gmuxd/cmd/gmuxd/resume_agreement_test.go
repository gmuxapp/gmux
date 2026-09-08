package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// The production value itself, not a copy of it: serve_central.go injects
	// exactly this function as productionRunnerSpawner.ResolveCommand, so
	// reverting the wiring to the resume-only resolver fails here.
	resolve := productionResolveRelaunchCommand
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

// TestRelaunchDerivationPresentationMatchesSpawn is the same agreement for
// rows with NO conversation ref — the shape that shipped broken. Every dead
// shell session is one (the shell has no hook that binds a conversation), and
// so is any agent that died before its first hook event. Presentation derived
// "resumable" from the durable command while the spawner only ever resolved
// the *resume* command, so the sidebar offered an action the daemon answered
// with "session is not resumable" (and Restart killed the session first).
//
// Sweeping every registered adapter keeps this systemic: a new adapter cannot
// reintroduce the divergence for its own kind.
func TestRelaunchDerivationPresentationMatchesSpawn(t *testing.T) {
	dir := t.TempDir()
	resolve := productionResolveRelaunchCommand
	conv := &wire.Converter{ResumeCommand: func(adapterName, ref string) []string {
		return discovery.ResolveResumeCommandFor(adapterName, ref)
	}}

	adapterNames := []string{"shell", "editor", "claude", "codex", "pi", "acp-unknown"}
	for i, name := range adapterNames {
		for _, tc := range []struct {
			label   string
			command []string
			// A dead row with no conversation ref is rerunnable exactly
			// when it recorded a command.
			wantRelaunch string
		}{
			{"recorded command", []string{"/root/.local/bin/fish"}, "rerun"},
			{"no command", nil, ""},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				row := centralstore.Session{
					ID: centralstore.SessionID(fmt.Sprintf("%06dx%d", i, len(tc.label))), Adapter: name,
					Command: tc.command, CWD: dir, CreatedAt: 1700000000000, StatusReported: true,
				}

				// Presentation: the composer's own Resumable term (dead ∧
				// command ∧ verdict ≠ Gone) feeds the converter, exactly
				// like production.
				payload := conv.Sessions(&central.SessionsPayload{Sessions: []central.SessionRow{{
					SessionView: centralstore.SessionView{Session: row},
					Resumable:   len(tc.command) > 0,
				}}}, nil, nil)
				got := payload.Sessions[0]
				if got.Relaunch != tc.wantRelaunch {
					t.Errorf("wire relaunch = %q, want %q", got.Relaunch, tc.wantRelaunch)
				}
				if got.Resumable != (tc.wantRelaunch != "") {
					t.Errorf("wire resumable = %v, want %v", got.Resumable, tc.wantRelaunch != "")
				}

				// Execution: the production spawner must accept exactly the
				// rows presentation advertised.
				spawner := &productionRunnerSpawner{
					GmuxBin:        "/bin/gmux",
					ResolveDir:     func(centralstore.Session) (string, error) { return dir, nil },
					ResolveCommand: resolve,
					Launch: func(context.Context, runnerLaunchRequest) (runnerLaunchResult, error) {
						return runnerLaunchResult{}, errSpawnReached
					},
				}
				_, err := spawner.Spawn(context.Background(), row)
				spawnAttempted := err == errSpawnReached
				if spawnAttempted != got.Resumable {
					t.Errorf("spawn attempted = %v (err %v), want %v — presentation and execution disagree",
						spawnAttempted, err, got.Resumable)
				}
			})
		}
	}
}

// TestRelaunchSpawnRerunsTheRecordedCommand pins what a rerun actually
// executes: the recorded command, in the resolved directory, under the same
// session id (the runner re-registers the same session, as resume does).
func TestRelaunchSpawnRerunsTheRecordedCommand(t *testing.T) {
	dir := t.TempDir()
	var seen runnerLaunchRequest
	spawner := &productionRunnerSpawner{
		GmuxBin:        "/bin/gmux",
		ResolveDir:     func(centralstore.Session) (string, error) { return dir, nil },
		ResolveCommand: productionResolveRelaunchCommand,
		Launch: func(_ context.Context, req runnerLaunchRequest) (runnerLaunchResult, error) {
			seen = req
			return runnerLaunchResult{}, errSpawnReached
		},
	}
	row := centralstore.Session{
		ID: "1rerun01", Adapter: "shell", Command: []string{"bash", "-lc", "echo hi"},
		CWD: dir, CreatedAt: 1700000000000, StatusReported: true,
	}
	if _, err := spawner.Spawn(context.Background(), row); err != errSpawnReached {
		t.Fatalf("Spawn err = %v, want the launcher to be reached", err)
	}
	if want := []string{"bash", "-lc", "echo hi"}; !slicesEqual(seen.Command, want) {
		t.Errorf("launched command = %v, want %v", seen.Command, want)
	}
	if seen.CWD != dir {
		t.Errorf("launched cwd = %q, want %q", seen.CWD, dir)
	}
	if seen.ResumeID != "1rerun01" {
		t.Errorf("launched ResumeID = %q, want the same session id", seen.ResumeID)
	}
}

// TestRelaunchGuardMessagesAreActionable pins the two refusals the daemon can
// still emit and that each says what to do about it.
func TestRelaunchGuardMessagesAreActionable(t *testing.T) {
	exited := centralstore.UnixMillis(1700000001000)
	alive := centralstore.Session{ID: "1alive00", Adapter: "shell", Command: []string{"bash"}}
	status, code, msg := relaunchGuard(alive)
	if status == 0 || code != "not_resumable" {
		t.Errorf("running session: status=%d code=%q, want a not_resumable refusal", status, code)
	}
	// The same refusal covers a row whose runner is gone but whose exit is
	// not durable yet (the convergence window), so it must not order the user
	// to stop a process that may not exist.
	if strings.Contains(msg, "stop it first") || !strings.Contains(msg, "retry in a moment") {
		t.Errorf("exit-less message = %q, want advice valid for both a running and a converging row", msg)
	}
	bare := centralstore.Session{ID: "1bare000", Adapter: "shell", ExitedAt: &exited}
	_, _, msg = relaunchGuard(bare)
	if !strings.Contains(msg, "no recorded command to rerun") {
		t.Errorf("bare row message = %q, want it to name the missing command", msg)
	}
	goneConv := centralstore.Session{ID: "1gone000", Adapter: "pi", Command: []string{"pi"},
		ExitedAt: &exited, ConversationRef: filepath.Join(t.TempDir(), "missing.jsonl")}
	_, _, msg = relaunchGuard(goneConv)
	if !strings.Contains(msg, "missing.jsonl") || !strings.Contains(msg, "start a new session") {
		t.Errorf("gone-conversation message = %q, want the ref and a next step", msg)
	}
	ok := centralstore.Session{ID: "1okrerun", Adapter: "shell", Command: []string{"bash"}, ExitedAt: &exited}
	if status, _, msg := relaunchGuard(ok); status != 0 {
		t.Errorf("rerunnable row refused: status=%d msg=%q", status, msg)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRestartGuardRefusesBeforeStopping: restart is stop+spawn, so the check
// that the spawner will accept the row has to happen while the session is
// still running. A live shell must pass (it reruns its command); a live agent
// whose conversation vanished must be refused rather than stopped and lost.
func TestRestartGuardRefusesBeforeStopping(t *testing.T) {
	liveShell := centralstore.Session{ID: "1liveshl", Adapter: "shell", Command: []string{"bash"}, CWD: t.TempDir()}
	if status, _, msg := relaunchCommandGuard(liveShell); status != 0 {
		t.Errorf("live shell restart refused: status=%d msg=%q", status, msg)
	}
	lostConv := centralstore.Session{ID: "1lostcnv", Adapter: "pi", Command: []string{"pi"},
		CWD: t.TempDir(), ConversationRef: filepath.Join(t.TempDir(), "vanished.jsonl")}
	status, code, msg := relaunchCommandGuard(lostConv)
	if status == 0 || code != "not_resumable" {
		t.Fatalf("lost conversation accepted for restart: status=%d code=%q", status, code)
	}
	if !strings.Contains(msg, "vanished.jsonl") {
		t.Errorf("message = %q, want the unresolvable ref named", msg)
	}
	// The exit precondition belongs to resume alone: restart's guard must not
	// reject a session for still running.
	if status, _, _ := relaunchGuard(liveShell); status == 0 {
		t.Error("resume guard accepted a session that has not exited")
	}
}
