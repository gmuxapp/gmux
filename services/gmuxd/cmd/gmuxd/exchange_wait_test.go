package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func TestObservationalPromptWaitIgnoresInstructionsUntilSourceClose(t *testing.T) {
	outcomes := make(chan sessioncoord.Outcome, 3)
	current := &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 4, Exchanges: []sessioncoord.TurnExchange{{User: "ask", Iterations: 1}, {User: "follow"}}}}
	result := make(chan agentWaitResult, 1)
	go func() {
		result <- runAgentWait(context.Background(), outcomes, "s", agentWaitSpec{baselineActive: true, wait: true, generation: 1, observedSeq: 4, frame: func() *sessioncoord.TurnFrame { return current }, generationLost: func() bool { return false }}, func(time.Duration) <-chan time.Time { return make(chan time.Time) })
	}()
	// A frame-only user boundary must not resolve the observer.
	outcomes <- sessioncoord.Outcome{ID: centralstore.SessionID("s"), Type: sessioncoord.OutcomeActivity, Frame: current}
	select {
	case r := <-result:
		t.Fatalf("instruction resolved wait: %+v", r)
	case <-time.After(20 * time.Millisecond):
	}
	closed := &sessioncoord.TurnFrame{Last: &sessioncoord.TurnClose{TurnSeq: 4, Outcome: outcomeCompleted, Output: "done", Exchanges: current.Current.Exchanges}}
	outcomes <- sessioncoord.Outcome{ID: centralstore.SessionID("s"), Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Frame: closed, Session: &centralstore.Session{ID: "s", StatusReported: true, Active: false}}
	select {
	case r := <-result:
		if r.Outcome != outcomeCompleted || r.Close == nil || len(r.Close.Exchanges) != 2 {
			t.Fatalf("%+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("source close did not resolve")
	}
}

func TestRunnerLossPartialRefusesStaleExchange(t *testing.T) {
	frame := &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 9, Exchanges: []sessioncoord.TurnExchange{{Ordinal: 1, User: "repeat"}}}}
	close := runnerLossClose(frame)
	if close == nil || close.Output != "" || len(close.Exchanges) != 1 || close.Diagnostic != "" {
		t.Fatalf("%+v", close)
	}
}

func TestActiveExchangeReconciliationBeforeAndAfterPersistence(t *testing.T) {
	one := 1
	current := &sessioncoord.TurnCurrent{PreviousExchanges: &one, Exchanges: []sessioncoord.TurnExchange{{Ordinal: 1, User: "repeat"}}}
	old := adapter.Exchange{Ordinal: 1, User: "repeat", Iterations: 1, Terminal: "old answer"}
	before := reconcileActiveExchanges([]adapter.Exchange{old}, current)
	if len(before) != 2 || before[0].Terminal != "old answer" || before[1].User != "repeat" {
		t.Fatalf("before persistence=%+v", before)
	}
	persisted := []adapter.Exchange{old, {Ordinal: 2, User: "repeat"}}
	after := reconcileActiveExchanges(persisted, current)
	if len(after) != 2 || after[0].Terminal != "old answer" || after[1].Terminal != "" {
		t.Fatalf("after persistence=%+v", after)
	}
}

func TestActiveExchangeReconciliationAnonymousHookFrameDoesNotDuplicateBoundary(t *testing.T) {
	native := []adapter.Exchange{{Ordinal: 1, User: "real persisted prompt", Iterations: 1, Terminal: "stale"}}
	current := &sessioncoord.TurnCurrent{Exchanges: []sessioncoord.TurnExchange{{User: ""}}}
	got := reconcileActiveExchanges(native, current)
	if len(got) != 1 || got[0].User != "real persisted prompt" || got[0].Terminal != "" {
		t.Fatalf("anonymous hook frame duplicated or corrupted boundary: %+v", got)
	}
	if got := reconcileActiveExchanges(nil, current); len(got) != 0 {
		t.Fatalf("anonymous pre-persistence frame became an empty user exchange: %+v", got)
	}
}

func TestActiveExchangeReconciliationEvictionAndHookLag(t *testing.T) {
	zero := 0
	var native []adapter.Exchange
	for i := 1; i <= 71; i++ {
		native = append(native, adapter.Exchange{User: fmt.Sprintf("u%d", i), Iterations: i})
	}
	native[70].Terminal = "hook-lag prose"
	current := &sessioncoord.TurnCurrent{PreviousExchanges: &zero, OmittedExchanges: 5}
	for i := 6; i <= 70; i++ {
		current.Exchanges = append(current.Exchanges, sessioncoord.TurnExchange{Ordinal: uint64(i), User: fmt.Sprintf("u%d", i), Iterations: 1000 + i})
	}
	got := reconcileActiveExchanges(native, current)
	if len(got) != 71 || got[4].User != "u5" || got[4].Iterations != 5 || got[5].User != "u6" || got[5].Iterations != 1006 || got[69].Iterations != 1070 || got[70].User != "u71" {
		t.Fatalf("rollover/hook-lag reconciliation len=%d head=%+v overlay=%+v tail=%+v", len(got), got[4], got[5], got[len(got)-1])
	}
	if got[70].Terminal != "" {
		t.Fatalf("active native tail retained terminal prose: %+v", got[70])
	}
}

