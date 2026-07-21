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
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func wf(s wire.Session) *sseFanout {
	f := newSSEFanout()
	f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{s}}})
	return f
}
func outcome(id string, alive bool, working *bool, exit *int) sessioncoord.Outcome {
	started := centralstore.UnixMillis(1)
	row := centralstore.Session{ID: centralstore.SessionID(id), StartedAt: &started, ExitCode: exit, StatusReported: working != nil}
	if working != nil {
		row.Working = *working
	}
	return sessioncoord.Outcome{Type: sessioncoord.OutcomeUpserted, ID: row.ID, Session: &row, Alive: alive}
}
func boolp(v bool) *bool { return &v }

func TestTerminalReasonAndRunEvidenceTable(t *testing.T) {
	exit := 0
	cases := []struct {
		name   string
		s      compatSession
		seen   bool
		reason string
		done   bool
	}{
		{"already idle", compatSession{Alive: true, Status: &compatStatus{}}, false, "idle", true},
		{"startup phantom", compatSession{}, false, "", false},
		{"dead on arrival", compatSession{StartedAt: "x"}, false, "died", true},
		{"clean death", compatSession{ExitCode: &exit, Status: &compatStatus{}}, false, "idle", true},
		{"mid turn death", compatSession{ExitCode: &exit, Status: &compatStatus{Working: true}}, false, "died", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, d := terminalReason(tc.s, tc.seen)
			if r != tc.reason || d != tc.done {
				t.Fatalf("got %q,%v", r, d)
			}
		})
	}
	if !hasRunEvidence(compatSession{}, true) || !hasRunEvidence(compatSession{StartedAt: "x"}, false) || hasRunEvidence(compatSession{}, false) {
		t.Fatal("run evidence table")
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
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Working: false}})
		ch := make(chan sessioncoord.Outcome, 2)
		ch <- outcome("s", true, boolp(true), nil)
		ch <- outcome("s", true, boolp(false), nil)
		r, to := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r != "idle" || to {
			t.Fatalf("%q %v", r, to)
		}
	})
	t.Run("mid turn death", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true})
		ch := make(chan sessioncoord.Outcome, 2)
		ch <- outcome("s", true, boolp(true), nil)
		x := 1
		ch <- outcome("s", false, boolp(true), &x)
		r, _ := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r != "died" {
			t.Fatal(r)
		}
	})
	t.Run("timeout stale idle ignored", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Working: false}})
		r, to := awaitTurnCentral(context.Background(), f, make(chan sessioncoord.Outcome), "s", time.After(10*time.Millisecond))
		if r != "" || !to {
			t.Fatalf("%q %v", r, to)
		}
	})
	t.Run("removal", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true})
		ch := make(chan sessioncoord.Outcome, 1)
		ch <- sessioncoord.Outcome{Type: sessioncoord.OutcomeRemoved, ID: "s"}
		r, _ := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r != "died" {
			t.Fatal(r)
		}
	})
	t.Run("dropped event repoll", func(t *testing.T) {
		f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Working: true}})
		ch := make(chan sessioncoord.Outcome)
		go func() {
			time.Sleep(20 * time.Millisecond)
			f.BroadcastFrames(wire.Frames{Sessions: &wire.SessionsPayload{Sessions: []wire.Session{{ID: "s", Alive: true, Status: &wire.Status{Working: false}}}}})
		}()
		r, to := awaitTurnCentral(context.Background(), f, ch, "s", time.After(time.Second))
		if r != "idle" || to {
			t.Fatalf("%q %v", r, to)
		}
	})
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
	}{{"already idle", wire.Session{ID: "s", Alive: true, Status: &wire.Status{Working: false}}, 200, "idle"}, {"dead arrival", wire.Session{ID: "s", Alive: false, StartedAt: "x"}, 200, "died"}, {"no phantom death timeout", wire.Session{ID: "s", Alive: false}, 408, ""}} {
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
	f := wf(wire.Session{ID: "s", Alive: true, Status: &wire.Status{Working: false}})
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
