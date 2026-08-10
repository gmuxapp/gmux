package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func waitTestSessions() []cliSession {
	return []cliSession{
		{ID: "aaaaaaaa", Adapter: "pi", Alive: true, Slug: "alpha"},
		{ID: "bbbbbbbb", Adapter: "pi", Alive: true, Slug: "beta"},
	}
}

func writeWaitOutcome(w http.ResponseWriter, outcome, answer string) {
	writeEnvelope(w, http.StatusOK, map[string]any{
		"reason": "idle", "outcome": outcome,
		"exchanges": []map[string]any{{"ordinal": 1, "user": "ask " + answer}},
		"output":    answer,
	})
}

func TestWaitMultiConcurrentAndArgvOrderedHeaders(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	bArmed := make(chan struct{})
	var once sync.Once
	d.on(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "aaaaaaaa"):
			select {
			case <-bArmed:
			case <-time.After(time.Second):
				t.Error("second wait was not armed concurrently")
			}
			// B writes first; hold A long enough to force reverse settlement.
			time.Sleep(50 * time.Millisecond)
			writeWaitOutcome(w, waitOutcomeCompleted, "answer A")
		case strings.Contains(r.URL.Path, "bbbbbbbb"):
			once.Do(func() { close(bArmed) })
			writeWaitOutcome(w, waitOutcomeCompleted, "answer B")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	var code int
	stdout := captureStdout(t, func() {
		code = cmdWait([]string{"alpha", "beta"}, 0, "", "", false)
	})
	if code != waitExitOK {
		t.Fatalf("exit=%d", code)
	}
	wantA, wantB := "=== aaaaaaaa ===\n\n", "=== bbbbbbbb ===\n\n"
	if strings.Count(stdout, "=== ") != 2 || !strings.Contains(stdout, wantA) || !strings.Contains(stdout, wantB) {
		t.Fatalf("headers missing: %q", stdout)
	}
	if strings.Index(stdout, wantA) > strings.Index(stdout, wantB) || strings.Index(stdout, "answer A") > strings.Index(stdout, "answer B") {
		t.Fatalf("reports not in argv order: %q", stdout)
	}
	if !strings.Contains(stdout, "[AGENT]: answer A\n\n=== bbbbbbbb ===") {
		t.Fatalf("report blocks do not have exactly one blank separator: %q", stdout)
	}
}

func TestWaitCommandSessionsUseNeutralReportsSingleAndMulti(t *testing.T) {
	for _, refs := range [][]string{{"alpha"}, {"alpha", "beta"}} {
		t.Run(strconv.Itoa(len(refs)), func(t *testing.T) {
			sessions := waitTestSessions()
			for i := range sessions {
				sessions[i].Adapter = "shell"
			}
			d := startStubDaemon(t, sessions)
			d.on(func(w http.ResponseWriter, _ *http.Request) {
				writeEnvelope(w, http.StatusOK, map[string]any{"reason": "idle", "outcome": waitOutcomeCompleted})
			})
			var code int
			stdout := captureStdout(t, func() { code = cmdWait(refs, 0, "", "", false) })
			if code != 0 || strings.Count(stdout, "[Session activity completed]") != len(refs) {
				t.Fatalf("exit=%d stdout=%q", code, stdout)
			}
			if strings.Contains(strings.ToLower(stdout), "agent") || strings.Contains(stdout, "[No exchanges yet]") {
				t.Fatalf("command report uses agent/exchange language: %q", stdout)
			}
		})
	}
}

func TestWaitSingleKeepsHeaderlessOutput(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	d.on(func(w http.ResponseWriter, _ *http.Request) { writeWaitOutcome(w, waitOutcomeCompleted, "done") })
	var code int
	stdout := captureStdout(t, func() { code = cmdWait([]string{"alpha"}, 0, "", "", false) })
	if code != 0 || strings.Contains(stdout, "=== ") || !strings.Contains(stdout, "[AGENT]: done") {
		t.Fatalf("exit=%d stdout=%q", code, stdout)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	read := false
	for _, req := range d.requests {
		read = read || strings.HasSuffix(req.path, "/read")
	}
	if !read {
		t.Fatal("successful wait did not acknowledge unread")
	}
}

func TestWaitDelayedAcknowledgementCannotClearNewerCompletion(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions()[:1])
	currentToken := "turn-1"
	d.on(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wait") {
			writeEnvelope(w, http.StatusOK, map[string]any{
				"reason": "idle", "outcome": waitOutcomeCompleted,
				"output": "turn one", "unread_token": "turn-1",
			})
			// Deterministic interleaving: N+1 completes after wait observed N but
			// before its acknowledgement reaches /read.
			currentToken = "turn-2"
			return
		}
		if strings.HasSuffix(r.URL.Path, "/read") {
			if r.URL.Query().Get("token") == "turn-1" && currentToken == "turn-2" {
				writeErrEnvelope(w, http.StatusConflict, "result_changed", "newer result")
				return
			}
			t.Fatalf("unexpected read schedule: %s current token=%s", r.URL.String(), currentToken)
		}
	})
	if code := cmdWait([]string{"alpha"}, 0, "", "", false); code != waitExitOK {
		t.Fatalf("wait exit=%d, want observed turn success with newer result retained", code)
	}
	if currentToken != "turn-2" {
		t.Fatal("delayed wait acknowledgement cleared the newer token")
	}
}