func TestRunAgentWaitRetainedSchedules(t *testing.T) {
	row := func(active, failed bool) *centralstore.Session {
		return &centralstore.Session{ID: "s", StatusReported: true, Active: active, Error: failed}
	}
	outcomes := make(chan sessioncoord.Outcome, 4)
	result := make(chan agentWaitResult, 1)
	current := &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 4, Exchanges: []sessioncoord.TurnExchange{{Ordinal: 1, User: "ask"}}}}
	go func() {
		result <- runAgentWait(context.Background(), outcomes, "s", agentWaitSpec{baselineActive: true, wait: true, generation: 1, observedSeq: 4,
			frame: func() *sessioncoord.TurnFrame { return current }, generationLost: func() bool { return false }}, func(time.Duration) <-chan time.Time { return make(chan time.Time) })
	}()
	// A retry/rate-limit error while active and a foreign generation are not closes.
	outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Session: row(true, true)}
	outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 2, Session: row(false, true)}
	select {
	case got := <-result:
		t.Fatalf("premature resolution: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	// Two closes between observations: turn identity mismatch is result-free.
	current = &sessioncoord.TurnFrame{Last: &sessioncoord.TurnClose{TurnSeq: 5, Outcome: outcomeCompleted, Output: "foreign"}}
	outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Frame: current, Session: row(false, false)}
	got := <-result
	if got.Outcome != outcomeCompleted || got.Close != nil {
		t.Fatalf("mismatch must be result-free: %+v", got)
	}
}

func TestRunAgentWaitAdmissionAndNonFrameSchedules(t *testing.T) {
	t.Run("pre-status and coalesced frame before active", func(t *testing.T) {
		outcomes := make(chan sessioncoord.Outcome, 3)
		result := make(chan agentWaitResult, 1)
		go func() {
			result <- runAgentWait(context.Background(), outcomes, "s", agentWaitSpec{requireAcceptance: true, wait: true, generation: 1,
				frame: func() *sessioncoord.TurnFrame { return nil }, generationLost: func() bool { return false }, admission: time.Hour},
				func(time.Duration) <-chan time.Time { return make(chan time.Time) })
		}()
		outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Session: &centralstore.Session{ID: "s"}}
		open := &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 7, Exchanges: []sessioncoord.TurnExchange{{Ordinal: 1, User: "ask"}}}}
		outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Frame: open, Session: &centralstore.Session{ID: "s", StatusReported: true, Active: true}}
		closed := &sessioncoord.TurnFrame{Last: &sessioncoord.TurnClose{TurnSeq: 7, Outcome: outcomeCompleted, Output: "done"}}
		outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Frame: closed, Session: &centralstore.Session{ID: "s", StatusReported: true}}
		got := <-result
		if got.Admission != admissionAccepted || got.Close == nil || got.Close.Output != "done" {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("non-frame adapter closes result-free", func(t *testing.T) {
		outcomes := make(chan sessioncoord.Outcome, 1)
		outcomes <- sessioncoord.Outcome{ID: "s", Type: sessioncoord.OutcomeUpserted, Alive: true, Generation: 1, Session: &centralstore.Session{ID: "s", StatusReported: true}}
		got := runAgentWait(context.Background(), outcomes, "s", agentWaitSpec{baselineActive: true, wait: true, generation: 1,
			frame: func() *sessioncoord.TurnFrame { return nil }, generationLost: func() bool { return false }}, func(time.Duration) <-chan time.Time { return make(chan time.Time) })
		if got.Outcome != outcomeCompleted || got.Close != nil {
			t.Fatalf("%+v", got)
		}
	})
}

func TestLateFailedExchangeCarriesPartialAndDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	body := "{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"ask\"}}\n" +
		"{\"type\":\"message\",\"message\":{\"role\":\"assistant\",\"content\":\"half\"}}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	writeLateExchangeWait(rec, centralstore.Session{Adapter: "pi", ConversationRef: path, StatusReported: true, Error: true},
		&sessioncoord.TurnFrame{Last: &sessioncoord.TurnClose{Outcome: outcomeError, Diagnostic: "provider failed"}})
	got := rec.Body.String()
	if !strings.Contains(got, `"terminal_partial":true`) || !strings.Contains(got, `"diagnostic":"provider failed"`) {
		t.Fatalf("late response=%s", got)
	}
}

