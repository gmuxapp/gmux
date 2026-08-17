package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReservedDetachedLaunchHelper(t *testing.T) {
	if os.Getenv("GMUX_TEST_RESERVED_LAUNCH_HELPER") != "1" {
		return
	}
	control := os.NewFile(3, "control")
	gate := os.NewFile(4, "gate")
	hold := os.NewFile(5, "hold")
	if control == nil || gate == nil || hold == nil {
		os.Exit(90)
	}
	if err := os.WriteFile(os.Getenv("GMUX_TEST_RESERVED_LAUNCH_TOKEN_FILE"), []byte(os.Getenv(activeSubagentReservationEnv)), 0o600); err != nil {
		os.Exit(91)
	}
	_, _ = fmt.Fprintf(control, "TARGET %d\n1va8lvdv\n", os.Getpid())
	_ = control.Close()
	var token [1]byte
	if _, err := io.ReadFull(gate, token[:]); err != nil || token[0] != 'G' {
		os.Exit(92)
	}
	_, _ = io.Copy(io.Discard, hold)
	_ = gate.Close()
	_ = hold.Close()
	os.Exit(0)
}

func TestLaunchDetachedSessionReservedCarriesReceiptBeforeProcessStart(t *testing.T) {
	old := detachedLaunchCommand
	t.Cleanup(func() { detachedLaunchCommand = old })
	t.Setenv("GMUX_TEST_RESERVED_LAUNCH_HELPER", "1")
	tokenFile := filepath.Join(t.TempDir(), "token")
	t.Setenv("GMUX_TEST_RESERVED_LAUNCH_TOKEN_FILE", tokenFile)
	detachedLaunchCommand = func([]string) (*exec.Cmd, *os.File, error) {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			return nil, nil, err
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestReservedDetachedLaunchHelper$")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
		return cmd, devNull, nil
	}
	id, err := launchDetachedSessionReserved([]string{"pi"}, "receipt-123")
	if err != nil || id != "1va8lvdv" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil || string(raw) != "receipt-123" {
		t.Fatalf("token=%q err=%v", raw, err)
	}
}

func TestTakeActiveSubagentReservationDoesNotLeakToAgentEnvironment(t *testing.T) {
	t.Setenv(activeSubagentReservationEnv, "receipt")
	if got := takeActiveSubagentReservation(os.Getenv, os.Unsetenv); got != "receipt" {
		t.Fatalf("token=%q", got)
	}
	if _, present := os.LookupEnv(activeSubagentReservationEnv); present {
		t.Fatal("receipt remained in process environment")
	}
}

func stubAgentLaunch(t *testing.T, id string, err error) *[]string {
	t.Helper()
	var got []string
	oldLaunch, oldReserve, oldRelease := agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent
	agentReserveActiveSubagent = func(string) (string, error) { return "receipt", nil }
	agentReleaseActiveSubagent = func(string) {}
	agentLaunchSession = func(argv []string, token string) (string, error) {
		if token != "receipt" {
			t.Fatalf("reservation = %q", token)
		}
		got = argv
		return id, err
	}
	t.Cleanup(func() {
		agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent = oldLaunch, oldReserve, oldRelease
	})
	return &got
}

func TestAgentPromptNewAdmissionAPIAndExitCode(t *testing.T) {
	t.Run("structured limit rejection happens before spawn", func(t *testing.T) {
		d := startStubDaemon(t, nil)
		d.on(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/agent-launch-reservations" {
				t.Fatalf("path = %s", r.URL.Path)
			}
			writeErrEnvelope(w, http.StatusTooManyRequests, "subagent_limit_reached", "subagent limit reached at depth 2 for root root: 8 of 8 autonomous subagents at this depth; run 'gmux ls' to see who holds the slots")
		})
		oldLaunch, oldReserve, oldRelease := agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent
		agentReserveActiveSubagent, agentReleaseActiveSubagent = reserveActiveSubagent, releaseActiveSubagent
		launched := false
		agentLaunchSession = func([]string, string) (string, error) { launched = true; return "", nil }
		t.Cleanup(func() {
			agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent = oldLaunch, oldReserve, oldRelease
		})
		t.Setenv("GMUX_SESSION_ID", "root")
		text := "go"
		var code int
		stderr := captureStderr(t, func() { code = cmdAgentPromptNew("", "", true, 0, &text) })
		if code != waitExitError || launched {
			t.Fatalf("exit=%d launched=%v", code, launched)
		}
		if !strings.Contains(stderr, "subagent_limit_reached: subagent limit reached at depth 2 for root root: 8 of 8 autonomous subagents") || !strings.Contains(stderr, "gmux ls") {
			t.Fatalf("stderr = %q", stderr)
		}
	})

	t.Run("allowed receipt reaches launch and is released", func(t *testing.T) {
		d := startStubDaemon(t, nil)
		deleted := false
		d.on(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/v1/agent-launch-reservations":
				writeEnvelope(w, http.StatusOK, map[string]any{"token": "receipt", "limit": 8})
			case r.Method == http.MethodDelete && r.URL.Path == "/v1/agent-launch-reservations/receipt":
				deleted = true
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt"):
				writeEnvelope(w, http.StatusAccepted, map[string]any{"admission": "accepted"})
			default:
				t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
			}
		})
		oldLaunch, oldReserve, oldRelease := agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent
		agentReserveActiveSubagent, agentReleaseActiveSubagent = reserveActiveSubagent, releaseActiveSubagent
		gotToken := ""
		agentLaunchSession = func(_ []string, token string) (string, error) { gotToken = token; return "1va8lvdv", nil }
		t.Cleanup(func() {
			agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent = oldLaunch, oldReserve, oldRelease
		})
		text := "go"
		var code int
		captureStdout(t, func() { code = cmdAgentPromptNew("", "", true, 0, &text) })
		if code != 0 || gotToken != "receipt" || !deleted {
			t.Fatalf("exit=%d token=%q deleted=%v", code, gotToken, deleted)
		}
	})
}

func TestAgentPromptNewLaunchFailureReleasesReservation(t *testing.T) {
	oldLaunch, oldReserve, oldRelease := agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent
	agentReserveActiveSubagent = func(string) (string, error) { return "receipt", nil }
	agentLaunchSession = func([]string, string) (string, error) { return "", errors.New("startup failed") }
	released := ""
	agentReleaseActiveSubagent = func(token string) { released = token }
	t.Cleanup(func() {
		agentLaunchSession, agentReserveActiveSubagent, agentReleaseActiveSubagent = oldLaunch, oldReserve, oldRelease
	})
	text := "go"
	var code int
	captureStderr(t, func() { code = cmdAgentPromptNew("", "", false, 0, &text) })
	if code != waitExitError || released != "receipt" {
		t.Fatalf("exit=%d released=%q", code, released)
	}
}

func TestAgentPromptHelpDocumentsActiveSubagentBudget(t *testing.T) {
	var out strings.Builder
	printAgentUsage(&out, "agent prompt")
	text := out.String()
	for _, want := range []string{"unlimited direct children", "shared grandchildren", "behavioral root", "gmux ls"} {
		if !strings.Contains(text, want) {
			t.Errorf("help missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "--wait-for-slot") {
		t.Fatal("help advertises unimplemented --wait-for-slot")
	}
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
