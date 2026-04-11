//go:build integration

// Codex adapter integration tests.
//
// Run: go test -tags integration -v -timeout 300s -run TestCodex ./packages/adapter/adapters/
//
// Requirements: codex binary on PATH, valid OpenAI API key, internet access.
// Uses gpt-4o-mini (cheapest model).

package adapters

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters/testutil"
)

func requireCodexIntegration(t *testing.T) {
	t.Helper()
	testutil.RequireBinary(t, "codex")
}

var codexModel = []string{"codex", "--model", "gpt-4o-mini"}

// codexSendAndWait sends a message to codex and waits for the response.
func codexSendAndWait(t *testing.T, g *testutil.Gmuxd, send func(string), sessID string) {
	t.Helper()
	// Codex shows a trust prompt for new workspaces — dismiss it.
	s, _ := g.GetSession(sessID)
	g.WaitForScrollback(s.SocketPath, "trust", 15*time.Second)
	time.Sleep(1 * time.Second)
	send("\r") // accept "Yes, continue"

	// Wait for Codex prompt to appear (post-trust).
	g.WaitForScrollback(s.SocketPath, "Codex", 15*time.Second)
	time.Sleep(2 * time.Second)

	// Type message and submit.
	send("say hi")
	time.Sleep(500 * time.Millisecond)
	send("\r")

	// Wait for file attribution.
	g.WaitForSession(sessID, func(s testutil.Session) bool {
		return s.Slug != ""
	}, 90*time.Second, "file attribution (slug)")
}

// TestCodexTurnAndTitle sends a message and verifies title + attribution.
func TestCodexTurnAndTitle(t *testing.T) {
	requireCodexIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(codexModel, cwd)
	if sess.Kind != "codex" {
		t.Fatalf("expected kind=codex, got %q", sess.Kind)
	}
	t.Logf("session %s alive", sess.ID)

	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	codexSendAndWait(t, g, send, sess.ID)

	updated := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Title != "" && s.Title != "codex" && s.Title != "(new)"
	}, 15*time.Second, "title from first user message")
	t.Logf("title: %q", updated.Title)

	attributed, _ := g.GetSession(sess.ID)
	t.Logf("slug: %s", attributed.Slug)
}

// TestCodexSecondTurnKeepsTitle verifies title doesn't change on second message.
func TestCodexSecondTurnKeepsTitle(t *testing.T) {
	requireCodexIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(codexModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	codexSendAndWait(t, g, send, sess.ID)

	first, _ := g.GetSession(sess.ID)
	firstTitle := first.Title
	t.Logf("first title: %q", firstTitle)

	time.Sleep(2 * time.Second)
	send("say goodbye\r")

	gSess, _ := g.GetSession(sess.ID)
	g.WaitForScrollback(gSess.SocketPath, "goodbye", 60*time.Second)
	time.Sleep(3 * time.Second)

	second, _ := g.GetSession(sess.ID)
	if second.Title != firstTitle {
		t.Errorf("title changed from %q to %q after second message", firstTitle, second.Title)
	}
}

// TestCodexResumability does a full kill → resumable → resume cycle.
func TestCodexResumability(t *testing.T) {
	requireCodexIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(codexModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	codexSendAndWait(t, g, send, sess.ID)

	beforeKill, _ := g.GetSession(sess.ID)
	titleBeforeKill := beforeKill.Title

	g.Kill(sess.ID)

	resumable := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return !s.Alive && s.Resumable
	}, 15*time.Second, "resumable")
	t.Logf("resumable: command=%v", resumable.Command)

	if len(resumable.Command) == 0 {
		t.Fatal("expected resume command")
	}

	g.Resume(sess.ID)

	resumed := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Alive && s.SocketPath != ""
	}, 30*time.Second, "resumed alive")

	if resumed.Title != titleBeforeKill {
		t.Errorf("title changed across resume: %q → %q", titleBeforeKill, resumed.Title)
	}
	t.Logf("resumed with title: %q", resumed.Title)
}