func TestOldRunnerCloseResponsePreservesUnboundedOutput(t *testing.T) {
	rec := httptest.NewRecorder()
	writeWaitConclusion(rec, httptest.NewRequest(http.MethodPost, "/wait", nil), nil, "s", waitConclusion{Reason: "idle", Outcome: outcomeCompleted},
		&sessioncoord.TurnClose{TurnSeq: 1, Outcome: outcomeCompleted, Trigger: "old ask", Output: "old answer", Truncated: true}, 0)
	got := rec.Body.String()
	for _, want := range []string{`"trigger":"old ask"`, `"output":"old answer"`, `"truncated":true`, `"outcome":"completed"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("old close response missing %s: %s", want, got)
		}
	}
}

func TestLateSnapshotHasExplicitCurrentWireOutcome(t *testing.T) {
	for _, tc := range []struct{ name, content, exchange string }{
		{"virgin", "", `"exchanges":null`},
		{"history after reset", "{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"ask\"}}\n", `"user":"ask"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conv.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			writeLateExchangeWait(rec, centralstore.Session{Adapter: "pi", ConversationRef: path}, nil)
			if got := rec.Body.String(); !strings.Contains(got, `"outcome":"snapshot"`) || !strings.Contains(got, tc.exchange) {
				t.Fatalf("snapshot response=%s", got)
			}
		})
	}
}

func TestConversationEndpointActivePersistenceWindow(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	path := filepath.Join(t.TempDir(), "conv.jsonl")
	old := "{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"repeat\"}}\n" +
		"{\"type\":\"message\",\"message\":{\"role\":\"assistant\",\"content\":\"old answer\"}}\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertSession(ctx, centralstore.NewSession{ID: "active", Adapter: "pi", ConversationRef: path, Command: []string{"pi"}, Active: true, StatusReported: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	one := 1
	frame := &sessioncoord.TurnFrame{Current: &sessioncoord.TurnCurrent{TurnSeq: 2, PreviousExchanges: &one, Exchanges: []sessioncoord.TurnExchange{{Ordinal: 1, User: "repeat"}}}}
	oldRetained := retainedTurnFrame
	retainedTurnFrame = func(*Bootstrap, string) *sessioncoord.TurnFrame { return frame }
	t.Cleanup(func() { retainedTurnFrame = oldRetained })
	boot := &Bootstrap{Store: st}
	read := func(tail int) string {
		rec := httptest.NewRecorder()
		conversationHandlerCentral(rec, httptest.NewRequest(http.MethodGet, "/conversation?tail="+fmt.Sprint(tail), nil), "active", boot)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if got := read(1); strings.Contains(got, "old answer") || strings.Count(got, "[USER]: repeat") != 1 || !strings.Contains(got, "[Agent active") {
		t.Fatalf("pre-persistence n1=%q", got)
	}
	if got := read(2); !strings.Contains(got, "[Agent worked for 1 iteration]") || strings.Count(got, "[USER]: repeat") != 2 {
		t.Fatalf("pre-persistence n2=%q", got)
	}
	if err := os.WriteFile(path, []byte(old+"{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"repeat\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(2); strings.Count(got, "[USER]: repeat") != 2 {
		t.Fatalf("post-persistence duplicated boundary=%q", got)
	}
}

func TestConversationExchangeEndpointMatrix(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	conv := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(conv, []byte("{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"one\"}}\n{\"type\":\"message\",\"message\":{\"role\":\"assistant\",\"content\":\"a\"}}\n{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"two\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	codexConv := filepath.Join(dir, "codex.jsonl")
	if err := os.WriteFile(codexConv, []byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"inspect this\"}]}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"observed\"}]}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []centralstore.NewSession{
		{ID: "pi", Adapter: "pi", ConversationRef: conv, Command: []string{"pi"}, CreatedAt: 1},
		{ID: "codex", Adapter: "codex", ConversationRef: codexConv, Command: []string{"codex"}, CreatedAt: 1},
		{ID: "shell", Adapter: "shell", Command: []string{"sh"}, CreatedAt: 1},
	} {
		if _, _, err := st.InsertSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	boot := &Bootstrap{Store: st}
	check := func(id, raw string, want int) string {
		rec := httptest.NewRecorder()
		conversationHandlerCentral(rec, httptest.NewRequest(http.MethodGet, "/conversation"+raw, nil), id, boot)
		if rec.Code != want {
			t.Fatalf("%s%s=%d body=%s", id, raw, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := check("pi", "?tail=1", 200); !strings.Contains(body, "[1 previous exchange]") || !strings.Contains(body, "[USER]: two") {
		t.Fatalf("tail report=%q", body)
	}
	if body := check("codex", "", 200); !strings.Contains(body, "[USER]: inspect this") || !strings.Contains(body, "observed") {
		t.Fatalf("codex report=%q", body)
	}
	check("pi", "?tail=0", 400)
	check("pi", "?types=user", 400)
	check("missing", "", 404)
	check("shell", "", 422)
	if err := os.WriteFile(conv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if body := check("pi", "", 200); body != "[No exchanges yet]\n" {
		t.Fatalf("empty=%q", body)
	}
}
