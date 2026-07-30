package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func stubAgentLaunch(t *testing.T, id string, err error) *[]string {
	t.Helper()
	var got []string
	old := agentLaunchSession
	agentLaunchSession = func(argv []string) (string, error) { got = argv; return id, err }
	t.Cleanup(func() { agentLaunchSession = old })
	return &got
}

func TestAgentPromptNewOutputAndOrphanContracts(t *testing.T) {
	t.Run("no wait prints bare id", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(w, http.StatusAccepted, map[string]any{"admission": "accepted"})
		})
		stubAgentLaunch(t, "1va8lvdv", nil)
		text := "go"
		var code int
		stdout := captureStdout(t, func() { code = cmdAgentPromptNew("", "", true, 0, &text) })
		if code != 0 || stdout != "1va8lvdv\n" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("sync stdout is the report alone, id on stderr", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(w, http.StatusOK, map[string]any{"admission": "accepted", "outcome": "completed",
				"exchanges": []map[string]any{{"ordinal": 1, "user": "go", "iterations": 1}}, "output": "done"})
		})
		stubAgentLaunch(t, "1va8lvdv", nil)
		text := "go"
		var code int
		var stderr string
		stdout := captureStdout(t, func() {
			stderr = captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
		})
		if code != 0 || !strings.HasPrefix(stdout, "[USER]: go") || !strings.Contains(stdout, "[AGENT]: done") {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if stderr != "1va8lvdv\n" {
			t.Fatalf("stderr=%q, want bare id line", stderr)
		}
	})

	t.Run("failure after spawn still prints id on stderr", func(t *testing.T) {
		d := startStubDaemon(t, localSession())
		d.on(func(w http.ResponseWriter, _ *http.Request) {
			writeErrEnvelope(w, http.StatusGatewayTimeout, "admission_timeout", "not ready")
		})
		stubAgentLaunch(t, "1va8lvdv", nil)
		text := "go"
		var code int
		var stderr string
		stdout := captureStdout(t, func() {
			stderr = captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
		})
		if code != waitExitError || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
		if !strings.HasPrefix(stderr, "1va8lvdv\n") {
			t.Fatalf("stderr=%q, want the id as line 1", stderr)
		}
	})

	t.Run("spawn failure prints no id", func(t *testing.T) {
		startStubDaemon(t, localSession())
		stubAgentLaunch(t, "", errors.New("spawn failed"))
		text := "go"
		var code int
		stdout := captureStdout(t, func() { captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) }) })
		if code != waitExitError || stdout != "" {
			t.Fatalf("exit=%d stdout=%q", code, stdout)
		}
	})

	t.Run("bad prompt is rejected before spawn", func(t *testing.T) {
		startStubDaemon(t, localSession())
		argv := stubAgentLaunch(t, "1va8lvdv", nil)
		empty := "   "
		captureStderr(t, func() { _ = cmdAgentPromptNew("", "", false, 0, &empty) })
		if *argv != nil {
			t.Fatalf("spawned invalid prompt: %q", *argv)
		}
	})
}
