package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// postInput POSTs to /v1/sessions/<idAndQuery> where idAndQuery is
// "<id>/input?..." and decodes the JSON envelope (if any).
func postInput(t *testing.T, srv *httptest.Server, idAndQuery string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/sessions/"+idAndQuery, "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
	}
	return resp, body
}

// sendWaitTestServer wires handleInputWait against a real store the
// way main.go's input?wait=idle branch does, with the runner delivery
// replaced by a send closure. Like waitTestServer, the store is the
// load-bearing dependency: handleInputWait's whole contract is about
// its subscription ordering against real store broadcasts.
func sendWaitTestServer(t *testing.T, send func(st *store.Store)) (*httptest.Server, *store.Store) {
	t.Helper()
	st := store.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[3] != "input" {
			http.NotFound(w, r)
			return
		}
		sess, ok := st.Get(parts[2])
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		body := []byte("prompt\r")
		if b := r.URL.Query().Get("body"); b != "" {
			body = []byte(b)
		}
		handleInputWait(w, r, st, sess, body, func() error {
			if send != nil {
				send(st)
			}
			return nil
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

func upsertAgent(st *store.Store, id string, alive bool, status *store.Status) {
	st.Upsert(store.Session{ID: id, Adapter: "pi", Alive: alive, Status: status})
}

// TestSendWaitIgnoresStalePreviousIdle is the regression test for
// issue #218: at the moment the input is delivered, the store still
// holds the *previous* turn's Working=false. A naive send-then-wait
// composition returns "idle" immediately off that stale snapshot.
// send --wait must instead hold until a fresh Working=true pulse and
// its subsequent Working=false.
func TestSendWaitIgnoresStalePreviousIdle(t *testing.T) {
	const id = "sess-stale"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		// Simulate the async runner: the agent starts working shortly
		// after the bytes land, then finishes its turn.
		go func() {
			time.Sleep(100 * time.Millisecond)
			upsertAgent(st, id, true, &store.Status{Working: true})
			time.Sleep(150 * time.Millisecond)
			upsertAgent(st, id, true, &store.Status{Working: false})
		}()
	})
	// Stale state: idle from the previous turn.
	upsertAgent(st, id, true, &store.Status{Working: false})

	start := time.Now()
	resp, body := postInput(t, srv, id+"/input?wait=idle")
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned in %v — observed the stale previous idle instead of the fresh turn", elapsed)
	}
}

// TestSendWaitCannotMissTheWorkingPulse pins the subscribe-before-send
// ordering: even when the Working pulse is broadcast synchronously
// inside the send delivery itself (the fastest possible agent), the
// wait half observes it because the subscription already exists.
func TestSendWaitCannotMissTheWorkingPulse(t *testing.T) {
	const id = "sess-fast"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		// The entire turn happens "instantly" during delivery.
		upsertAgent(st, id, true, &store.Status{Working: true})
		upsertAgent(st, id, true, &store.Status{Working: false})
	})
	upsertAgent(st, id, true, &store.Status{Working: false})

	resp, body := postInput(t, srv, id+"/input?wait=idle&timeout=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}

// TestSendWaitReturnsDiedMidTurn: the agent starts working and then
// crashes before going idle.
func TestSendWaitReturnsDiedMidTurn(t *testing.T) {
	const id = "sess-crash"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			upsertAgent(st, id, true, &store.Status{Working: true})
			time.Sleep(50 * time.Millisecond)
			upsertAgent(st, id, false, &store.Status{Working: true})
		}()
	})
	upsertAgent(st, id, true, &store.Status{Working: false})

	resp, body := postInput(t, srv, id+"/input?wait=idle")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "died" {
		t.Errorf("reason = %v, want died", got)
	}
}

