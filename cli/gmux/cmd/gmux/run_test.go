package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWaitForRegistration_SessionAlreadyPresent returns promptly when
// the session is already in gmuxd's list. Verifies the fast path: we
// don't sleep registrationPollInterval before the first check.
func TestWaitForRegistration_SessionAlreadyPresent(t *testing.T) {
	stateDir, _, cleanup := startTestSocketDaemonWithSessions(t, "dev",
		[]cliSession{{ID: "sess-abcd1234"}})
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	start := time.Now()
	ok := waitForRegistration("sess-abcd1234", time.Second)
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("expected registration to be seen")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("fast path took %v, want prompt return", elapsed)
	}
}

// TestWaitForRegistration_SessionAppearsLater exercises the real
// workflow: the daemon doesn't know about the session when we start
// polling, but it does by the time the child has finished registering.
// This is the race spawnDetached closes.
func TestWaitForRegistration_SessionAppearsLater(t *testing.T) {
	stateDir, addSession, cleanup := startTestSocketDaemonWithSessions(t, "dev", nil)
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	// Simulate the child finishing registration ~150 ms after spawnDetached
	// started waiting — the upper end of what we see in practice.
	go func() {
		time.Sleep(150 * time.Millisecond)
		addSession(cliSession{ID: "sess-cafef00d"})
	}()

	if !waitForRegistration("sess-cafef00d", 2*time.Second) {
		t.Fatal("expected registration to be seen before timeout")
	}
}

// TestWaitForRegistration_Timeout is the failure-mode contract: when
// the session never appears, we bail out within roughly the timeout and
// return false. spawnDetached treats false as "announce anyway" so the
// caller still gets the id; this test just locks in that we don't hang
// indefinitely if gmuxd is stuck.
func TestWaitForRegistration_Timeout(t *testing.T) {
	stateDir, _, cleanup := startTestSocketDaemonWithSessions(t, "dev", nil)
	defer cleanup()
	t.Setenv("XDG_STATE_HOME", stateDir)

	start := time.Now()
	ok := waitForRegistration("sess-nope", 150*time.Millisecond)
	elapsed := time.Since(start)

	if ok {
		t.Error("expected false on timeout")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned early at %v, want to wait at least the timeout", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("returned late at %v, poll loop is too lax", elapsed)
	}
}

// TestAnnounceDetached locks in the stream contract spawnDetached
// relies on: the short session id on stdout, the human message on
// stderr. Anything else (for example mixing the message into stdout,
// or printing a sess- prefix) would break
//
//	id=$(gmux --no-attach cmd)
//
// which is the whole reason announceDetached exists.
func TestAnnounceDetached(t *testing.T) {
	var out, errw bytes.Buffer
	announceDetached(&out, &errw, "sess-cafef00d", "started hello in background (visible in gmux)")

	if got := out.String(); got != "cafef00d\n" {
		t.Errorf("stdout = %q, want \"cafef00d\\n\"", got)
	}
	if got := errw.String(); !strings.Contains(got, "visible in gmux") {
		t.Errorf("stderr = %q, expected human message", got)
	}
}

// TestAnnounceDetached_EmptyMessageIsSilent keeps the stderr stream
// clean when the caller has nothing interesting to say. Scripts that
// redirect stderr to a log file shouldn't get blank lines they have to
// filter out.
func TestAnnounceDetached_EmptyMessageIsSilent(t *testing.T) {
	var out, errw bytes.Buffer
	announceDetached(&out, &errw, "sess-abcd1234", "")

	if got := out.String(); got != "abcd1234\n" {
		t.Errorf("stdout = %q, want \"abcd1234\\n\"", got)
	}
	if errw.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errw.String())
	}
}

// TestNextSessionID covers the forced-ID contract between a detached-
// spawning parent and its child. When --no-attach (or the nested-gmux
// auto-background path) pre-generates a session ID so it can be
// announced to the caller on stdout, the child process must adopt
// that exact ID instead of generating a fresh one. Otherwise the ID
// the parent printed would be a lie.
//
// The helper also clears the env var after reading so the forced ID
// doesn't leak into grandchildren (another session launched from
// inside this one would otherwise silently collide).
func TestNextSessionID_HonorsForcedEnv(t *testing.T) {
	t.Setenv(envForceSessionID, "sess-deadbeef")

	got := nextSessionID()
	if got != "sess-deadbeef" {
		t.Errorf("nextSessionID() = %q, want sess-deadbeef", got)
	}
	if v, ok := os.LookupEnv(envForceSessionID); ok {
		t.Errorf("expected %s to be cleared after read, still set to %q", envForceSessionID, v)
	}
}

func TestNextSessionID_GeneratesWhenUnset(t *testing.T) {
	// Make sure the env var is unset even if something else in the
	// process set it; t.Setenv("", "") isn't quite right, so unset
	// explicitly.
	t.Setenv(envForceSessionID, "")
	os.Unsetenv(envForceSessionID)

	got := nextSessionID()
	if !strings.HasPrefix(got, "sess-") {
		t.Errorf("generated id = %q, want sess- prefix", got)
	}
	if len(got) <= len("sess-") {
		t.Errorf("generated id looks empty: %q", got)
	}
}

// TestNextSessionID_IgnoresMalformedForced guards against an operator
// setting the env var to something that isn't a valid session ID.
// Rather than propagating garbage (which would break `gmux --tail`,
// `--send`, etc.), we fall back to a freshly generated ID. The env
// var is cleared regardless so a bad value doesn't persist.
func TestNextSessionID_IgnoresMalformedForced(t *testing.T) {
	t.Setenv(envForceSessionID, "not-a-session-id")

	got := nextSessionID()
	if !strings.HasPrefix(got, "sess-") {
		t.Errorf("fallback id = %q, want sess- prefix", got)
	}
	if got == "not-a-session-id" {
		t.Error("malformed forced id should not be adopted")
	}
	if _, ok := os.LookupEnv(envForceSessionID); ok {
		t.Error("expected env var to be cleared even when malformed")
	}
}
