//go:build integration

// Shell adapter integration tests. Verifies the core WS→PTY input pipeline.
//
// Run: go test -tags integration -v -timeout 60s -run TestShell ./packages/adapter/adapters/

package adapters

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter/adapters/testutil"
)

// TestShellWSInput verifies that WebSocket input reaches the PTY and produces
// output. This is the foundational test — if this fails, all adapter tests will too.
func TestShellWSInput(t *testing.T) {
	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch([]string{"bash"}, cwd)
	send, _ := g.ConnectSession(sess.ID)
	g.WaitForOutput(sess.ID, 10*time.Second)

	send("echo GMUX_TEST_MARKER_42\r")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		text := testutil.ReadScrollback(t, sess.SocketPath)
		if strings.Contains(text, "GMUX_TEST_MARKER_42") {
			t.Log("WS input verified — marker found in scrollback")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	text := testutil.ReadScrollback(t, sess.SocketPath)
	t.Fatalf("marker not found in scrollback:\n%s", text)
}

// TestShellKill verifies /kill terminates an interactive shell session.
// Regression test for the SIGTERM→SIGHUP change: interactive bash ignores
// SIGTERM but exits on SIGHUP.
func TestShellKill(t *testing.T) {
	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch([]string{"bash"}, cwd)
	g.WaitForOutput(sess.ID, 10*time.Second)

	g.Kill(sess.ID)

	dead := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return !s.Alive
	}, 10*time.Second, "dead after kill")

	if dead.ID != sess.ID {
		t.Fatalf("session ID changed: %s → %s", sess.ID, dead.ID)
	}
	if len(dead.Command) == 0 {
		t.Fatal("dead session should have a resume command")
	}
}

// TestShellKillFish verifies /kill terminates a fish session. Fish ignores
// SIGHUP on interactive shells, so the runner must escalate to SIGKILL.
// Regression test for "dismiss didn't work on /bin/fish sessions".
func TestShellKillFish(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish not installed")
	}
	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch([]string{"fish"}, cwd)
	g.WaitForOutput(sess.ID, 10*time.Second)

	g.Kill(sess.ID)

	g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return !s.Alive
	}, 10*time.Second, "dead after kill (fish ignores SIGHUP)")
}

// TestShellRestart verifies /v1/sessions/:id/restart on an alive session:
// it kills the runner, waits for the exit lifecycle, then relaunches it,
// keeping the same session ID so the frontend's selection remains sticky.
func TestShellRestart(t *testing.T) {
	g := testutil.StartGmuxd(t)
	cwd := t.TempDir()

	sess := g.Launch([]string{"bash"}, cwd)
	g.WaitForOutput(sess.ID, 10*time.Second)

	before, _ := g.GetSession(sess.ID)
	if !before.Alive || before.SocketPath == "" {
		t.Fatalf("expected alive session before restart: alive=%v socket=%q", before.Alive, before.SocketPath)
	}
	oldSocket := before.SocketPath

	g.Restart(sess.ID)

	after := g.WaitForSession(sess.ID, func(s testutil.Session) bool {
		return s.Alive && s.SocketPath != "" && s.SocketPath != oldSocket
	}, 15*time.Second, "alive again with a fresh socket")

	if after.ID != sess.ID {
		t.Fatalf("session ID changed across restart: %s → %s", sess.ID, after.ID)
	}

	// The new runner should accept input and produce output.
	send, _ := g.ConnectSession(sess.ID)
	send("echo GMUX_RESTART_MARKER\r")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		text := testutil.ReadScrollback(t, after.SocketPath)
		if strings.Contains(text, "GMUX_RESTART_MARKER") {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("post-restart marker not found in scrollback")
}