// TestSendWaitTimesOut: the agent never reacts (wedged adapter, modal
// dialog); ?timeout=N bounds the wait with a distinct 408.
func TestSendWaitTimesOut(t *testing.T) {
	const id = "sess-wedged"
	srv, st := sendWaitTestServer(t, nil)
	upsertAgent(st, id, true, &store.Status{Working: false})

	start := time.Now()
	resp, _ := postInput(t, srv, id+"/input?wait=idle&timeout=1")
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", resp.StatusCode)
	}
	if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want ~1s", elapsed)
	}
}

// TestSendWaitRejectsShellSessionsWithoutPromptMarks mirrors
// handleWait's gate: a shell session with a nil Status has never
// emitted an OSC 133 prompt mark, so no idle signal exists and the
// wait contract can't be honored — fail loudly, don't deliver.
func TestSendWaitRejectsShellSessionsWithoutPromptMarks(t *testing.T) {
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		t.Error("input must not be delivered when the wait contract can't be honored")
	})
	st.Upsert(store.Session{ID: "sess-shell", Adapter: "shell", Alive: true})

	resp, _ := postInput(t, srv, "sess-shell/input?wait=idle")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// upsertShell mirrors upsertAgent for shell sessions, whose Status
// comes from runner-tracked OSC 133 prompt marks (issue #373).
func upsertShell(st *store.Store, id string, alive bool, status *store.Status) {
	st.Upsert(store.Session{ID: id, Adapter: "shell", Alive: alive, Status: status})
}

// TestSendWaitShellPromptCycle: `gmux send --wait` on a shell sitting
// at its prompt (idle Status from the last OSC 133;A mark) must hold
// through the command it triggers — busy on the command-start mark,
// idle again when the prompt returns — exactly like an agent turn.
// Without the per-session evidence gate this 422s; without honoring
// the pulse it would return off the stale pre-send idle.
func TestSendWaitShellPromptCycle(t *testing.T) {
	const id = "sess-shell-cycle"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		// Simulate the runner's prompt-mark tracking after the bytes
		// land: 133;C flips busy, the next 133;A flips idle.
		go func() {
			time.Sleep(100 * time.Millisecond)
			upsertShell(st, id, true, &store.Status{Working: true})
			time.Sleep(150 * time.Millisecond)
			upsertShell(st, id, true, &store.Status{Working: false})
		}()
	})
	// Stale state: at the prompt since the previous command.
	upsertShell(st, id, true, &store.Status{Working: false})

	start := time.Now()
	resp, body := postInput(t, srv, id+"/input?wait=idle")
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned in %v — observed the stale pre-send prompt idle instead of the triggered command's cycle", elapsed)
	}
}

// TestSendWaitShellFastCommandPulse: a fast shell command's busy and
// idle marks can arrive in a single PTY read; the runner still emits
// both Status transitions, and because the subscription predates the
// delivery, send --wait observes the full pulse. This pins the
// daemon half of that contract (the runner half lives in
// ptyserver's prompt-mark tests).
func TestSendWaitShellFastCommandPulse(t *testing.T) {
	const id = "sess-shell-fast"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		upsertShell(st, id, true, &store.Status{Working: true})
		upsertShell(st, id, true, &store.Status{Working: false})
	})
	upsertShell(st, id, true, &store.Status{Working: false})

	resp, body := postInput(t, srv, id+"/input?wait=idle&timeout=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}

// TestSendWaitShellDiesOnExit: `gmux send <id> 'exit' Enter --wait`
// ends the session instead of returning to a prompt. The wait must
// unblock with "died" rather than hang — process exit always
// satisfies a wait, just with a distinct reason/exit code.
func TestSendWaitShellDiesOnExit(t *testing.T) {
	const id = "sess-shell-exit"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			upsertShell(st, id, true, &store.Status{Working: true})
			time.Sleep(50 * time.Millisecond)
			upsertShell(st, id, false, nil)
		}()
	})
	upsertShell(st, id, true, &store.Status{Working: false})

	resp, body := postInput(t, srv, id+"/input?wait=idle")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "died" {
		t.Errorf("reason = %v, want died", got)
	}
}