func TestWaitMultiExitAggregation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcomes map[string]string
		want     int
	}{
		{"all completed", map[string]string{"aaaaaaaa": waitOutcomeCompleted, "bbbbbbbb": waitOutcomeCompleted}, 0},
		{"interrupted", map[string]string{"aaaaaaaa": waitOutcomeCompleted, "bbbbbbbb": waitOutcomeInterrupted}, 2},
		{"failure dominates interruption", map[string]string{"aaaaaaaa": waitOutcomeInterrupted, "bbbbbbbb": waitOutcomeError}, 1},
		{"timeout dominates interruption", map[string]string{"aaaaaaaa": waitOutcomeInterrupted, "bbbbbbbb": outcomeTimeout}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startStubDaemon(t, waitTestSessions())
			d.on(func(w http.ResponseWriter, r *http.Request) {
				for id, outcome := range tc.outcomes {
					if strings.Contains(r.URL.Path, id) {
						writeWaitOutcome(w, outcome, id)
						return
					}
				}
				t.Errorf("unexpected path %s", r.URL.Path)
			})
			var code int
			stdout := captureStdout(t, func() { code = cmdWait([]string{"alpha", "beta"}, 0, "", "", true) })
			if code != tc.want || stdout != "" {
				t.Fatalf("exit=%d want=%d stdout=%q", code, tc.want, stdout)
			}
		})
	}
}

func TestWaitMultiTimeoutIncludesResolution(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	d.onSessions(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(700 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": waitTestSessions()})
	})
	d.on(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("wait was armed after resolution consumed the invocation deadline")
		writeWaitOutcome(w, waitOutcomeCompleted, "must not arm")
	})

	started := time.Now()
	var code int
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code = cmdWait([]string{"alpha", "beta"}, 1, "", "", false)
		})
	})
	if elapsed := time.Since(started); elapsed > 1150*time.Millisecond {
		t.Fatalf("one-second timeout did not include resolution: %s", elapsed)
	}
	if code != waitExitError || stdout != "" || !strings.Contains(stderr, "wait timed out after 1s before any session was armed") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	d.mu.Lock()
	requests := len(d.requests)
	d.mu.Unlock()
	if requests != 0 {
		t.Fatalf("armed %d waits", requests)
	}
}

func TestWaitMultiReceivesAuthoritativeTimeoutWithinWholeDeadline(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	d.onSessions(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": waitTestSessions()})
	})
	d.on(func(w http.ResponseWriter, r *http.Request) {
		millis, err := strconv.Atoi(r.URL.Query().Get("timeout_ms"))
		// Resolution consumes about 100ms and the client reserves another
		// 100ms for the response. Leave generous scheduler headroom while
		// rejecting a handoff that expires grossly before the whole deadline.
		if err != nil || millis < 650 || millis >= 900 {
			t.Errorf("invalid remaining-budget handoff %q", r.URL.RawQuery)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		<-time.NewTimer(time.Duration(millis) * time.Millisecond).C
		id := "aaaaaaaa"
		if strings.Contains(r.URL.Path, "bbbbbbbb") {
			id = "bbbbbbbb"
		}
		writeEnvelope(w, http.StatusRequestTimeout, map[string]any{
			"reason": "timeout", "outcome": "timeout", "cause": "timeout",
			"exchanges": []map[string]any{{"ordinal": 1, "user": "partial " + id}},
		})
	})

	started := time.Now()
	var code int
	stdout := captureStdout(t, func() {
		code = cmdWait([]string{"alpha", "beta"}, 1, "", "", false)
	})
	if elapsed := time.Since(started); elapsed > 1100*time.Millisecond {
		t.Fatalf("one-second timeout exceeded hard whole-call bound: %s", elapsed)
	}
	if code != waitExitError || !strings.Contains(stdout, "partial aaaaaaaa") || !strings.Contains(stdout, "partial bbbbbbbb") {
		t.Fatalf("exit=%d stdout=%q", code, stdout)
	}
	if strings.Contains(stdout, "session state unknown") {
		t.Fatalf("authoritative timeout report was replaced by local fallback: %q", stdout)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, req := range d.requests {
		if strings.HasSuffix(req.path, "/read") {
			t.Fatalf("timed-out wait consumed unread: %+v", d.requests)
		}
	}
}

func TestWaitMultiRejectsSuccessInsideFormerGrace(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	d.on(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1100 * time.Millisecond)
		writeWaitOutcome(w, waitOutcomeCompleted, "late success")
	})

	started := time.Now()
	var code int
	stdout := captureStdout(t, func() {
		code = cmdWait([]string{"alpha", "beta"}, 1, "", "", false)
	})
	if elapsed := time.Since(started); elapsed > 1150*time.Millisecond {
		t.Fatalf("one-second timeout retained a response grace: %s", elapsed)
	}
	if code != waitExitError || strings.Contains(stdout, "late success") || strings.Count(stdout, "[Wait timed out after 1s; session state unknown]") != 2 {
		t.Fatalf("exit=%d stdout=%q", code, stdout)
	}
}

