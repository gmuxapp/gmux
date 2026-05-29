package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFreshLoginEnvOnLaunch proves the ADR-0006 behavior end-to-end:
// the daemon sources a fresh login environment when it launches and
// restarts sessions, so an edit to the user's "dotfile" is reflected on
// the next launch/restart without restarting the daemon.
func TestFreshLoginEnvOnLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	repoRoot := findRepoRoot(t)

	// Build gmuxd and gmux into the SAME directory so the daemon's
	// resolveGmux() finds gmux as a sibling and can run the
	// `gmux --dump-env` probe.
	binDir := t.TempDir()
	buildInto(t, repoRoot, "services/gmuxd/cmd/gmuxd", filepath.Join(binDir, "gmuxd"))
	buildInto(t, repoRoot, "cli/gmux/cmd/gmux", filepath.Join(binDir, "gmux"))
	gmuxdBin := filepath.Join(binDir, "gmuxd")

	socketDir := t.TempDir()
	stateDir := t.TempDir()
	configDir := t.TempDir()
	cfgDir := filepath.Join(configDir, "gmux")
	os.MkdirAll(cfgDir, 0o755)
	port := freePort(t)
	os.WriteFile(filepath.Join(cfgDir, "host.toml"),
		[]byte(fmt.Sprintf("port = %d\n", port)), 0o644)
	gmuxdSock := filepath.Join(stateDir, "gmux", "gmuxd.sock")

	// Fake $SHELL: sources a sentinel "dotfile", then runs the -c
	// command (gmux --dump-env). Editing the dotfile between launches
	// simulates the user editing ~/.zshrc.
	dotfile := filepath.Join(t.TempDir(), "dotfile")
	os.WriteFile(dotfile, []byte("export SENTINEL=before-edit\n"), 0o644)
	fakeShell := filepath.Join(t.TempDir(), "fakeshell")
	os.WriteFile(fakeShell, []byte(
		"#!/bin/sh\n. "+dotfile+" 2>/dev/null\nshift 3\nexec sh -c \"$1\"\n"), 0o755)

	outFile := filepath.Join(t.TempDir(), "sentinel.out")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gmuxd := exec.CommandContext(ctx, gmuxdBin, "run")
	home := t.TempDir()
	gmuxd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+configDir,
		"XDG_STATE_HOME="+stateDir,
		"GMUX_SOCKET_DIR="+socketDir,
		"SHELL="+fakeShell,
	)
	gmuxd.Stdout, gmuxd.Stderr = os.Stdout, os.Stderr
	if err := gmuxd.Start(); err != nil {
		t.Fatalf("start gmuxd: %v", err)
	}
	defer gmuxd.Process.Kill()
	waitForSocket(t, gmuxdSock, 20*time.Second)

	launch := func() {
		t.Helper()
		os.Remove(outFile)
		body := fmt.Sprintf(`{"command":["sh","-c","echo SENTINEL=$SENTINEL > %s; sleep 300"],"cwd":%q}`,
			outFile, repoRoot)
		client := unixHTTPClient(gmuxdSock)
		resp, err := client.Post("http://localhost/v1/launch", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /v1/launch: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("POST /v1/launch: status %d", resp.StatusCode)
		}
	}

	waitForSentinel := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(outFile); err == nil {
				last = strings.TrimSpace(string(b))
				if last == "SENTINEL="+want {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("sentinel = %q, want SENTINEL=%s", last, want)
	}

	// 1) Initial launch sees the original dotfile value.
	launch()
	waitForSentinel("before-edit")

	// 2) Edit the dotfile (no daemon restart), then Restart the session.
	os.WriteFile(dotfile, []byte("export SENTINEL=after-edit\n"), 0o644)

	var sid string
	for _, s := range listSessions(t, gmuxdSock) {
		if s.Alive {
			sid = s.ID
			break
		}
	}
	if sid == "" {
		t.Fatal("no alive session to restart")
	}

	client := unixHTTPClient(gmuxdSock)
	resp, err := client.Post("http://localhost/v1/sessions/"+sid+"/restart", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST restart: status %d", resp.StatusCode)
	}

	// The restarted session must reflect the edited dotfile.
	waitForSentinel("after-edit")
}

func buildInto(t *testing.T, repoRoot, pkg, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, "./"+pkg)
	cmd.Dir = repoRoot
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", pkg, err)
	}
}
