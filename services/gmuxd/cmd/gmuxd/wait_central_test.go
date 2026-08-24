package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func wf(s wire.Session) *sseFanout {
	f := newSSEFanout()
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{s}}})
	return f
}
func outcome(id string, alive bool, active *bool, exit *int) sessioncoord.Outcome {
	started := centralstore.UnixMillis(1)
	row := centralstore.Session{ID: centralstore.SessionID(id), StartedAt: &started, ExitCode: exit, StatusReported: active != nil}
	if active != nil {
		row.Active = *active
	}
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: &row, Alive: alive}
}
func boolp(v bool) *bool { return &v }

func TestTerminalReasonAndRunEvidenceTable(t *testing.T) {
	exit := 0
	cases := []struct {
		name string
		s    compatSession
		seen bool
		want waitConclusion
		done bool
	}{
		{"already idle", compatSession{Alive: true, Status: &compatStatus{}}, false,
			waitConclusion{Reason: "idle", Outcome: "completed"}, true},
		{"startup phantom", compatSession{}, false, waitConclusion{}, false},
		// Death before any status report proves neither a turn nor its
		// absence, so it is an error with a cause, never a completion.
		{"dead on arrival", compatSession{StartedAt: "x"}, false,
			waitConclusion{Reason: "died", Outcome: "error", Cause: "runner_died"}, true},
		{"clean death", compatSession{ExitCode: &exit, Status: &compatStatus{}}, false,
			waitConclusion{Reason: "idle", Outcome: "completed"}, true},
		{"mid turn death", compatSession{ExitCode: &exit, Status: &compatStatus{Active: true}}, false,
			waitConclusion{Reason: "died", Outcome: "error", Cause: "runner_died"}, true},
		// An intentional stop closes the turn: the wait resolves on the
		// inactive edge exactly like a completion, but its conclusion is
		// `interrupted`, which the CLI reports as exit 2 with no result.
		{"interrupted live", compatSession{Alive: true, Status: &compatStatus{Interrupted: true}}, false,
			waitConclusion{Reason: "idle", Outcome: "interrupted"}, true},
		{"interrupted then death", compatSession{ExitCode: &exit, Status: &compatStatus{Interrupted: true}}, false,
			waitConclusion{Reason: "idle", Outcome: "interrupted"}, true},
		{"terminal failure", compatSession{Alive: true, Status: &compatStatus{Error: true}}, false,
			waitConclusion{Reason: "idle", Outcome: "error"}, true},
		// Error wins over interruption: a turn that failed is not a clean
		// stop, and an interrupted flag left over from a prior stop must not
		// downgrade a failure.
		{"failure with interrupt flag", compatSession{Alive: true, Status: &compatStatus{Error: true, Interrupted: true}}, false,
			waitConclusion{Reason: "idle", Outcome: "error"}, true},
		{"active error keeps waiting", compatSession{Alive: true, Status: &compatStatus{Active: true, Error: true}}, false,
			waitConclusion{}, false},
		{"dead mid turn with error keeps its cause", compatSession{ExitCode: &exit, Status: &compatStatus{Active: true, Error: true}}, false,
			waitConclusion{Reason: "died", Outcome: "error", Cause: "runner_died"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, d := terminalReason(tc.s, tc.seen)
			if r != tc.want || d != tc.done {
				t.Fatalf("got %+v,%v want %+v,%v", r, d, tc.want, tc.done)
			}
		})
	}
	if got := classifyTurnClose(false, false); got != "completed" {
		t.Fatalf("classify clean: %q", got)
	}
	if got := diedConclusion(); got.Outcome != "error" || got.Cause != "runner_died" || got.Reason != "died" {
		t.Fatalf("died conclusion: %+v", got)
	}
	if !hasRunEvidence(compatSession{}, true) || !hasRunEvidence(compatSession{StartedAt: "x"}, false) || hasRunEvidence(compatSession{}, false) {
		t.Fatal("run evidence table")
	}
}

