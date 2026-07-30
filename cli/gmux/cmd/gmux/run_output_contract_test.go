package main

// run_output_contract_test.go — acceptance for the CLI output-channel rule
// (ADR 0028 amendment): in the non-interactive `gmux -- <cmd>` flow, stdout
// carries exactly the payload the command was asked to produce — the child's
// output, ANSI-stripped and CRLF-normalised — while the session id goes to
// stderr. `gmux -d` keeps the id on stdout because the id IS its payload.
//
// Like run_socket_lifecycle_test.go, these tests run the real binary: the
// contract lives in runSession, a func that ends in os.Exit.

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/packages/paths"
)

// runGmux runs the built gmux binary with the given args in an isolated
// environment and returns stdout, stderr, and the exit code.
func runGmux(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	bin := buildGmuxBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("gmux %v: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func outputContractEnv(t *testing.T) []string {
	t.Helper()
	stateHome := t.TempDir()
	socketDir := t.TempDir() + "/sessions"
	startFakeDaemon(t, stateHome+"/gmux/gmuxd.sock")
	return launchEnv(stateHome, socketDir)
}

// TestPipedRunStdoutIsChildOutput pins the payload rule for the piped flow:
// stdout is the child's output — escapes stripped, CRLF normalised — and
// nothing else; the session id is a bare line on stderr.
func TestPipedRunStdoutIsChildOutput(t *testing.T) {
	env := outputContractEnv(t)
	stdout, stderr, code := runGmux(t, env, "--",
		"sh", "-c", `printf '\033[31mred\033[0m one\ntwo\r\n\033]0;title\007three\n'`)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if stdout != "red one\ntwo\nthree\n" {
		t.Errorf("stdout = %q, want the child's plain output alone", stdout)
	}
	id := strings.TrimSpace(stderr)
	if !paths.IsValidSessionID(id) {
		t.Errorf("stderr = %q, want a bare session id line", stderr)
	}
}

// TestPipedRunPropagatesExitCode: a failing child fails the gmux invocation,
// with no payload invented on stdout.
func TestPipedRunPropagatesExitCode(t *testing.T) {
	env := outputContractEnv(t)
	stdout, stderr, code := runGmux(t, env, "--", "false")
	if code != 1 {
		t.Fatalf("exit=%d, want 1\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty for a silent child", stdout)
	}
	if !paths.IsValidSessionID(strings.TrimSpace(stderr)) {
		t.Errorf("stderr = %q, want a bare session id line", stderr)
	}
}

// TestDetachedRunKeepsIDOnStdout: for `gmux -d` the id is the payload, so it
// stays on stdout (the id=$(gmux -d -- …) capture shape), stderr silent.
func TestDetachedRunKeepsIDOnStdout(t *testing.T) {
	env := outputContractEnv(t)
	stdout, stderr, code := runGmux(t, env, "-d", "--", "sleep", "0.1")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if !paths.IsValidSessionID(strings.TrimSpace(stdout)) {
		t.Errorf("stdout = %q, want the bare session id", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on a successful detach", stderr)
	}
}
