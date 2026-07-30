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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	env, _, _ := outputContractHarness(t)
	return env
}

func outputContractHarness(t *testing.T) ([]string, *fakeDaemon, string) {
	t.Helper()
	stateHome := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "sessions")
	daemon := startFakeDaemon(t, filepath.Join(stateHome, "gmux", "gmuxd.sock"))
	return launchEnv(stateHome, socketDir), daemon, socketDir
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

// TestPipedRunStartupFailurePublishesNoID pins the pre-publication boundary:
// an id is not a session payload until the child and PTY have started.
func TestPipedRunStartupFailurePublishesNoID(t *testing.T) {
	env := outputContractEnv(t)
	stdout, stderr, code := runGmux(t, env, "--", "gmux-test-command-that-does-not-exist")
	if code == 0 {
		t.Fatalf("exit=%d, want nonzero\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want no child payload", stdout)
	}
	if !strings.Contains(stderr, "failed to start") {
		t.Errorf("stderr = %q, want startup diagnostic", stderr)
	}
	if strings.Contains(stderr, "stdin is not forwarded") {
		t.Errorf("stderr = %q, notice must not advertise an unstarted session", stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if paths.IsValidSessionID(strings.TrimSpace(line)) {
			t.Errorf("stderr = %q, must not publish a session id", stderr)
		}
	}
}

// TestDetachedRunKeepsIDOnStdout: for `gmux -d` the id is the payload, so it
// stays on stdout (the id=$(gmux -d -- …) capture shape), stderr silent.
func TestDetachedRunKeepsIDOnStdout(t *testing.T) {
	env, daemon, socketDir := outputContractHarness(t)
	pidFile := filepath.Join(t.TempDir(), "resident.pid")
	stdout, stderr, code := runGmux(t, env, "-d", "--", "sh", "-c", `echo $$ > "$1"; exec sleep 30`, "sh", pidFile)
	t.Cleanup(func() {
		// This acceptance daemon deliberately has no kill API. Always reap the
		// resident target directly, including when an assertion below fails.
		var pid int
		pidDeadline := time.Now().Add(time.Second)
		for time.Now().Before(pidDeadline) {
			pidData, err := os.ReadFile(pidFile)
			if err == nil {
				pid, err = strconv.Atoi(strings.TrimSpace(string(pidData)))
				if err == nil {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}

		if pid > 0 {
			pgid, err := syscall.Getpgid(pid)
			if err != nil && err != syscall.ESRCH {
				t.Errorf("clean up resident pid %d: obtain process group: %v", pid, err)
			}
			if err == nil {
				group := -pgid
				if err := syscall.Kill(group, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
					t.Errorf("clean up resident process group %d with TERM: %v", pgid, err)
				}
				termDeadline := time.Now().Add(250 * time.Millisecond)
				for time.Now().Before(termDeadline) && syscall.Kill(group, 0) == nil {
					time.Sleep(10 * time.Millisecond)
				}
				if syscall.Kill(group, 0) == nil {
					if err := syscall.Kill(group, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
						t.Errorf("clean up resident process group %d with KILL: %v", pgid, err)
					}
				}
				killDeadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(killDeadline) && syscall.Kill(group, 0) == nil {
					time.Sleep(10 * time.Millisecond)
				}
				if err := syscall.Kill(group, 0); err == nil {
					t.Errorf("resident process group %d still exists after KILL", pgid)
				}
			}
		}

		id := strings.TrimSpace(stdout)
		artifactDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(artifactDeadline) {
			deregistered := daemon.deregistrations()[id]
			socketGone := true
			if socketPath := daemon.registrations()[id]; socketPath != "" {
				_, err := os.Stat(socketPath)
				socketGone = os.IsNotExist(err)
			}
			if deregistered && socketGone && len(leftoverSockets(t, socketDir)) == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !daemon.deregistrations()[id] {
			t.Errorf("detached session %q did not deregister", id)
		}
		if left := leftoverSockets(t, socketDir); len(left) != 0 {
			t.Errorf("detached session %q left socket artifacts %v", id, left)
		}
	})
	if code != 0 {
		t.Fatalf("exit=%d\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	id := strings.TrimSuffix(stdout, "\n")
	if !paths.IsValidSessionID(id) || stdout != id+"\n" {
		t.Fatalf("stdout = %q, want exactly one bare session id line", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty on a successful detach", stderr)
	}
}