func TestConvertedDeadActiveSnapshotStillReportsRunnerDied(t *testing.T) {
	exit := 1
	row := central.SessionRow{SessionView: centralstore.SessionView{Session: centralstore.Session{
		ID: "dead", CreatedAt: 1, StartedAt: func() *centralstore.UnixMillis { v := centralstore.UnixMillis(1); return &v }(),
		ExitCode: &exit, StatusReported: true, Active: true,
	}}}
	payload := (&wire.Converter{}).Sessions(&central.SessionsPayload{Sessions: []central.SessionRow{row}}, nil, nil)
	if len(payload.Sessions) != 1 || payload.Sessions[0].Status == nil || !payload.Sessions[0].Status.Active {
		t.Fatalf("converter dropped durable active-at-death: %#v", payload.Sessions)
	}
	got, done := terminalReason(legacySessionFromWire(payload.Sessions[0]), false)
	want := waitConclusion{Reason: "died", Outcome: "error", Cause: "runner_died"}
	if !done || got != want {
		t.Fatalf("converted wait verdict = %+v,%v; want %+v,true", got, done, want)
	}
}

func TestInputSubmitsTable(t *testing.T) {
	for _, tc := range []struct {
		name, s string
		want    bool
	}{{"carriage", "x\r", true}, {"kitty enter", "x\x1b[13u", true}, {"kitty modified", "x\x1b[13;2u", true}, {"newline", "x\n", false}, {"plain", "x", false}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputSubmits([]byte(tc.s)); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestAwaitTurnCentralSchedules(t *testing.T) {
	t.Run("block to idle cannot miss pulse", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
		ch := make(chan sessioncoord.Outcome, 2)
		ch <- outcome("s", true, boolp(true), nil)
		ch <- outcome("s", true, boolp(false), nil)
		r, to := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r.Reason != "idle" || r.Outcome != outcomeCompleted || to {
			t.Fatalf("%+v %v", r, to)
		}
	})
	t.Run("mid turn death", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true})
		ch := make(chan sessioncoord.Outcome, 2)
		ch <- outcome("s", true, boolp(true), nil)
		x := 1
		ch <- outcome("s", false, boolp(true), &x)
		r, _ := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r.Reason != "died" || r.Outcome != outcomeError || r.Cause != causeRunnerDied {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("timeout stale idle ignored", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
		r, to := awaitTurnCentral(context.Background(), f, make(chan sessioncoord.Outcome), "s", time.After(10*time.Millisecond))
		if r.Reason != "" || !to {
			t.Fatalf("%+v %v", r, to)
		}
	})
	t.Run("removal", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true})
		ch := make(chan sessioncoord.Outcome, 1)
		ch <- sessioncoord.Outcome{Type: sessioncoord.OutcomeRemoved, ID: "s"}
		r, _ := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r.Reason != "died" || r.Outcome != outcomeError {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("dropped event repoll", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: true}})
		ch := make(chan sessioncoord.Outcome)
		go func() {
			time.Sleep(20 * time.Millisecond)
			f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Active: false}}}}})
		}()
		r, to := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r.Reason != "idle" || r.Outcome != outcomeCompleted || to {
			t.Fatalf("%+v %v", r, to)
		}
	})
	// send --wait must observe an intentional stop, not just idleness: without
	// it the composition the docs call preferred exits 0 for a turn that
	// `gmux wait` reports as exit 2.
	t.Run("interrupted turn is reported as interrupted", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
		ch := make(chan sessioncoord.Outcome, 2)
		ch <- outcome("s", true, boolp(true), nil)
		ch <- interruptedOutcome("s")
		r, _ := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r.Reason != "idle" || r.Outcome != outcomeInterrupted {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("failed turn is reported as an error", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
		ch := make(chan sessioncoord.Outcome, 2)
		ch <- outcome("s", true, boolp(true), nil)
		ch <- erroredOutcome("s")
		r, _ := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r.Reason != "idle" || r.Outcome != outcomeError || r.Cause != "" {
			t.Fatalf("%+v", r)
		}
	})
}

// interruptedOutcome/erroredOutcome are closed-turn reports carrying the
// terminal flags the shared classification reads.
func interruptedOutcome(id string) sessioncoord.Outcome {
	started := centralstore.UnixMillis(1)
	row := centralstore.Session{ID: centralstore.SessionID(id), StartedAt: &started, StatusReported: true, Interrupted: true}
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: &row, Alive: true}
}

func erroredOutcome(id string) sessioncoord.Outcome {
	started := centralstore.UnixMillis(1)
	row := centralstore.Session{ID: centralstore.SessionID(id), StartedAt: &started, StatusReported: true, Error: true}
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: &row, Alive: true}
}

