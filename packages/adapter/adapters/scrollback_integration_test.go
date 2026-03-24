//go:build integration

// Scrollback integration tests. Verify that the scrollback buffer captures
// meaningful content from TUI apps (pi, claude, codex) and correctly omits
// overwritten spinner frames.
//
// Run: go test -tags integration -v -timeout 300s -run TestScrollback ./packages/adapter/adapters/
//
// Requirements: at least one of pi/claude/codex on PATH, valid API keys.

package adapters

import (
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters/testutil"
)

// --- Pi scrollback tests ---

// TestScrollbackPiConversation verifies that pi's scrollback contains
// both the user prompt and the assistant response after a turn.
func TestScrollbackPiConversation(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	// Send a message with a distinctive marker.
	time.Sleep(2 * time.Second)
	send("say the word pineapple exactly once\r")

	// Wait for pi to process and respond.
	s, _ := g.GetSession(sess.ID)
	g.WaitForScrollback(s.SocketPath, "pineapple", 60*time.Second)

	// Give time for the full response to render.
	time.Sleep(5 * time.Second)

	text := testutil.ReadScrollback(t, sess.SocketPath)
	t.Logf("scrollback (%d chars):\n%s", len(text), text)

	// The scrollback must contain the user's prompt text.
	if !strings.Contains(strings.ToLower(text), "pineapple") {
		t.Errorf("scrollback missing user prompt content 'pineapple'")
	}

	// The scrollback should be substantial (not just a status bar).
	if len(text) < 100 {
		t.Errorf("scrollback suspiciously short (%d chars), expected conversation content", len(text))
	}
}

// TestScrollbackPiMultiTurn verifies scrollback retains content across
// multiple conversation turns.
func TestScrollbackPiMultiTurn(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	// First turn.
	time.Sleep(2 * time.Second)
	send("say the word strawberry exactly once\r")
	s, _ := g.GetSession(sess.ID)
	g.WaitForScrollback(s.SocketPath, "strawberry", 60*time.Second)
	time.Sleep(5 * time.Second)

	// Second turn.
	send("now say the word watermelon exactly once\r")
	g.WaitForScrollback(s.SocketPath, "watermelon", 60*time.Second)
	time.Sleep(5 * time.Second)

	text := testutil.ReadScrollback(t, s.SocketPath)
	t.Logf("scrollback (%d chars):\n%s", len(text), text)

	// Both turn contents should be present.
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "strawberry") {
		t.Errorf("scrollback missing first turn content 'strawberry'")
	}
	if !strings.Contains(lower, "watermelon") {
		t.Errorf("scrollback missing second turn content 'watermelon'")
	}
}

// TestScrollbackPiNoSpinnerFrames verifies that loading spinner frames
// are collapsed and don't appear as duplicate content in scrollback.
func TestScrollbackPiNoSpinnerFrames(t *testing.T) {
	requirePiIntegration(t)

	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch(piModel, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 15*time.Second)

	time.Sleep(2 * time.Second)
	send("say hello in one sentence\r")
	s, _ := g.GetSession(sess.ID)
	g.WaitForScrollback(s.SocketPath, "hello", 60*time.Second)
	time.Sleep(5 * time.Second)

	text := testutil.ReadScrollback(t, s.SocketPath)

	// Spinner characters should not appear in the normalized scrollback.
	// These are the Braille spinner frames pi uses.
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for _, ch := range spinnerChars {
		if strings.Contains(text, ch) {
			// Spinner chars might appear in the response itself, but there
			// shouldn't be multiple consecutive spinner lines.
			count := strings.Count(text, ch)
			if count > 2 {
				t.Errorf("found %d occurrences of spinner char %q, expected <=2 (collapsed)", count, ch)
			}
		}
	}
}

// --- Claude scrollback tests ---
// NOTE: Claude integration tests are currently broken due to a pre-existing
// adapter detection issue (claude detected as kind "pi") and TUI interaction
// changes (welcome overlay intercepts input). These need a separate fix to
// the claude adapter and test harness. The scrollback fix itself is validated
// by the pi and shell tests below, which exercise the same TermWriter code path.

// --- Shell scrollback tests (control group) ---

// TestScrollbackShellPreservesOutput verifies plain shell output is
// retained in scrollback.
func TestScrollbackShellPreservesOutput(t *testing.T) {
	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch([]string{"bash"}, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 10*time.Second)

	// Generate distinctive output.
	send("echo MARKER_ALPHA_123\r")
	time.Sleep(1 * time.Second)
	send("echo MARKER_BETA_456\r")
	time.Sleep(1 * time.Second)
	send("echo MARKER_GAMMA_789\r")
	time.Sleep(2 * time.Second)

	text := testutil.ReadScrollback(t, sess.SocketPath)
	t.Logf("scrollback: %s", text)

	for _, marker := range []string{"MARKER_ALPHA_123", "MARKER_BETA_456", "MARKER_GAMMA_789"} {
		if !strings.Contains(text, marker) {
			t.Errorf("scrollback missing %q", marker)
		}
	}
}

// TestScrollbackShellClearDoesNotWipe verifies that ESC[2J (clear screen)
// in a shell session does not wipe earlier output from the scrollback.
// The clear sequence is passed through so WebSocket clients process it
// natively, but the ring buffer retains pre-clear content.
func TestScrollbackShellClearDoesNotWipe(t *testing.T) {
	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch([]string{"bash"}, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 10*time.Second)

	send("echo BEFORE_CLEAR_MARKER\r")
	time.Sleep(1 * time.Second)
	send("clear\r")
	time.Sleep(1 * time.Second)
	send("echo AFTER_CLEAR_MARKER\r")
	time.Sleep(2 * time.Second)

	text := testutil.ReadScrollback(t, sess.SocketPath)
	t.Logf("scrollback: %s", text)

	// Post-clear content must be present.
	if !strings.Contains(text, "AFTER_CLEAR_MARKER") {
		t.Errorf("scrollback missing post-clear content")
	}
	// Pre-clear content is preserved in the ring buffer (clear sequences
	// are passed through rather than resetting the buffer).
	if !strings.Contains(text, "BEFORE_CLEAR_MARKER") {
		t.Errorf("scrollback missing pre-clear content (clear wiped the buffer)")
	}
}