func TestWaitMultiSharedTimeoutBoundsStalledRequests(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	d.on(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond) // deliberately ignore ?timeout=1
		writeWaitOutcome(w, waitOutcomeCompleted, "too late")
	})
	started := time.Now()
	var code int
	stdout := captureStdout(t, func() { code = cmdWait([]string{"alpha", "beta"}, 1, "", "", false) })
	if elapsed := time.Since(started); elapsed > 1300*time.Millisecond {
		t.Fatalf("one-second timeout did not bound the whole call: %s", elapsed)
	}
	if code != 1 || strings.Count(stdout, "[Wait timed out after 1s; session state unknown]") != 2 {
		t.Fatalf("exit=%d stdout=%q", code, stdout)
	}
}

func TestWaitMultiSharedTimeoutBoundsStalledBodies(t *testing.T) {
	d := startStubDaemon(t, waitTestSessions())
	d.on(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond)
	})
	started := time.Now()
	var code int
	stdout := captureStdout(t, func() { code = cmdWait([]string{"alpha", "beta"}, 1, "", "", false) })
	if elapsed := time.Since(started); elapsed > 1400*time.Millisecond {
		t.Fatalf("one-second timeout did not bound stalled bodies: %s", elapsed)
	}
	if code != 1 || strings.Count(stdout, "[Wait timed out after 1s; session state unknown]") != 2 {
		t.Fatalf("exit=%d stdout=%q", code, stdout)
	}
}

func TestWaitMultiSignalSuppressesSettledReports(t *testing.T) {
	oldDie := dieFromSignal
	death := make(chan os.Signal, 1)
	dieFromSignal = func(sig os.Signal) { death <- sig }
	t.Cleanup(func() { dieFromSignal = oldDie })

	d := startStubDaemon(t, waitTestSessions())
	armed := make(chan struct{}, 2)
	release := make(chan struct{})
	d.on(func(w http.ResponseWriter, _ *http.Request) {
		armed <- struct{}{}
		<-release
		writeWaitOutcome(w, waitOutcomeCompleted, "late")
	})
	var code int
	stdout := captureStdout(t, func() {
		done := make(chan struct{})
		go func() {
			code = cmdWait([]string{"alpha", "beta"}, 0, "", "", false)
			close(done)
		}()
		<-armed
		<-armed
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(syscall.SIGINT)
		select {
		case <-death:
		case <-time.After(time.Second):
			t.Fatal("signal did not reach death path")
		}
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("multi-wait did not finish after handlers settled")
		}
	})
	if code != 130 || stdout != "[Wait interrupted; agent remains active]\n" {
		t.Fatalf("exit=%d stdout=%q", code, stdout)
	}
}

func TestWaitMultiResolutionFailsBeforeArming(t *testing.T) {
	for _, refs := range [][]string{{"alpha", "missing"}, {"alpha", "aaaaaaaa"}} {
		t.Run(strings.Join(refs, "_"), func(t *testing.T) {
			d := startStubDaemon(t, waitTestSessions())
			d.on(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("wait was armed despite resolution error")
				w.WriteHeader(http.StatusInternalServerError)
			})
			var code int
			stdout := captureStdout(t, func() {
				captureStderr(t, func() { code = cmdWait(refs, 0, "", "", false) })
			})
			if code != 1 || stdout != "" {
				t.Fatalf("exit=%d stdout=%q", code, stdout)
			}
			d.mu.Lock()
			requests := len(d.requests)
			d.mu.Unlock()
			if requests != 0 {
				t.Fatalf("armed %d waits", requests)
			}
		})
	}
}