func TestWaitOutputExistingBlockingRegexAndExitWinner(t *testing.T) {
	dir := t.TempDir()
	sess := wire.Session{ID: "s", Alive: true, TerminalCols: 80, TerminalRows: 24}
	write := func(v string) {
		if err := os.WriteFile(filepath.Join(dir, scrollback.ActiveName), []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("hello world\n")
	if !outputMatchesCentral(dir, sess, func(s string) bool { return strings.Contains(s, "world") }) {
		t.Fatal("existing text")
	}
	if !outputMatchesCentral(dir, sess, func(s string) bool { return strings.HasPrefix(s, "hello") }) {
		t.Fatal("regex-equivalent")
	}
	write("final match\n")
	if !outputMatchesCentral(dir, sess, func(s string) bool { return s == "final match" }) {
		t.Fatal("match at exit must win")
	}
	write("waiting\n")
	if outputMatchesCentral(dir, sess, func(s string) bool { return strings.Contains(s, "later") }) {
		t.Fatal("premature match")
	}
	write("waiting later\n")
	if !outputMatchesCentral(dir, sess, func(s string) bool { return strings.Contains(s, "later") }) {
		t.Fatal("blocking match")
	}
}

func TestWaitAndInputBadConditions(t *testing.T) {
	f := wf(wire.Session{ID: "s", Alive: true})
	for _, url := range []string{"/wait?for_text=x&for_regex=x", "/wait?for_regex=[", "/wait?timeout=nope"} {
		rec := httptest.NewRecorder()
		handleWaitCentral(rec, httptest.NewRequest(http.MethodPost, url, nil), nil, f, "s", func(string) string { return t.TempDir() })
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s=%d", url, rec.Code)
		}
	}
	for _, tc := range []struct {
		url    string
		body   []byte
		status int
	}{{"/input?wait=bogus", []byte("x\r"), 400}, {"/input?wait=idle", []byte("x\n"), 422}} {
		{
			rec := httptest.NewRecorder()
			handleInputWaitCentral(rec, httptest.NewRequest(http.MethodPost, tc.url, nil), nil, f, "s", tc.body, func() error { return nil })
			if rec.Code != tc.status {
				t.Fatalf("%s=%d", tc.url, rec.Code)
			}
		}
	}
}

func TestCentralWaitHandlerAlreadyIdleDeadArrivalNoPhantomAndTimeout(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The wait handler uses a store-direct existence check; seed the
	// session so it passes.
	if _, _, err := st.InsertSession(ctx, centralstore.NewSession{
		ID: "s", Adapter: "shell", Command: []string{"sh"},
		CreatedAt: centralstore.UnixMillis(1),
	}); err != nil {
		t.Fatal(err)
	}
	coord := sessioncoord.New(nil, &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{}, blocked: map[string]bool{}}, st, nil, nil)
	defer coord.Close()
	boot := &Bootstrap{Store: st, Coordinator: coord}
	for _, tc := range []struct {
		name   string
		s      wire.Session
		want   int
		reason string
	}{{"already idle", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}, UnreadToken: "result-1"}, 200, "idle"}, {"dead arrival", wire.Session{ID: "s", Alive: false, StartedAt: "x"}, 200, "died"}, {"no phantom death timeout", wire.Session{ID: "s", Alive: false}, 408, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := wf(tc.s)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/wait?timeout=1", nil)
			handleWaitCentral(rec, req, boot, f, "s", func(string) string { return t.TempDir() })
			if rec.Code != tc.want {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.reason != "" && !strings.Contains(rec.Body.String(), tc.reason) {
				t.Fatalf("body=%s", rec.Body.String())
			}
			if tc.name == "already idle" && !strings.Contains(rec.Body.String(), `"unread_token":"result-1"`) {
				t.Fatalf("wait response lost observed unread token: %s", rec.Body.String())
			}
		})
	}
}

func TestCentralInputWaitKittyAcceptedAndTimesOut(t *testing.T) {
	st, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	coord := sessioncoord.New(nil, &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{}, blocked: map[string]bool{}}, st, nil, nil)
	defer coord.Close()
	boot := &Bootstrap{Store: st, Coordinator: coord}
	f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Active: false}})
	sent := false
	rec := httptest.NewRecorder()
	handleInputWaitCentral(rec, httptest.NewRequest(http.MethodPost, "/input?wait=idle&timeout=1", nil), boot, f, "s", []byte("x\x1b[13u"), func() error { sent = true; return nil })
	if !sent {
		t.Fatal("kitty Enter rejected before send")
	}
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
