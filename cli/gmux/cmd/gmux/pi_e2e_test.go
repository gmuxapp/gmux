package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/agentext"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// TestPiCommandsWithExtensionInjected runs a matrix of real `pi` invocations
// exactly as gmux would launch them — passthrough commands run as-is, session
// commands get `-e <ext>` injected — against an isolated, model-less pi config.
//
// The goal is not to test pi, but to prove gmux's launch decision never
// corrupts pi's argument parsing: a subcommand must never be demoted to a chat
// prompt, `-e`/the extension path must never be treated as the prompt, and no
// invocation may produce an "Unknown option/command" parse error. Failures from
// having no model configured are expected and fine; parse failures are not.
//
// Requires the `pi` binary; skips otherwise (e.g. in CI, where pi isn't
// installed). Skipped under -short.
func TestPiCommandsWithExtensionInjected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-pi integration test in -short mode")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi binary not on PATH; skipping real-pi integration test")
	}

	extPath, err := agentext.Path()
	if err != nil {
		t.Fatalf("materialize extension: %v", err)
	}
	pi := adapters.NewPi()
	version := regexp.MustCompile(`\d+\.\d+\.\d+`)

	// reachedModel reports whether pi got past arg parsing and tried to start
	// the agent loop without a model — pi then prints a runtime "/login" hint.
	// We match that distinctive runtime instruction rather than "provider" or
	// "api key", which also appear in `--help`'s static provider list. Its
	// presence means the agent loop started; its absence (for a subcommand or
	// info flag) means the command ran without touching the model — the signal
	// that distinguishes "ran the subcommand" from "demoted it to a prompt".
	reachedModel := func(out string) bool {
		l := strings.ToLower(out)
		return strings.Contains(l, "/login") || strings.Contains(l, "log into a provider")
	}

	cases := []struct {
		name string
		// user is what the user typed after `gmux --` (args[0] == "pi").
		user []string
		// wantPassthrough is gmux's expected classification for `user`.
		wantPassthrough bool
		// wantContains, if set, must appear in combined output.
		wantContains string
		// agentLaunch marks a real session launch: it should reach the model
		// stage (no model → fails there), proving the whole arg chain parsed.
		agentLaunch bool
	}{
		{name: "version", user: []string{"pi", "--version"}, wantPassthrough: true, wantContains: ""},
		{name: "help", user: []string{"pi", "--help"}, wantPassthrough: true, wantContains: "Usage:"},
		{name: "update help", user: []string{"pi", "update", "--help"}, wantPassthrough: true, wantContains: "update [source"},
		{name: "list subcommand", user: []string{"pi", "list"}, wantPassthrough: true, wantContains: "package"},
		// Session-form invocations that gmux extends with -e. The info flags
		// short-circuit even with -e present, so they parse cleanly and never
		// reach the model — the cleanest proof that injection is harmless.
		{name: "version with -e", user: []string{"pi", "--version"}, wantPassthrough: true},
		{name: "agent launch with -e", user: []string{"pi", "-p", "ping"}, wantPassthrough: false, agentLaunch: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror run.go's launch decision exactly.
			if got := pi.IsPassthrough(tc.user); got != tc.wantPassthrough {
				t.Fatalf("IsPassthrough(%v) = %v, want %v", tc.user, got, tc.wantPassthrough)
			}
			argv := tc.user
			if !pi.IsPassthrough(tc.user) {
				// gmux splices the extension in right after the pi binary.
				argv = pi.ExtendCommand(tc.user, extPath)
				if argv[1] != "-e" || argv[2] != extPath {
					t.Fatalf("expected -e injected after binary, got %v", argv)
				}
			}

			out, exit := runPi(t, argv)
			t.Logf("argv=%v exit=%d\noutput:\n%s", argv, exit, out)

			// Universal: never a parse error.
			for _, bad := range []string{"Unknown option", "Unknown command"} {
				if strings.Contains(out, bad) {
					t.Errorf("parse error %q for %v:\n%s", bad, argv, out)
				}
			}
			// -e / the extension path must never be echoed back as the prompt.
			if strings.Contains(out, "-e "+extPath) || strings.Contains(out, "Prompt: "+extPath) {
				t.Errorf("extension flag/path leaked into prompt for %v:\n%s", argv, out)
			}
			if tc.wantContains != "" && !strings.Contains(out, tc.wantContains) {
				t.Errorf("output for %v missing %q:\n%s", argv, tc.wantContains, out)
			}
			switch {
			case tc.agentLaunch:
				// A real launch must reach the model stage (proving -p's prompt
				// parsed and the agent loop started), not die in arg parsing.
				if !reachedModel(out) {
					t.Errorf("agent launch %v did not reach the model stage; output:\n%s", argv, out)
				}
			case tc.wantPassthrough:
				// A passthrough command must run without touching the model.
				// If it had been demoted to a prompt, it would reach the model.
				if reachedModel(out) {
					t.Errorf("passthrough %v reached the model stage (demoted to a prompt?):\n%s", argv, out)
				}
			}
			if tc.name == "version" || tc.name == "version with -e" {
				if !version.MatchString(out) {
					t.Errorf("expected a version string for %v, got:\n%s", argv, out)
				}
			}
		})
	}
}

// runPi runs pi with a minimal, isolated environment (fresh HOME, no inherited
// API keys) so it parses arguments and then fails fast at model resolution.
// Returns combined stdout+stderr and the exit code.
func runPi(t *testing.T, argv []string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"NO_COLOR=1",
	}
	// The isolated HOME breaks proto-shimmed runtimes: pi's `#!/usr/bin/env
	// node` can resolve to ~/.proto/shims/node (moon prepends proto to PATH),
	// and the shim locates its toolchain via $PROTO_HOME or $HOME/.proto —
	// with a fresh HOME it blocks trying to bootstrap a toolchain and every
	// subtest times out. Point PROTO_HOME at the real one so the shim works.
	if ph := os.Getenv("PROTO_HOME"); ph != "" {
		cmd.Env = append(cmd.Env, "PROTO_HOME="+ph)
	} else if realHome, err := os.UserHomeDir(); err == nil {
		cmd.Env = append(cmd.Env, "PROTO_HOME="+filepath.Join(realHome, ".proto"))
	}
	// Stdin nil → /dev/null, so any interactive prompt gets EOF instead of
	// hanging the test.
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("pi %v timed out (possible hang on a corrupted invocation)", argv)
	}
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run pi %v: %v", argv, err)
	}
	return string(out), exit
}
