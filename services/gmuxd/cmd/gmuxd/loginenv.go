package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// loginEnvTimeout bounds how long captureLoginEnv waits for the probe
// shell to source the user's dotfiles and dump its environment. Heavy
// rc setups (nvm, mise, conda, prompt frameworks) can take a second or
// two; 5s is generous enough that a slow-but-valid shell completes
// rather than falsely timing out into the stale-env fallback. A hung
// rc file is bounded by the process-group kill on expiry.
const loginEnvTimeout = 5 * time.Second

// captureLoginEnv returns a freshly-sourced login environment for a
// session about to be launched in cwd, by running
//
//	$SHELL -l -i -c '<gmuxBin> --dump-env'
//
// in that cwd and reading the NUL-delimited environment the probe
// writes to fd 3. The interactive login shell sources the user's
// profile *and* rc files (~/.zshrc / ~/.bashrc), so dotfile changes —
// and the Restart button — take effect without a daemon restart. The
// probe shell inherits the daemon's environment as its base, so this is
// a merge (daemon env ∪ rc-file changes), preserving session/PAM vars
// like DISPLAY and SSH_AUTH_SOCK. See ADR 0006.
//
// On any failure (shell unset, probe binary unresolved, non-zero exit,
// empty read, timeout) it falls back to os.Environ() and logs a
// warning, so a failed refresh never produces a worse environment than
// today nor blocks a launch indefinitely. Callers still run the result
// through sessionenv.Strip.
func captureLoginEnv(gmuxBin, cwd string) []string {
	return captureLoginEnvTimeout(gmuxBin, cwd, loginEnvTimeout)
}

// captureLoginEnvTimeout is captureLoginEnv with an injectable timeout
// for tests.
func captureLoginEnvTimeout(gmuxBin, cwd string, timeout time.Duration) []string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		// No login shell to source (typical for systemd/Docker
		// daemons). Skip rather than guess /bin/sh -l -i, which could
		// hang or surprise. Inherit the daemon env unchanged — exactly
		// today's behavior.
		return os.Environ()
	}
	if gmuxBin == "" {
		log.Printf("loginenv: gmux binary unresolved; using daemon env")
		return os.Environ()
	}

	env, err := runLoginEnvProbe(shell, gmuxBin, cwd, timeout)
	if err != nil {
		log.Printf("loginenv: capture via %s failed (%v); using daemon env", shell, err)
		return os.Environ()
	}
	if len(env) == 0 {
		log.Printf("loginenv: capture via %s produced no variables; using daemon env", shell)
		return os.Environ()
	}
	return env
}

// runLoginEnvProbe runs the probe shell and returns the parsed
// environment, or an error. The probe's stdout/stderr are discarded
// (rc-file banners land there); only fd 3 carries the payload.
func runLoginEnvProbe(shell, gmuxBin, cwd string, timeout time.Duration) ([]string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pipe: %w", err)
	}
	defer pr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// -l (login) sources the profile files; -i (interactive) sources
	// ~/.zshrc / ~/.bashrc, where most users' PATH/exports/tool-shims
	// live. shellQuote guards a gmuxBin path containing spaces.
	cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c", shellQuote(gmuxBin)+" --dump-env")
	cmd.Dir = cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.ExtraFiles = []*os.File{pw} // child sees this as fd 3
	// Own process group so a timeout kill reaches rc-spawned children
	// too, and so a child holding the pipe open can't wedge the read.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// On ctx timeout, kill the whole process group (negative pid), not
	// just the direct shell. An rc-spawned descendant (e.g. a lingering
	// `sleep`) would otherwise keep the fd-3 pipe open and wedge the
	// read below forever. Matches ptyserver.handleKill's group signal.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	if err := cmd.Start(); err != nil {
		pw.Close()
		return nil, fmt.Errorf("start %s: %w", shell, err)
	}
	// The child owns the write end now; close the parent's copy so the
	// read below sees EOF once the child exits.
	pw.Close()

	// Read concurrently with Wait so a child that writes a lot can't
	// deadlock against a full pipe while we block in Wait.
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(pr)
		readCh <- readResult{data, err}
	}()

	waitErr := cmd.Wait()

	// Wait for the reader, but don't trust it to finish just because the
	// shell exited. A process the rc files spawned in the background
	// (e.g. a daemon started with `&`) can inherit fd 3 and keep the
	// pipe's write end open after the shell is gone, so io.ReadAll never
	// sees EOF. By this point cmd.Wait has returned, so Go's context
	// watcher has stopped and cmd.Cancel will not fire again — we must
	// bail out ourselves: close the read end to unblock io.ReadAll and
	// signal the lingering process group (still reachable by pgid while
	// a member is alive).
	var rr readResult
	select {
	case rr = <-readCh:
	case <-ctx.Done():
		pr.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-readCh // io.ReadAll returns now that pr is closed; don't leak it
		return nil, fmt.Errorf("timed out after %s", timeout)
	}

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("timed out after %s", timeout)
	}
	if rr.err != nil {
		return nil, fmt.Errorf("read fd 3: %w", rr.err)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("%s exited: %w", shell, waitErr)
	}
	return parseNulEnv(rr.data), nil
}

// parseNulEnv splits a NUL-delimited environment dump into KEY=VALUE
// entries. A trailing NUL (the dumper terminates every entry) yields no
// empty final element; any empty segments are skipped defensively.
func parseNulEnv(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, string(p))
	}
	return out
}

// shellQuote wraps s in single quotes for safe inclusion in a `sh -c`
// command string, escaping any embedded single quotes. Sufficient for
// the one value we interpolate (the resolved gmux binary path).
func shellQuote(s string) string {
	return "'" + escapeSingleQuotes(s) + "'"
}

func escapeSingleQuotes(s string) string {
	out := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}
