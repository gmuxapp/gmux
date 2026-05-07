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

// waitTestServer wires handleWait against a real store.Store at the
// path the production dispatcher serves it from. Tests then drive
// store.Upsert to simulate Working transitions and observe the
// daemon's response. The store is the load-bearing dependency here:
// faking it would only test the test, since the whole point of
// handleWait is to consume real session-event broadcasts.
func waitTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := store.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[3] != "wait" {
			http.NotFound(w, r)
			return
		}
		handleWait(w, r, st, parts[2])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

func waitURL(srv *httptest.Server, id string) string {
	return srv.URL + "/v1/sessions/" + id + "/wait"
}

// TestWaitReturnsImmediatelyWhenAlreadyIdle pins the contract that
// `gmux --wait` is a no-op when the agent has already finished its
// turn before the wait call lands. This is the common case for
// composition (`gmux --send X && gmux --wait X` after the agent
// races ahead between the two CLI calls), and the no-op-when-idle
// behavior is what makes that composition reliable.
func TestWaitReturnsImmediatelyWhenAlreadyIdle(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:     "sess-idle",
		Kind:   "claude",
		Alive:  true,
		Status: &store.Status{Working: false},
	})

	start := time.Now()
	resp, body := postWait(t, srv, "sess-idle")
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("returned in %v, want immediate", elapsed)
	}
}

// TestWaitBlocksUntilWorkingFlipsFalse is the headline behavior: a
// session that's currently busy should block --wait, and unblock it
// the moment the adapter emits Working: false. Drives the store
// asynchronously so the test exercises the subscription path rather
// than the initial-snapshot fast path.
func TestWaitBlocksUntilWorkingFlipsFalse(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:     "sess-busy",
		Kind:   "pi",
		Alive:  true,
		Status: &store.Status{Working: true},
	})

	done := make(chan struct{})
	var resp *http.Response
	var body map[string]any
	go func() {
		resp, body = postWait(t, srv, "sess-busy")
		close(done)
	}()

	// Give the goroutine time to land in the Subscribe + select loop;
	// any short, finite delay is fine because the test fails by
	// timeout, not by sleeping too short.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("--wait returned before the Working flag flipped")
	default:
	}

	st.Upsert(store.Session{
		ID:     "sess-busy",
		Kind:   "pi",
		Alive:  true,
		Status: &store.Status{Working: false},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("--wait did not return after Working flipped to false")
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}

// TestWaitReturnsDiedWhenSessionDies covers the failure mode where a
// session crashes or is killed mid-turn. The caller's intent is "wait
// for this turn to finish"; if the agent dies first we surface that
// with reason=died so the CLI can map it to a non-zero exit code.
func TestWaitReturnsDiedWhenSessionDies(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:     "sess-busy",
		Kind:   "claude",
		Alive:  true,
		Status: &store.Status{Working: true},
	})

	done := make(chan struct{})
	var body map[string]any
	go func() {
		_, body = postWait(t, srv, "sess-busy")
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)

	// Death signal: Alive flips to false. Status.Working may still be
	// true at the point of death (adapter never got to emit a final
	// flip) — that's exactly the case the "died" reason is for.
	st.Upsert(store.Session{
		ID:     "sess-busy",
		Kind:   "claude",
		Alive:  false,
		Status: &store.Status{Working: true},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("--wait did not return after session died")
	}

	if got := body["data"].(map[string]any)["reason"]; got != "died" {
		t.Errorf("reason = %v, want died", got)
	}
}

// TestWaitTimesOut verifies the timeout escape hatch returns a
// distinct HTTP status (408) so the CLI can tell timeout apart from
// idle and from died.
func TestWaitTimesOut(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:     "sess-stuck",
		Kind:   "codex",
		Alive:  true,
		Status: &store.Status{Working: true},
	})

	// 1-second timeout; production callers compose with `timeout`
	// for sub-second values, but the endpoint accepts integer seconds.
	req, _ := http.NewRequest(http.MethodPost, waitURL(srv, "sess-stuck")+"?timeout=1", nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", resp.StatusCode)
	}
	if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want ~1s", elapsed)
	}
}

// TestWaitRejectsShellSessions pins the allowlist: shell sessions
// don't emit Working transitions, so `gmux --wait` against them
// would return immediately with reason=idle and silently do the
// wrong thing for `gmux make build && gmux --wait <id>`-style
// composition. 422 surfaces the limitation explicitly.
func TestWaitRejectsShellSessions(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:    "sess-shell",
		Kind:  "shell",
		Alive: true,
	})

	resp, _ := postWait(t, srv, "sess-shell")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// TestWaitDoesNotTreatNilStatusAsIdle pins the contract that a
// freshly-spawned agent (registered in the store but not yet emitting
// status events) is not mistaken for an already-idle session. Without
// this guard, `gmux pi <prompt> --no-attach && gmux --wait $id` would
// race the runner's first Working:true event and return immediately.
func TestWaitDoesNotTreatNilStatusAsIdle(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:     "sess-fresh",
		Kind:   "pi",
		Alive:  true,
		Status: nil, // hasn't emitted its first status yet
	})

	done := make(chan struct{})
	var body map[string]any
	go func() {
		_, body = postWait(t, srv, "sess-fresh")
		close(done)
	}()

	// Should not return on its own — nil Status means "not started yet,"
	// not "idle." Wait long enough that the 500ms re-poll runs at least
	// once: if the implementation treats nil as idle anywhere (initial
	// snapshot, event handler, or ticker) the test catches it.
	select {
	case <-done:
		t.Fatal("--wait returned for nil-status session before any transition")
	case <-time.After(700 * time.Millisecond):
	}

	st.Upsert(store.Session{
		ID:     "sess-fresh",
		Kind:   "pi",
		Alive:  true,
		Status: &store.Status{Working: false},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("--wait did not return after Status appeared")
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}

// TestWaitUnderHighEventLoad asserts --wait still completes correctly
// when the subscription channel is under heavy traffic from unrelated
// sessions. store.broadcast uses a non-blocking send into a 64-slot
// buffered channel, so events for our session can theoretically be
// dropped if other sessions outpace handleWait's drain. handleWait
// guards against this with a periodic re-snapshot of the session
// state; this test exercises the high-throughput path and verifies
// the response is correct regardless of which path (subscription or
// re-snapshot) caught the transition.
func TestWaitUnderHighEventLoad(t *testing.T) {
	srv, st := waitTestServer(t)
	st.Upsert(store.Session{
		ID:     "sess-drop",
		Kind:   "claude",
		Alive:  true,
		Status: &store.Status{Working: true},
	})

	done := make(chan struct{})
	var body map[string]any
	go func() {
		_, body = postWait(t, srv, "sess-drop")
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 500; i++ {
		st.Upsert(store.Session{ID: "sess-noise", Kind: "claude", Alive: true, Status: &store.Status{Working: true}})
	}
	st.Upsert(store.Session{
		ID:     "sess-drop",
		Kind:   "claude",
		Alive:  true,
		Status: &store.Status{Working: false},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("--wait did not return under high event load")
	}
	if got := body["data"].(map[string]any)["reason"]; got != "idle" {
		t.Errorf("reason = %v, want idle", got)
	}
}

// TestWaitNotFound keeps the "missing session" case from being
// confused with "session is busy forever."
func TestWaitNotFound(t *testing.T) {
	srv, _ := waitTestServer(t)
	resp, _ := postWait(t, srv, "sess-nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func postWait(t *testing.T, srv *httptest.Server, id string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(waitURL(srv, id), "", nil)
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
