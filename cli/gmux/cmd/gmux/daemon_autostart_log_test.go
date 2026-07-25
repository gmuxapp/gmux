package main

// The CLI is the daemon's primary launcher: almost every gmuxd on a developer
// machine is autostarted by a `gmux` invocation, not by `gmuxd start`. Where
// that daemon's stderr points therefore decides whether the daemon's own log
// bounding attaches at all -- it only bounds a file it can confirm is the log
// at paths.DaemonLogPath -- and whether `gmuxd log-path` tells the truth.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
)

// Mutation: point autostartLogFile back at $TMPDIR/gmuxd.log, or give it
// O_TRUNC.
func TestAutostartLogUsesTheDaemonLogPath(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // a TempDir the log must NOT land in

	logPath := paths.DaemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	incumbent := "an incumbent daemon's history\n"
	if err := os.WriteFile(logPath, []byte(incumbent), 0o600); err != nil {
		t.Fatal(err)
	}

	f := autostartLogFile()
	if f == nil {
		t.Fatal("autostartLogFile returned nothing")
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pi, err := os.Lstat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, pi) {
		t.Fatalf("autostart opened %v, want the daemon log at %s", fi.Name(), logPath)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "gmuxd.log")); err == nil {
		t.Fatal("autostart still creates a second log in TMPDIR")
	}
	// An incumbent's history must survive: this runs before the child has
	// checked whether one is already healthy.
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != incumbent {
		t.Fatalf("autostart truncated the log; content = %q", content)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %o, want 600", fi.Mode().Perm())
	}
}

// The end-to-end version: a real `gmux` invocation with no daemon running
// autostarts one, and that daemon's stderr is the file `gmuxd log-path`
// names -- which is the precondition for the daemon bounding it.
//
// The daemon here is a stub: this pins where the CLI points a child's stderr,
// which is the CLI's half of the contract. The daemon's half (bounding
// exactly that file) is pinned in services/gmuxd.
func TestAutostartedDaemonInheritsTheDaemonLog(t *testing.T) {
	bin := buildGmuxBinary(t)
	stateHome := t.TempDir()
	socketDir := filepath.Join(t.TempDir(), "sessions")
	logPath := filepath.Join(stateHome, "gmux", "gmuxd.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// A pre-existing log, as a healthy incumbent would have.
	if err := os.WriteFile(logPath, []byte("earlier daemon output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A stub gmuxd next to the gmux binary, which findGmuxdBin prefers.
	stub := filepath.Join(filepath.Dir(bin), "gmuxd")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho AUTOSTARTED-DAEMON-STDERR >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(stub) })

	childTmp := t.TempDir()
	cmd := exec.Command(bin, "--", "true")
	cmd.Env = append(launchEnv(stateHome, socketDir), "TMPDIR="+childTmp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gmux -- true: %v\n%s", err, out)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		content, readErr := os.ReadFile(logPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(content), "AUTOSTARTED-DAEMON-STDERR") {
			if !strings.Contains(string(content), "earlier daemon output") {
				t.Fatal("the autostart truncated an existing daemon log")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the autostarted daemon's stderr never reached %s; content = %q", logPath, content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Lstat(filepath.Join(childTmp, "gmuxd.log")); err == nil {
		t.Fatal("the autostarted daemon logged to TMPDIR as well")
	}
}
