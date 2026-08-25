package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
)

func runGmuxWithStdin(t *testing.T, env []string, stdin io.Reader, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(buildGmuxBinary(t), args...)
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("gmux %v: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

func assertStdinContractRun(t *testing.T, stdin io.Reader) {
	t.Helper()
	stdout, stderr, code := runGmuxWithStdin(t, outputContractEnv(t), stdin, "--", "sh", "-c",
		"stty -icanon min 0 time 1; data=$(dd bs=1 count=16 2>/dev/null); printf 'out:%s\\n' \"$data\"; exit 7")
	if code != 7 {
		t.Fatalf("exit=%d, want 7\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if stdout != "out:\n" {
		t.Errorf("stdout=%q, want child output with no forwarded input", stdout)
	}
	if !paths.IsValidSessionID(strings.TrimSpace(stderr)) {
		t.Fatalf("stderr=%q, want a bare session id", stderr)
	}
}

// assertPendingStdinRefused runs the real binary and pins the pre-session
// boundary: definite launcher input must fail before the command, runner
// socket, or daemon registration can exist.
func assertPendingStdinRefused(t *testing.T, stdin *os.File, detached bool) {
	t.Helper()
	stateHome := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "sessions")
	daemon := startFakeDaemon(t, filepath.Join(stateHome, "gmux", "gmuxd.sock"))
	env := launchEnv(stateHome, socketDir)
	marker := filepath.Join(t.TempDir(), "child-started")

	args := []string{"--", "sh", "-c", `printf started > "$1"`, "sh", marker}
	if detached {
		args = append([]string{"-d"}, args...)
	}

	// Build before starting the latency clock: this assertion measures the CLI
	// refusal, not a cold Go compilation performed by the test harness.
	bin := buildGmuxBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != exitUsage {
		t.Fatalf("error=%v, want exit %d\nstdout: %q\nstderr: %q", err, exitUsage, stdout.String(), stderr.String())
	}
	if elapsed > time.Second {
		t.Errorf("refusal took %v, want <1s", elapsed)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout=%q, want empty", stdout.String())
	}
	wantStderr := pendingStdinRefusal + "\n"
	if stderr.String() != wantStderr {
		t.Errorf("stderr=%q, want %q", stderr.String(), wantStderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("wrapped command started: marker stat error=%v", err)
	}
	if got := daemon.requests.Load(); got != 0 {
		t.Errorf("daemon requests=%d, want 0", got)
	}
	if got := daemon.registered.Load(); got != 0 {
		t.Errorf("registrations=%d, want 0", got)
	}
	if got := daemon.deregistered.Load(); got != 0 {
		t.Errorf("deregistrations=%d, want 0", got)
	}
	if left := leftoverSockets(t, socketDir); len(left) != 0 {
		t.Errorf("session socket artifacts=%v, want none", left)
	}
}

func TestPrefilledPipeStdinIsRefusedWithoutConsumption(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("input\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	defer r.Close()

	assertPendingStdinRefused(t, r, false)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "input\n" {
		t.Errorf("stdin after refusal=%q, want original bytes", got)
	}
}

func TestRegularFileStdinIsRefusedWithoutConsumption(t *testing.T) {
	f := regularInputFile(t, "input\n")
	assertPendingStdinRefused(t, f, false)
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 0 {
		t.Errorf("stdin offset after refusal=%d, want 0", offset)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "input\n" {
		t.Errorf("stdin after refusal=%q, want original bytes", got)
	}
}

func TestDetachedPrefilledStdinIsRefusedBeforeReexec(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("input\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	defer r.Close()

	assertPendingStdinRefused(t, r, true)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "input\n" {
		t.Errorf("stdin after refusal=%q, want original bytes", got)
	}
}

func TestEmptyPipeStdinRemainsAccepted(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	assertStdinContractRun(t, r)
}

func TestDevNullStdinRemainsAccepted(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	assertStdinContractRun(t, f)
}
