package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func passthroughTestEnv(t *testing.T) ([]string, *fakeDaemon) {
	t.Helper()
	stateHome := t.TempDir()
	socketDir := t.TempDir() + "/sessions"
	d := startFakeDaemon(t, stateHome+"/gmux/gmuxd.sock")
	env := launchEnv(stateHome, socketDir)

	binDir := t.TempDir()
	pi := filepath.Join(binDir, "pi")
	if err := os.WriteFile(pi, []byte("#!/bin/sh\nprintf 'internal=%s\\npublic=%s\\nuser=%s\\n' \"${_GMXINTERNAL_TEST-unset}\" \"${GMUX_PUBLIC_TEST-unset}\" \"${USER_TEST-unset}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + binDir + ":" + strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	env = append(env,
		"_GMXINTERNAL_TEST=secret",
		"GMUX_PUBLIC_TEST=public",
		"USER_TEST=user",
	)
	return env, d
}

func TestPassthroughExecScrubsOnlyInternalEnv(t *testing.T) {
	env, _ := passthroughTestEnv(t)
	stdout, stderr, code := runGmux(t, env, "--", "pi", "--version")
	if code != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "internal=unset\npublic=public\nuser=user\n" {
		t.Fatalf("passthrough environment = %q", stdout)
	}
}

func TestDetachedPassthroughRejectedBeforeHandshake(t *testing.T) {
	env, daemon := passthroughTestEnv(t)
	stdout, stderr, code := runGmux(t, env, "-d", "--", "pi", "--version")
	if code == 0 || stdout != "" || !strings.Contains(stderr, "-d/--detach cannot be used with one-shot passthrough commands") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got := daemon.registered.Load(); got != 0 {
		t.Fatalf("detached passthrough created %d registrations", got)
	}
}
