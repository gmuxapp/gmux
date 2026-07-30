package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireScript(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is not available")
	}
	if !supportsRequiredScriptDialect(path) {
		t.Skip("script(1) does not support util-linux -qec")
	}
	return path
}

// supportsRequiredScriptDialect contains a normally hung tool on timeout; a
// successful capability probe assumes the executable returns without daemonizing.
func supportsRequiredScriptDialect(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-qec", "exit 23", os.DevNull)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 23
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func fakeScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequiredScriptDialectRejectsOptionError(t *testing.T) {
	path := fakeScript(t, "case \"$1\" in *e*) exit 64;; esac\nexit 0\n")
	if supportsRequiredScriptDialect(path) {
		t.Fatal("accepted script implementation that rejects -e")
	}
}

func TestRequiredScriptDialectRejectsNoExecImplementation(t *testing.T) {
	path := fakeScript(t, "exit 0\n")
	if supportsRequiredScriptDialect(path) {
		t.Fatal("accepted script implementation that never executes the command")
	}
}

func TestRequiredScriptDialectBoundsHangingImplementation(t *testing.T) {
	dir := t.TempDir()
	leaderFile := filepath.Join(dir, "leader.pid")
	childFile := filepath.Join(dir, "child.pid")
	path := fakeScript(t, "echo $$ >"+shellQuote(leaderFile)+"\n"+
		"sleep 30 & child=$!\n"+
		"echo $child >"+shellQuote(childFile)+"\n"+
		"wait\n")

	start := time.Now()
	if supportsRequiredScriptDialect(path) {
		t.Fatal("accepted hanging script implementation")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("capability probe took %v, want a bounded result", elapsed)
	}

	leader := readPIDFile(t, leaderFile)
	child := readPIDFile(t, childFile)
	// This also makes mutation failures safe: never leave the fake tool's
	// descendant behind if the production cleanup stops killing its group.
	cleanupArmed := true
	t.Cleanup(func() {
		if cleanupArmed {
			_ = syscall.Kill(-leader, syscall.SIGKILL)
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for _, proc := range []struct {
		name string
		pid  int
	}{{"leader", leader}, {"child", child}, {"process group", -leader}} {
		for {
			err := syscall.Kill(proc.pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if err != nil {
				t.Fatalf("probe %s existence: %v", proc.name, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("probe %s %d still exists after timeout cleanup", proc.name, proc.pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// All recorded process identities are gone. Do not signal numeric IDs that
	// the OS might reuse before test cleanup runs.
	cleanupArmed = false
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read probe pid %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid probe pid %q in %s", raw, path)
	}
	return pid
}

func TestRequiredScriptDialectAcceptsInstalledUtilLinux(t *testing.T) {
	path, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is not available")
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil || !strings.Contains(strings.ToLower(string(out)), "util-linux") {
		t.Skip("installed script(1) is not util-linux")
	}
	if !supportsRequiredScriptDialect(path) {
		t.Fatal("util-linux script failed the required -qec capability probe")
	}
}

func TestRequireScriptWiringSkipsIncompatibleDialect(t *testing.T) {
	fake := fakeScript(t, "exit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTTYStdinWithPipedStdoutUsesHeadlessRelay$", "-test.v")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PATH=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(fake))
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("wiring subprocess timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("wiring subprocess failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- SKIP: TestTTYStdinWithPipedStdoutUsesHeadlessRelay") {
		t.Fatalf("wiring subprocess did not skip incompatible script:\n%s", out)
	}
}

func TestTTYStdinWithPipedStdoutUsesHeadlessRelay(t *testing.T) {
	script := requireScript(t)
	bin := buildGmuxBinary(t)
	capture := filepath.Join(t.TempDir(), "stdout")
	command := shellQuote(bin) + " -- sh -c " + shellQuote("printf '\\033[31mred\\033[0m\\n'") + " >" + shellQuote(capture)
	cmd := exec.Command(script, "-qec", command, os.DevNull)
	cmd.Env = outputContractEnv(t)
	var transcript strings.Builder
	cmd.Stdout, cmd.Stderr = &transcript, &transcript
	if err := cmd.Run(); err != nil {
		t.Fatalf("script probe: %v\ntranscript: %q", err, transcript.String())
	}
	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "red\n" {
		t.Fatalf("piped stdout=%q, want stripped child output", got)
	}
	if strings.Contains(string(got), "\x1b") {
		t.Fatalf("piped stdout contains terminal escapes: %q", got)
	}
}

func TestEditUsesControllingTerminalWhenStdoutIsRedirected(t *testing.T) {
	script := requireScript(t)
	bin := buildGmuxBinary(t)
	dir := t.TempDir()
	editor := filepath.Join(dir, "editor")
	content := "#!/bin/sh\nIFS= read -r key\nprintf '%s' \"$key\" > \"$1\"\nprintf '\\033[31meditor-ui\\033[0m\\n'\nexit 7\n"
	if err := os.WriteFile(editor, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "edited")
	capture := filepath.Join(dir, "stdout")
	command := shellQuote(bin) + " edit " + shellQuote(file) + " >" + shellQuote(capture)
	cmd := exec.Command(script, "-qec", command, os.DevNull)
	cmd.Env = append(outputContractEnv(t), "GMUX_EDIT_FALLBACK="+editor)
	cmd.Stdin = strings.NewReader("typed\n")
	var transcript strings.Builder
	cmd.Stdout, cmd.Stderr = &transcript, &transcript
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("exit error=%v, want editor exit 7\ntranscript: %q", err, transcript.String())
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("editor did not receive terminal input: %v\ntranscript: %q", err, transcript.String())
	}
	if string(got) != "typed" {
		t.Errorf("edited content=%q, want typed", got)
	}
	redirected, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if len(redirected) != 0 {
		t.Errorf("redirected stdout=%q, want editor UI only on /dev/tty", redirected)
	}
	if !strings.Contains(transcript.String(), "editor-ui") {
		t.Errorf("terminal transcript=%q, want editor UI", transcript.String())
	}
}

func TestEditWithoutControllingTerminalFailsBeforeLaunch(t *testing.T) {
	bin := buildGmuxBinary(t)
	cmd := exec.Command(bin, "edit", filepath.Join(t.TempDir(), "file"))
	cmd.Env = outputContractEnv(t)
	cmd.Stdin = strings.NewReader("")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("edit unexpectedly succeeded without a controlling terminal")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires a controlling terminal") {
		t.Fatalf("stdout=%q stderr=%q, want controlling-terminal error", stdout.String(), stderr.String())
	}
}