// TestSendWaitRejectsNonSubmittingInput: input without a carriage
// return never starts a turn, so waiting on it would only ever time
// out. Fail loudly instead, and don't deliver the bytes.
func TestSendWaitRejectsNonSubmittingInput(t *testing.T) {
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		t.Error("non-submitting input must not be delivered under --wait")
	})
	upsertAgent(st, "sess-nosubmit", true, &store.Status{Working: false})

	resp, body := postInput(t, srv, "sess-nosubmit/input?wait=idle&body=no+enter+here")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	if body != nil {
		if errObj, ok := body["error"].(map[string]any); ok {
			if errObj["code"] != "input_no_submit" {
				t.Errorf("error code = %v, want input_no_submit", errObj["code"])
			}
		}
	}
}

// TestSendWaitAcceptsKittyEnterSubmit is the regression test for
// `gmux send --follow-up --wait` on pi: the follow-up submit is the
// Kitty CSI-u encoding of Alt+Enter (\x1b[13;3u), which contains no
// carriage return. The input_no_submit guard must recognize it as a
// submit — rejecting it would break the flag composition the pi
// adapter's SubmitSeq deliberately produces.
func TestSendWaitAcceptsKittyEnterSubmit(t *testing.T) {
	const id = "sess-kitty"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		upsertAgent(st, id, true, &store.Status{Working: true})
		upsertAgent(st, id, true, &store.Status{Working: false})
	})
	upsertAgent(st, id, true, &store.Status{Working: false})

	resp, body := postInput(t, srv, id+"/input?wait=idle&timeout=2&body=prompt%1B%5B13%3B3u")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (CSI-u Enter must count as a submit)", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}

// TestInputSubmits pins the daemon's definition of "this input can
// start a turn": a carriage return anywhere (xterm Enter, legacy
// Alt+Enter = ESC CR) or a Kitty CSI-u Enter sequence (any modifier /
// event-type shape). Non-Enter CSI-u keys and bare newlines must NOT
// count — they never submit, so a --wait on them could only time out.
func TestInputSubmits(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"plain Enter", "prompt\r", true},
		{"legacy alt+enter (ESC CR)", "prompt\x1b\r", true},
		{"kitty alt+enter", "prompt\x1b[13;3u", true},
		{"kitty bare enter", "prompt\x1b[13u", true},
		{"kitty enter with event type", "prompt\x1b[13;3:1u", true},
		{"bare newline is literal text", "prompt\n", false},
		{"no submit at all", "prompt", false},
		{"kitty non-enter key (alt+a)", "prompt\x1b[97;3u", false},
		{"csi-u-looking text without ESC", "prompt[13;3u", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputSubmits([]byte(tc.body)); got != tc.want {
				t.Errorf("inputSubmits(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestSendWaitRejectsUnknownWaitMode: a typo'd wait mode (wait=true,
// wait=1, ...) must fail loudly rather than silently degrading to
// fire-and-forget — the caller believes it waited for the reply.
func TestSendWaitRejectsUnknownWaitMode(t *testing.T) {
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		t.Error("input must not be delivered for an unsupported wait mode")
	})
	upsertAgent(st, "sess-typo", true, &store.Status{Working: false})

	resp, _ := postInput(t, srv, "sess-typo/input?wait=true")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSendWaitAlreadyWorkingWaitsForCurrentTurn: if the agent is
// already mid-turn when the input lands, the bytes queue behind the
// current turn; its completion is the answer.
func TestSendWaitAlreadyWorkingWaitsForCurrentTurn(t *testing.T) {
	const id = "sess-busy"
	srv, st := sendWaitTestServer(t, func(st *store.Store) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			upsertAgent(st, id, true, &store.Status{Working: false})
		}()
	})
	upsertAgent(st, id, true, &store.Status{Working: true})

	resp, body := postInput(t, srv, id+"/input?wait=idle")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}
