//go:build integration

// Claude Code adapter integration tests.
//
// Run: go test -tags integration -v -timeout 300s -run TestClaude ./packages/adapter/adapters/
//
// Requirements: claude binary on PATH, valid API key, internet access.
// Uses claude-haiku-4-5 (cheapest available model).

package adapters

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters/testutil"
)

func requireClaudeIntegration(t *testing.T) {
	t.Helper()
	testutil.RequireBinary(t, "claude")
}

var claudeModel = []string{"claude", "--model", "claude-haiku-4-5"}

// claudeSendAndWait sends a message to claude and waits for the response.
// Claude streams output to the terminal, so we wait for the scrollback to
// grow. File attribution happens after the turn completes.
func claudeSendAndWait(t *testing.T, g *testutil.Gmuxd, send func(string), sessID string) {
	t.Helper()
	// Claude shows a trust prompt for new workspaces — dismiss it.
	s, _ := g.GetSession(sessID)
	g.WaitForScrollback(s.SocketPath, "trust", 15*time.Second)
	send("\r") // accept "Yes, I trust this folder"
	time.Sleep(3 * time.Second)
	send("say hi\r")

	sess, _ := g.GetSession(sessID)
	g.WaitForScrollback(sess.SocketPath, "say hi", 10*time.Second)

	// Wait for file attribution.
	g.WaitForSession(sessID, func(s testutil.Session) bool {
		return s.Slug != ""
	}, 60*time.Second, "file attribution (slug)")
}

// TestClaudeTurnAndTitle sends a message and verifies title + attribution.
func TestClaudeTurnAndTitle(t *testing.T) {
	requireClaudeIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(claudeModel, cwd)
	if sess.Kind != "claude" {
		t.Fatalf("expected kind=claude, got %q", sess.Kind)
	}
	t.Logf("session %s alive", sess.ID)

	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	claudeSendAndWait(t, g, send, sess.ID)

	updated := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Title != "" && s.Title != "claude" && s.Title != "(new)"
	}, 15*time.Second, "title from first user message")
	t.Logf("title: %q", updated.Title)

	attributed, _ := g.GetSession(sess.ID)
	t.Logf("slug: %s", attributed.Slug)
}

// TestClaudeSecondTurnKeepsTitle verifies title doesn't change on second message.
func TestClaudeSecondTurnKeepsTitle(t *testing.T) {
	requireClaudeIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(claudeModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	claudeSendAndWait(t, g, send, sess.ID)

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

// TestClaudeResumability does a full kill → resumable → resume cycle.
func TestClaudeResumability(t *testing.T) {
	requireClaudeIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(claudeModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	claudeSendAndWait(t, g, send, sess.ID)

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
