package discovery

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"nhooyr.io/websocket"
)

// The transport half of targeted termination: the daemon decides which runner
// to kill, and the runner enforces that decision. Between them sits this
// request, which has to carry the name and interpret the refusal.
//
// Mutation: drop the header, or map 409 onto the generic error path.
func TestKillSessionCarriesTheExpectedIncarnation(t *testing.T) {
	var got string
	var calls int
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got = r.Header.Get(expectIncarnationHeader)
		// Behave like a runner that is not the named process.
		if got != "" && got != "the-runner-that-answers" {
			http.Error(w, "incarnation mismatch", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	err := KillSessionContext(context.Background(), srv.socketPath, "the-runner-we-classified")
	if !errors.Is(err, ErrRunnerIncarnationMismatch) {
		t.Fatalf("KillSessionContext = %v, want ErrRunnerIncarnationMismatch", err)
	}
	if got != "the-runner-we-classified" {
		t.Fatalf("runner saw expectation %q, want the classified runner", got)
	}

	// The same call against the runner it named succeeds.
	if err := KillSessionContext(context.Background(), srv.socketPath, "the-runner-that-answers"); err != nil {
		t.Fatalf("KillSessionContext against the named runner: %v", err)
	}

	// And a caller with no particular runner in mind sends no expectation --
	// the pre-protocol behaviour an explicit user stop still relies on.
	if err := KillSessionContext(context.Background(), srv.socketPath, ""); err != nil {
		t.Fatalf("unnamed KillSessionContext: %v", err)
	}
	if got != "" {
		t.Fatalf("an unnamed kill sent expectation %q", got)
	}
	if calls != 3 {
		t.Fatalf("runner saw %d requests, want 3", calls)
	}
}

// See the note in ptyserver's incarnation_test.go: two modules, one wire.
func TestExpectIncarnationHeaderNameIsStable(t *testing.T) {
	if expectIncarnationHeader != "X-Gmux-Expect-Incarnation" {
		t.Errorf("expectIncarnationHeader = %q; the runner reads X-Gmux-Expect-Incarnation", expectIncarnationHeader)
	}
}

// The statuses that mean "this endpoint has no conditional reap".
//
// 404 is the tidy case and not the real one: a pre-protocol runner registers a
// catch-all "/" route for its WebSocket terminal, so POST /reap reaches the
// handshake and comes back 426. Mutation: narrow this back to 404 only.
func TestReapUnsupportedStatuses(t *testing.T) {
	unsupported := []int{
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusUpgradeRequired,
		http.StatusNotImplemented,
	}
	for _, code := range unsupported {
		if !reapUnsupportedStatus(code) {
			t.Errorf("status %d is not treated as an unsupported route", code)
		}
	}
	// A runner that understood the request and refused it, this transport's own
	// mistake, and a genuine failure are all distinct from "no such operation".
	for _, code := range []int{
		http.StatusNoContent,
		http.StatusBadRequest,
		http.StatusConflict,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		if reapUnsupportedStatus(code) {
			t.Errorf("status %d was treated as an unsupported route", code)
		}
	}
}

// End to end over a real socket against the real WebSocket handshake, which is
// what a pre-protocol runner's catch-all actually is.
func TestReapAgainstAWebSocketCatchAllIsUnsupported(t *testing.T) {
	mux := http.NewServeMux()
	var reached int
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reached++
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // the handshake wrote its own status
		}
		c.Close(websocket.StatusNormalClosure, "")
	})
	srv := startUnixServer(t, mux)

	err := ReapSessionContext(context.Background(), srv.socketPath, "incarnation-of-somebody")
	if !errors.Is(err, ErrRunnerReapUnsupported) {
		t.Fatalf("ReapSessionContext against a WebSocket catch-all = %v, want ErrRunnerReapUnsupported", err)
	}
	if reached != 1 {
		t.Fatalf("the catch-all was reached %d times, want 1", reached)
	}
}

// An unconditional reap is not an operation this transport offers, and the
// refusal happens before anything leaves the process: a request with no
// expectation would reach a runner that could only answer 400, and a
// pre-protocol occupant would not even get that far -- its catch-all would see
// a POST it cannot serve. Neither is a conversation worth having.
//
// Mutation: drop the empty-expectation early return.
func TestReapSessionRefusesAnEmptyIncarnationWithoutAskingTheRunner(t *testing.T) {
	var requests int
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))

	err := ReapSessionContext(context.Background(), srv.socketPath, "")
	if err == nil {
		t.Fatal("ReapSessionContext accepted an empty expectation")
	}
	if errors.Is(err, ErrRunnerReapUnsupported) || errors.Is(err, ErrRunnerIncarnationMismatch) {
		t.Fatalf("ReapSessionContext = %v; an unnamed reap is the caller's mistake, "+
			"not a verdict about the occupant", err)
	}
	if requests != 0 {
		t.Fatalf("the runner received %d requests for an unnamed reap, want 0", requests)
	}
	if srv.accepts.Load() != 0 {
		t.Fatalf("the runner's socket was dialled %d times, want 0", srv.accepts.Load())
	}
}
