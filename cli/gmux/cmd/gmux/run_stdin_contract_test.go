package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

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

func assertStdinContractRun(t *testing.T, stdin io.Reader, wantNotice bool) {
	t.Helper()
	stdout, stderr, code := runGmuxWithStdin(t, outputContractEnv(t), stdin, "--", "sh", "-c",
		"stty -icanon min 0 time 1; data=$(dd bs=1 count=16 2>/dev/null); printf 'out:%s\\n' \"$data\"; exit 7")
	if code != 7 {
		t.Fatalf("exit=%d, want 7\nstdout: %q\nstderr: %q", code, stdout, stderr)
	}
	if stdout != "out:\n" {
		t.Errorf("stdout=%q, want child output with no forwarded input", stdout)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) == 0 || !paths.IsValidSessionID(lines[0]) {
		t.Fatalf("stderr=%q, want session id first", stderr)
	}
	hasNotice := strings.Contains(stderr, "stdin is not forwarded into the session; use 'gmux send "+lines[0]+"'.")
	if hasNotice != wantNotice {
		t.Errorf("notice=%v, want %v; stderr=%q", hasNotice, wantNotice, stderr)
	}
}

func TestPrefilledPipeStdinWarnsAndIsNotForwarded(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("input\\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	defer r.Close()
	assertStdinContractRun(t, r, true)
}

func TestEmptyPipeStdinDoesNotWarn(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	assertStdinContractRun(t, r, false)
}

func TestDevNullStdinDoesNotWarn(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	assertStdinContractRun(t, f, false)
}

func TestRegularFileStdinWarns(t *testing.T) {
	assertStdinContractRun(t, regularInputFile(t, "input\\n"), true)
}
