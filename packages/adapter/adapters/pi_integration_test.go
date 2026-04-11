//go:build integration

// Pi adapter integration tests. These launch real pi processes through gmuxd
// and verify the full pipeline: launch → title → attribution → resume.
//
// Run: go test -tags integration -v -timeout 300s -run TestPi ./packages/adapter/adapters/
//
// Requirements: pi binary on PATH, valid API key, internet access.
// Uses claude-haiku-4-5 (cheapest available model).

package adapters

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters/testutil"
)

func requirePiIntegration(t *testing.T) {
	t.Helper()
	testutil.RequireBinary(t, "pi")
}

var piModel = []string{"pi", "--model", "claude-haiku-4-5"}

// sendAndWaitForTurn sends a message and waits for the assistant response to
// complete. Pi writes user+assistant messages to the JSONL as a batch after
// the turn finishes, so we detect completion via file attribution + title
// rather than transient working status.
func sendAndWaitForTurn(t *testing.T, g *testutil.Gmuxd, send func(string), sessID string) {
	t.Helper()
	// Brief pause for TUI input handler to be fully ready after render.
	time.Sleep(2 * time.Second)
	send("say hi\r")

	// Verify pi received the input by checking scrollback.
	sess, _ := g.GetSession(sessID)
	g.WaitForScrollback(sess.SocketPath, "say hi", 10*time.Second)

	// Wait for the turn to produce output (scrollback will change when
	// the assistant responds). Pi's scrollback grows as the response streams.
	g.WaitForScrollback(sess.SocketPath, "Hi", 60*time.Second)

	// Wait for file attribution (pi writes the full turn to JSONL after completion).
	g.WaitForSession(sessID, func(s testutil.Session) bool {
		return s.Slug != ""
	}, 30*time.Second, "file attribution (slug)")
}

// TestPiTurnAndTitle sends a message and verifies title + attribution.
func TestPiTurnAndTitle(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	if sess.Kind != "pi" {
		t.Fatalf("expected kind=pi, got %q", sess.Kind)
	}

	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	sendAndWaitForTurn(t, g, send, sess.ID)

	// Title should come from the first user message.
	updated := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Title != "" && s.Title != "pi" && s.Title != "(new)"
	}, 15*time.Second, "title from first user message")
	t.Logf("title=%q slug=%s", updated.Title, updated.Slug)
}

// TestPiNameOverridesTitle sends a message, then uses /name to override.
func TestPiNameOverridesTitle(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	sendAndWaitForTurn(t, g, send, sess.ID)

	initial, _ := g.GetSession(sess.ID)
	t.Logf("initial title: %q", initial.Title)

	// Set explicit name.
	send("/name integration-test-name\r")

	// Title should update to the explicit name.
	g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Title == "integration-test-name"
	}, 30*time.Second, "title from /name")
	t.Log("title overridden to 'integration-test-name'")
}

// TestPiSecondTurnKeepsTitle verifies title doesn't change on second message.
func TestPiSecondTurnKeepsTitle(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	sendAndWaitForTurn(t, g, send, sess.ID)

	first, _ := g.GetSession(sess.ID)
	firstTitle := first.Title
	t.Logf("first title: %q", firstTitle)

	// Second message.
	time.Sleep(2 * time.Second)
	send("say goodbye\r")

	// Wait for second response.
	gSess, _ := g.GetSession(sess.ID)
	g.WaitForScrollback(gSess.SocketPath, "goodbye", 60*time.Second)

	// Brief wait for file to be written and parsed.
	time.Sleep(3 * time.Second)

	second, _ := g.GetSession(sess.ID)
	if second.Title != firstTitle {
		t.Errorf("title changed from %q to %q after second message", firstTitle, second.Title)
	}
}

// TestPiResumability does a full kill → resumable → resume cycle.
func TestPiResumability(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	sendAndWaitForTurn(t, g, send, sess.ID)

	beforeKill, _ := g.GetSession(sess.ID)
	titleBeforeKill := beforeKill.Title

	// Kill.
	g.Kill(sess.ID)

	// Should become resumable.
	resumable := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return !s.Alive && s.Resumable
	}, 15*time.Second, "resumable")
	t.Logf("resumable: command=%v", resumable.Command)

	if len(resumable.Command) == 0 {
		t.Fatal("expected resume command")
	}

	// Resume.
	g.Resume(sess.ID)

	resumed := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Alive && s.SocketPath != ""
	}, 30*time.Second, "resumed alive")

	if resumed.Title != titleBeforeKill {
		t.Errorf("title changed across resume: %q → %q", titleBeforeKill, resumed.Title)
	}
	t.Logf("resumed with title: %q", resumed.Title)
}
