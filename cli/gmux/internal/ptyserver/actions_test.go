package ptyserver

// Tests for the runner's semantic agent actions (ADR 0027): readiness gating,
// POST /prompt, POST /cancel, and the guarantee that raw POST /input is
// untouched by any of it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// --- fakes ------------------------------------------------------------------

// fakeAgent is a minimal adapter with configurable semantic-action support. It
// exists so readiness-timeout tests don't have to wait out pi's real 10s policy
// and so "adapter cannot express this action" is reachable (pi expresses all
// three).
type fakeAgent struct {
	encode  map[adapter.AgentAction]string
	timeout time.Duration
}

func (f *fakeAgent) Name() string                      { return "fake" }
func (f *fakeAgent) Discover() bool                    { return true }
func (f *fakeAgent) Match(command []string) bool       { return false }
func (f *fakeAgent) Env(adapter.EnvContext) []string   { return nil }
func (f *fakeAgent) ActionReadyTimeout() time.Duration { return f.timeout }
func (f *fakeAgent) EncodeAction(a adapter.AgentAction) (string, bool) {
	in, ok := f.encode[a]
	return in, ok
}

// plainAdapter implements no semantic actions at all.
type plainAdapter struct{ fakeAgent }

func (plainAdapter) Name() string { return "plain" }

// recorder captures every byte handed to the semantic transport, together with
// the write boundaries — the ordering guarantee is about write *count*, so the
// boundaries have to be observable.
type recorder struct {
	mu      sync.Mutex
	writes  [][]byte
	err     error
	short   bool // report a truncated write
	onWrite func(p []byte)
}

func (rec *recorder) write(p []byte) (int, error) {
	rec.mu.Lock()
	onWrite := rec.onWrite
	failure := rec.err
	short := rec.short
	if failure == nil {
		rec.writes = append(rec.writes, append([]byte(nil), p...))
	}
	rec.mu.Unlock()
	// Fired with the write "in flight" — i.e. after the runner committed and
	// before the transport call returns. That is the window in which a real
	// agent's turn start can race the write, so it is where tests inject one.
	if onWrite != nil {
		onWrite(p)
	}
	if failure != nil {
		return 0, failure
	}
	if short {
		return len(p) - 1, nil
	}
	return len(p), nil
}

// setOnWrite installs the in-flight hook.
func (rec *recorder) setOnWrite(fn func([]byte)) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.onWrite = fn
}

func (rec *recorder) snapshot() [][]byte {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([][]byte, len(rec.writes))
	copy(out, rec.writes)
	return out
}

func (rec *recorder) bytes() string {
	var sb strings.Builder
	for _, w := range rec.snapshot() {
		sb.Write(w)
	}
	return sb.String()
}

// --- harness ----------------------------------------------------------------

type actionFixture struct {
	srv    *Server
	state  *session.State
	rec    *recorder
	client *http.Client
}

// observedCancelContext exposes when code under test has started observing its
// cancellation channel. It lets cancellation tests synchronize on the actual
// wait instead of guessing with sleeps or an HTTP client's return timing.
type observedCancelContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedCancelContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

type responseWriteTracker struct {
	header http.Header
	wrote  bool
}

func (w *responseWriteTracker) Header() http.Header { return w.header }
func (w *responseWriteTracker) WriteHeader(int)     { w.wrote = true }
func (w *responseWriteTracker) Write(p []byte) (int, error) {
	w.wrote = true
	return len(p), nil
}

// newActionFixture starts a runner with a long-lived, silent child and swaps the
// semantic transport for a recorder. The child is real (the runner needs a PTY)
// but never reads: what we assert is what the runner delivers, not what an
// agent does with it.
func newActionFixture(t *testing.T, ad adapter.Adapter) *actionFixture {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "s1", Adapter: ad.Name(), SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 30"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    ad,
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	rec := &recorder{}
	srv.deliverBytes = rec.write
	return &actionFixture{
		srv:   srv,
		state: st,
		rec:   rec,
		client: &http.Client{
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			}},
			Timeout: 20 * time.Second,
		},
	}
}

// post sends a request to the runner and returns status plus the decoded error
// code (empty for a 204). It names this runner's incarnation, which the
// semantic routes require (see requireIncarnation); the mismatch and
// missing-header cases are exercised explicitly instead.
func (f *actionFixture) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	resp, err := f.postAs(f.srv.Incarnation(), path, body)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, ""
	}
	var payload struct{ Code, Error string }
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, payload.Code
}

// postAs sends a semantic request naming an arbitrary expected incarnation.
// An empty want omits the header entirely.
func (f *actionFixture) postAs(want, path, body string) (*http.Response, error) {
	req, err := http.NewRequest("POST", "http://session"+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if want != "" {
		req.Header.Set(ExpectIncarnationHeader, want)
	}
	return f.client.Do(req)
}

// postExpect posts naming an arbitrary expected incarnation and decodes the
// error code, like post.
func (f *actionFixture) postExpect(t *testing.T, want, path, body string) (int, string) {
	t.Helper()
	resp, err := f.postAs(want, path, body)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, ""
	}
	var payload struct{ Code, Error string }
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, payload.Code
}

// put sends a PUT to the runner (used for the child self-report channel).
func (f *actionFixture) put(t *testing.T, path, body string) {
	t.Helper()
	req, err := http.NewRequest("PUT", "http://session"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	resp.Body.Close()
}

func (f *actionFixture) ready(t *testing.T) {
	t.Helper()
	postSessionEvent(t, f.srv.sockPath, `{"op":"ready"}`)
	deadline := time.After(3 * time.Second)
	for !f.srv.ready() {
		select {
		case <-deadline:
			t.Fatal("runner never became ready after {\"op\":\"ready\"}")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func piFixture(t *testing.T) *actionFixture { return newActionFixture(t, adapters.NewPi()) }

// promptFor builds a POST /prompt request addressed to srv's incarnation, for
// the tests that drive a server they built themselves rather than a fixture.
func promptFor(t *testing.T, srv *Server, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "http://session/prompt", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ExpectIncarnationHeader, srv.Incarnation())
	return req
}

func TestSuccessfulRawInputConsumesUnread(t *testing.T) {
	f := piFixture(t)
	f.state.SetUnread(true)
	code, _ := f.post(t, "/input", "next")
	if code != http.StatusNoContent || f.state.UnreadSnapshot() {
		t.Fatalf("input = %d unread=%v", code, f.state.UnreadSnapshot())
	}
}

func TestReadEndpointConsumesUnreadForExactRunner(t *testing.T) {
	f := piFixture(t)
	f.state.SetUnread(true)
	first := f.state.UnreadToken
	code, ec := f.post(t, "/read?token="+first, "")
	if code != http.StatusNoContent || ec != "" || f.state.UnreadSnapshot() {
		t.Fatalf("read = %d/%s unread=%v", code, ec, f.state.UnreadSnapshot())
	}
	f.state.SetUnread(true)
	second := f.state.UnreadToken
	if first == second {
		t.Fatal("completion reused an unread token")
	}
	code, _ = f.post(t, "/read?token="+first, "")
	if code != http.StatusConflict || !f.state.UnreadSnapshot() {
		t.Fatal("delayed first-token acknowledgement consumed the second result")
	}
	code, _ = f.postExpect(t, "another-runner", "/read?token="+second, "")
	if code != http.StatusConflict || !f.state.UnreadSnapshot() {
		t.Fatal("incarnation mismatch consumed unread")
	}
}

func TestSuccessfulCancelPreservesUnread(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	f.state.SetStatus(&adapter.Status{Active: true})
	f.state.SetUnread(true)
	code, ec := f.post(t, "/cancel", "")
	if code != http.StatusNoContent || ec != "" {
		t.Fatalf("cancel = %d/%s", code, ec)
	}
	if !f.state.UnreadSnapshot() {
		t.Fatal("cancel consumed an earlier unread result")
	}
}

func TestSuccessfulPromptConsumesUnread(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	f.state.SetUnread(true)
	code, ec := f.post(t, "/prompt", `{"prompt":"next","delivery":"now","require":"inactive"}`)
	if code != http.StatusNoContent || ec != "" {
		t.Fatalf("prompt = %d/%s", code, ec)
	}
	if f.state.UnreadSnapshot() {
		t.Fatal("successful prompt delivery must consume the previous result")
	}
}

// --- readiness --------------------------------------------------------------

// TestReadyHookEventIsIdempotentAndUnblocksAll pins the three properties the
// semantic layer depends on: a ready event marks the runner ready, repeats are
// harmless, and every waiter (not one) is released.
func TestReadyHookEventIsIdempotentAndUnblocksAll(t *testing.T) {
	f := piFixture(t)
	if f.srv.ready() {
		t.Fatal("a fresh runner must start unready")
	}

	const waiters = 4
	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			errs <- f.srv.awaitReady(context.Background(), 5*time.Second)
		}()
	}
	// Deliberately more ready events than waiters, so a signal-one
	// implementation could not be rescued by the extra events either.
	f.ready(t)
	f.ready(t)
	for range waiters {
		if err := <-errs; err != nil {
			t.Fatalf("waiter did not observe readiness: %v", err)
		}
	}
	if !f.srv.ready() {
		t.Fatal("repeated ready events must not un-ready the runner")
	}
}

// TestReadyDoesNotRequireAConversation is the deadlock regression: a fresh
// agent has no conversation file yet, and readiness must not be gated on one.
// A ready event alone has to admit a prompt.
func TestReadyDoesNotRequireAConversation(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	if ref := f.state.ConversationRefSnapshot(); ref != "" {
		t.Fatalf("fixture unexpectedly has a bound conversation %q", ref)
	}
	if code, ec := f.post(t, "/prompt", `{"prompt":"hi","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("prompt on a ready-but-unbound session = %d/%s, want 204", code, ec)
	}
}

// TestReadinessSurvivesConversationRebind: readiness describes the agent
// process's composer, not the conversation it holds. A rebind (pi's
// switch/new/resume/fork) is the same process, so it must not un-ready.
func TestReadinessSurvivesConversationRebind(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	postSessionEvent(t, f.srv.sockPath, `{"op":"session","path":"/tmp/a.jsonl"}`)
	postSessionEvent(t, f.srv.sockPath, `{"op":"session","path":"/tmp/b.jsonl"}`)
	if !f.srv.ready() {
		t.Fatal("a conversation rebind must not reset runner readiness")
	}
}

// TestNotReadyBlocksThenTimesOutWithoutDelivering is the core safety property:
// a readiness timeout is a promise that nothing was sent, which is what makes
// the caller's retry safe.
func TestNotReadyBlocksThenTimesOutWithoutDelivering(t *testing.T) {
	f := newActionFixture(t, &fakeAgent{
		timeout: 150 * time.Millisecond,
		encode:  map[adapter.AgentAction]string{adapter.ActionSend: "\r"},
	})
	start := time.Now()
	code, ec := f.post(t, "/prompt", `{"prompt":"hi","delivery":"now","require":"inactive"}`)
	elapsed := time.Since(start)
	if code != http.StatusGatewayTimeout || ec != CodeNotReady {
		t.Fatalf("prompt on an unready agent = %d/%s, want 504/%s", code, ec, CodeNotReady)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned after %v: the request must actually wait for readiness", elapsed)
	}
	if n := len(f.rec.snapshot()); n != 0 {
		t.Errorf("readiness timeout delivered %d writes (%q); it must deliver nothing", n, f.rec.bytes())
	}
}

// TestReadyUnblocksAPendingPrompt: the wait is a wait, not a poll of a
// pre-existing flag — a prompt issued before readiness must proceed the moment
// readiness arrives.
func TestReadyUnblocksAPendingPrompt(t *testing.T) {
	f := newActionFixture(t, &fakeAgent{
		timeout: 5 * time.Second,
		encode:  map[adapter.AgentAction]string{adapter.ActionSend: "\r"},
	})
	done := make(chan int, 1)
	go func() {
		code, _ := f.post(t, "/prompt", `{"prompt":"hi","delivery":"now","require":"inactive"}`)
		done <- code
	}()
	// Let the request reach the readiness wait, then release it.
	time.Sleep(100 * time.Millisecond)
	if n := len(f.rec.snapshot()); n != 0 {
		t.Fatalf("bytes delivered before readiness: %q", f.rec.bytes())
	}
	f.ready(t)
	select {
	case code := <-done:
		if code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 once ready", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readiness did not unblock the pending prompt")
	}
	if got := f.rec.bytes(); got != "hi\r" {
		t.Errorf("delivered %q, want %q", got, "hi\r")
	}
}

// --- delivery matrix --------------------------------------------------------

// TestPromptMatrix walks delivery × require × activity, asserting both the
// outcome and the exact bytes. The encodings come from the real pi adapter, so
// the runner cannot drift from what the adapter says a keystroke is.
func TestPromptMatrix(t *testing.T) {
	pi := adapters.NewPi()
	send, _ := pi.EncodeAction(adapter.ActionSend)
	afterTurn, _ := pi.EncodeAction(adapter.ActionSendAfterTurn)

	for _, tc := range []struct {
		name     string
		status   *adapter.Status
		body     string
		wantCode int
		wantErr  string
		wantOut  string
	}{
		{
			name: "plain prompt on an inactive agent", status: nil,
			body:     `{"prompt":"go","delivery":"now","require":"inactive"}`,
			wantCode: http.StatusNoContent, wantOut: "go" + send,
		},
		{
			name: "plain prompt on an active agent is refused", status: &adapter.Status{Active: true},
			body:     `{"prompt":"go","delivery":"now","require":"inactive"}`,
			wantCode: http.StatusConflict, wantErr: CodePrecondition,
		},
		{
			name: "steer needs an active turn", status: &adapter.Status{Active: true},
			body:     `{"prompt":"also this","delivery":"now","require":"active"}`,
			wantCode: http.StatusNoContent, wantOut: "also this" + send,
		},
		{
			name: "steer on an inactive agent is refused", status: &adapter.Status{},
			body:     `{"prompt":"also this","delivery":"now","require":"active"}`,
			wantCode: http.StatusConflict, wantErr: CodePrecondition,
		},
		{
			name: "follow-up while active queues after the turn", status: &adapter.Status{Active: true},
			body:     `{"prompt":"later","delivery":"after_turn","require":"any"}`,
			wantCode: http.StatusNoContent, wantOut: "later" + afterTurn,
		},
		{
			name: "follow-up while inactive still delivers", status: nil,
			body:     `{"prompt":"later","delivery":"after_turn","require":"any"}`,
			wantCode: http.StatusNoContent, wantOut: "later" + afterTurn,
		},
		{
			// Active+Error is an active turn (a retry/rate-limit condition),
			// so a plain prompt must be refused exactly like plain Active.
			name: "active+error counts as active", status: &adapter.Status{Active: true, Error: true},
			body:     `{"prompt":"go","delivery":"now","require":"inactive"}`,
			wantCode: http.StatusConflict, wantErr: CodePrecondition,
		},
		{
			// An interrupted turn is over. The interruption is durable state
			// about the past, and must not make the agent look busy.
			name: "interrupted counts as inactive", status: &adapter.Status{Interrupted: true},
			body:     `{"prompt":"go","delivery":"now","require":"inactive"}`,
			wantCode: http.StatusNoContent, wantOut: "go" + send,
		},
		{
			name: "terminal error counts as inactive", status: &adapter.Status{Error: true},
			body:     `{"prompt":"go","delivery":"now","require":"inactive"}`,
			wantCode: http.StatusNoContent, wantOut: "go" + send,
		},
		{
			name: "require any ignores activity", status: &adapter.Status{Active: true},
			body:     `{"prompt":"go","delivery":"now","require":"any"}`,
			wantCode: http.StatusNoContent, wantOut: "go" + send,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			if tc.status != nil {
				f.state.SetStatus(tc.status)
			}
			code, ec := f.post(t, "/prompt", tc.body)
			if code != tc.wantCode || ec != tc.wantErr {
				t.Fatalf("status/code = %d/%q, want %d/%q", code, ec, tc.wantCode, tc.wantErr)
			}
			if got := f.rec.bytes(); got != tc.wantOut {
				t.Fatalf("delivered %q, want %q", got, tc.wantOut)
			}
		})
	}
}

// TestPromptIsOneOrderedVerbatimWrite pins the anti-early-submit property: a
// multiline prompt and its submit keystroke leave the runner as a single
// ordered write, and the prompt text is passed through byte-for-byte — never
// interpreted as key tokens, even when it contains an "Enter" or an escape
// sequence.
func TestPromptIsOneOrderedVerbatimWrite(t *testing.T) {
	pi := adapters.NewPi()
	send, _ := pi.EncodeAction(adapter.ActionSend)
	prompt := "line one\nline two\ttabbed\r\x1b[13;3u literal enter and CSI-u \x1b"

	f := piFixture(t)
	f.ready(t)
	body, err := json.Marshal(map[string]string{"prompt": prompt, "delivery": "now", "require": "inactive"})
	if err != nil {
		t.Fatal(err)
	}
	if code, ec := f.post(t, "/prompt", string(body)); code != http.StatusNoContent {
		t.Fatalf("status = %d/%s, want 204", code, ec)
	}
	writes := f.rec.snapshot()
	if len(writes) != 1 {
		t.Fatalf("delivered in %d writes, want exactly 1 (a split write lets a multiline prompt submit early): %q", len(writes), f.rec.bytes())
	}
	if got, want := string(writes[0]), prompt+send; got != want {
		t.Fatalf("delivered %q, want %q (prompt verbatim, then the action)", got, want)
	}
}

// --- cancel -----------------------------------------------------------------

func TestCancelRequiresAnActiveTurn(t *testing.T) {
	pi := adapters.NewPi()
	interrupt, _ := pi.EncodeAction(adapter.ActionInterrupt)

	t.Run("active", func(t *testing.T) {
		f := piFixture(t)
		f.ready(t)
		f.state.SetStatus(&adapter.Status{Active: true})
		if code, ec := f.post(t, "/cancel", ""); code != http.StatusNoContent {
			t.Fatalf("cancel on an active turn = %d/%s, want 204", code, ec)
		}
		if got := f.rec.bytes(); got != interrupt {
			t.Fatalf("delivered %q, want the interrupt encoding %q", got, interrupt)
		}
		// Cancel returns on delivery: it must not wait for the agent to
		// acknowledge, so the status is still active here.
		if st := f.state.StatusSnapshot(); st == nil || !st.Active {
			t.Error("cancel must not synthesize an inactive status of its own")
		}
	})

	t.Run("inactive", func(t *testing.T) {
		f := piFixture(t)
		f.ready(t)
		if code, ec := f.post(t, "/cancel", ""); code != http.StatusConflict || ec != CodePrecondition {
			t.Fatalf("cancel on an idle agent = %d/%s, want 409/%s", code, ec, CodePrecondition)
		}
		if n := len(f.rec.snapshot()); n != 0 {
			t.Errorf("refused cancel delivered %q", f.rec.bytes())
		}
	})

	t.Run("unready", func(t *testing.T) {
		f := newActionFixture(t, &fakeAgent{
			timeout: 100 * time.Millisecond,
			encode:  map[adapter.AgentAction]string{adapter.ActionInterrupt: "\x1b[27u"},
		})
		f.state.SetStatus(&adapter.Status{Active: true})
		if code, ec := f.post(t, "/cancel", ""); code != http.StatusGatewayTimeout || ec != CodeNotReady {
			t.Fatalf("cancel on an unready agent = %d/%s, want 504/%s", code, ec, CodeNotReady)
		}
		if n := len(f.rec.snapshot()); n != 0 {
			t.Errorf("cancel delivered %q despite the readiness timeout", f.rec.bytes())
		}
	})
}

func TestCancelRejectsABody(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	f.state.SetStatus(&adapter.Status{Active: true})
	if code, ec := f.post(t, "/cancel", `{"prompt":"stop"}`); code != http.StatusBadRequest || ec != CodeInvalidRequest {
		t.Fatalf("cancel with a body = %d/%s, want 400/%s", code, ec, CodeInvalidRequest)
	}
	if n := len(f.rec.snapshot()); n != 0 {
		t.Errorf("rejected cancel delivered %q", f.rec.bytes())
	}
	// An empty JSON object is the same as no body: some clients cannot POST
	// nothing.
	if code, ec := f.post(t, "/cancel", `{}`); code != http.StatusNoContent {
		t.Fatalf("cancel with {} = %d/%s, want 204", code, ec)
	}
}

// --- unsupported adapters / actions -----------------------------------------

func TestUnsupportedAdaptersAndActions(t *testing.T) {
	t.Run("adapter without semantic actions", func(t *testing.T) {
		for _, ad := range []adapter.Adapter{adapters.NewClaude(), adapters.NewCodex(), adapters.NewShell()} {
			f := newActionFixture(t, ad)
			f.ready(t) // ready, so the refusal is about capability and nothing else
			code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"now","require":"any"}`)
			if code != http.StatusUnprocessableEntity || ec != CodeUnsupportedAdapter {
				t.Errorf("%s prompt = %d/%s, want 422/%s", ad.Name(), code, ec, CodeUnsupportedAdapter)
			}
			if code, ec := f.post(t, "/cancel", ""); code != http.StatusUnprocessableEntity || ec != CodeUnsupportedAdapter {
				t.Errorf("%s cancel = %d/%s, want 422/%s", ad.Name(), code, ec, CodeUnsupportedAdapter)
			}
			if n := len(f.rec.snapshot()); n != 0 {
				t.Errorf("%s: an unsupported adapter delivered %q", ad.Name(), f.rec.bytes())
			}
		}
	})

	t.Run("adapter missing one action", func(t *testing.T) {
		// Supports plain send only: follow-up and cancel must fail loudly
		// rather than fall back to Enter.
		f := newActionFixture(t, &fakeAgent{
			timeout: time.Second,
			encode:  map[adapter.AgentAction]string{adapter.ActionSend: "\r"},
		})
		f.ready(t)
		if code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"after_turn","require":"any"}`); code != http.StatusUnprocessableEntity || ec != CodeUnsupportedAction {
			t.Errorf("follow-up = %d/%s, want 422/%s", code, ec, CodeUnsupportedAction)
		}
		f.state.SetStatus(&adapter.Status{Active: true})
		if code, ec := f.post(t, "/cancel", ""); code != http.StatusUnprocessableEntity || ec != CodeUnsupportedAction {
			t.Errorf("cancel = %d/%s, want 422/%s", code, ec, CodeUnsupportedAction)
		}
		if n := len(f.rec.snapshot()); n != 0 {
			t.Errorf("unsupported actions delivered %q", f.rec.bytes())
		}
	})
}

// --- request validation -----------------------------------------------------

// TestPromptValidation pins strictness: nothing unknown is ever defaulted into
// a command that runs, and nothing invalid reaches the PTY.
func TestPromptValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"not JSON", `not json`},
		{"empty body", ``},
		{"empty prompt", `{"prompt":"","delivery":"now","require":"inactive"}`},
		{"missing prompt", `{"delivery":"now","require":"inactive"}`},
		{"missing delivery", `{"prompt":"go","require":"inactive"}`},
		{"missing require", `{"prompt":"go","delivery":"now"}`},
		{"unknown delivery", `{"prompt":"go","delivery":"eventually","require":"inactive"}`},
		{"unknown require", `{"prompt":"go","delivery":"now","require":"idle"}`},
		{"unknown field", `{"prompt":"go","delivery":"now","require":"any","mode":"steer"}`},
		{"wrong type", `{"prompt":42,"delivery":"now","require":"any"}`},
		{"trailing object", `{"prompt":"go","delivery":"now","require":"any"}{"prompt":"and this"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			code, ec := f.post(t, "/prompt", tc.body)
			if code != http.StatusBadRequest || ec != CodeInvalidRequest {
				t.Fatalf("%s = %d/%s, want 400/%s", tc.name, code, ec, CodeInvalidRequest)
			}
			if n := len(f.rec.snapshot()); n != 0 {
				t.Errorf("invalid request delivered %q", f.rec.bytes())
			}
		})
	}
}

// TestPromptSizeLimits pins the two separate limits and the boundary between
// them: the limit that matters is on the DECODED prompt (what gets delivered),
// while the envelope has its own, looser cap because JSON escaping expands a
// byte up to six-fold. Capping the envelope at the prompt limit would reject
// legitimate prompts full of control characters.
func TestPromptSizeLimits(t *testing.T) {
	body := func(prompt string) string {
		b, err := json.Marshal(map[string]string{"prompt": prompt, "delivery": "now", "require": "any"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	t.Run("exactly at the prompt limit is accepted", func(t *testing.T) {
		f := piFixture(t)
		f.ready(t)
		prompt := strings.Repeat("x", maxPromptBytes)
		if code, ec := f.post(t, "/prompt", body(prompt)); code != http.StatusNoContent {
			t.Fatalf("prompt of exactly %d bytes = %d/%s, want 204", maxPromptBytes, code, ec)
		}
		if got := f.rec.bytes(); got != prompt+"\r" {
			t.Fatalf("delivered %d bytes, want the %d-byte prompt plus the action", len(got), len(prompt))
		}
	})

	t.Run("one byte over the prompt limit is refused", func(t *testing.T) {
		f := piFixture(t)
		f.ready(t)
		code, ec := f.post(t, "/prompt", body(strings.Repeat("x", maxPromptBytes+1)))
		if code != http.StatusBadRequest || ec != CodeInvalidRequest {
			t.Fatalf("prompt of %d bytes = %d/%s, want 400/%s", maxPromptBytes+1, code, ec, CodeInvalidRequest)
		}
		if n := len(f.rec.snapshot()); n != 0 {
			t.Errorf("an over-limit prompt delivered %d writes; it must not be truncated into a shorter one", n)
		}
	})

	t.Run("escaping expansion stays inside the envelope", func(t *testing.T) {
		// 700 KiB of control characters is under the prompt limit but encodes
		// to ~4.2 MiB of JSON — well past the prompt limit, well inside the
		// envelope limit. This is the case a single shared limit would break.
		f := piFixture(t)
		f.ready(t)
		prompt := strings.Repeat("\x01", 700*1024)
		env := body(prompt)
		if len(env) <= maxPromptBytes {
			t.Fatalf("test setup: envelope is only %d bytes, not past the prompt limit", len(env))
		}
		if code, ec := f.post(t, "/prompt", env); code != http.StatusNoContent {
			t.Fatalf("%d-byte prompt in a %d-byte envelope = %d/%s, want 204", len(prompt), len(env), code, ec)
		}
	})

	t.Run("an oversized envelope is refused", func(t *testing.T) {
		f := piFixture(t)
		f.ready(t)
		// Raw padding past the envelope cap, without a valid prompt: the point
		// is that the body is refused on size before anything is parsed.
		huge := `{"prompt":"` + strings.Repeat("x", maxPromptEnvelopeBytes) + `","delivery":"now","require":"any"}`
		code, ec := f.post(t, "/prompt", huge)
		if code != http.StatusBadRequest || ec != CodeInvalidRequest {
			t.Fatalf("oversized envelope = %d/%s, want 400/%s", code, ec, CodeInvalidRequest)
		}
		if n := len(f.rec.snapshot()); n != 0 {
			t.Errorf("oversized envelope delivered %d writes", n)
		}
	})
}

// TestPromptTextIntegrity pins what happens to a prompt whose JSON encodes
// something the decoder would silently rewrite. encoding/json substitutes
// U+FFFD for invalid UTF-8 and for `\uXXXX` escapes that do not form a scalar,
// which would deliver mangled words under a 204 — worse than refusing them.
//
// The check is structural, on the caller's own `prompt` token, so it accepts
// everything legitimate: a correct surrogate PAIR, an explicit `\ufffd` escape,
// and a literal U+FFFD in the body — including a literal U+FFFD standing next
// to a genuinely broken escape, which is the case an origin-sniffing heuristic
// gets wrong.
func TestPromptTextIntegrity(t *testing.T) {
	// Raw invalid UTF-8 inside an otherwise well-formed envelope.
	invalidUTF8 := "{\"prompt\":\"a\xff b\",\"delivery\":\"now\",\"require\":\"any\"}"

	for _, tc := range []struct {
		name    string
		body    string
		want    int
		deliver string // expected delivered prompt text when accepted
	}{
		{
			name: "raw invalid UTF-8", body: invalidUTF8, want: http.StatusBadRequest,
		},
		{
			name: "unpaired high surrogate escape",
			body: `{"prompt":"a\ud83db","delivery":"now","require":"any"}`, want: http.StatusBadRequest,
		},
		{
			name: "stray low surrogate escape",
			body: `{"prompt":"a\ude00b","delivery":"now","require":"any"}`, want: http.StatusBadRequest,
		},
		{
			name: "literal U+FFFD next to an unpaired surrogate is still refused",
			body: "{\"prompt\":\"\uFFFD a\\ud83d\",\"delivery\":\"now\",\"require\":\"any\"}",
			want: http.StatusBadRequest,
		},
		{
			name: "valid surrogate pair",
			body: `{"prompt":"grin \ud83d\ude00","delivery":"now","require":"any"}`,
			want: http.StatusNoContent, deliver: "grin \U0001F600",
		},
		{
			name: "escaped replacement character",
			body: `{"prompt":"look: \ufffd","delivery":"now","require":"any"}`,
			want: http.StatusNoContent, deliver: "look: \uFFFD",
		},
		{
			name: "literal replacement character",
			body: "{\"prompt\":\"look: \uFFFD\",\"delivery\":\"now\",\"require\":\"any\"}",
			want: http.StatusNoContent, deliver: "look: \uFFFD",
		},
		{
			name: "prompt is not a string",
			body: `{"prompt":42,"delivery":"now","require":"any"}`, want: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			code, ec := f.post(t, "/prompt", tc.body)
			if code != tc.want {
				t.Fatalf("status = %d/%s, want %d", code, ec, tc.want)
			}
			if tc.want == http.StatusBadRequest {
				if ec != CodeInvalidRequest {
					t.Fatalf("code = %q, want %q", ec, CodeInvalidRequest)
				}
				if n := len(f.rec.snapshot()); n != 0 {
					t.Fatalf("delivered %q despite refusing the prompt", f.rec.bytes())
				}
				return
			}
			if got, want := f.rec.bytes(), tc.deliver+"\r"; got != want {
				t.Fatalf("delivered %q, want %q (verbatim, then the action)", got, want)
			}
		})
	}
}

func TestPromptRejectsWrongMediaType(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	req, err := http.NewRequest("POST", "http://session/prompt",
		strings.NewReader(`{"prompt":"go","delivery":"now","require":"any"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(ExpectIncarnationHeader, f.srv.Incarnation())
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if n := len(f.rec.snapshot()); n != 0 {
		t.Errorf("wrong-media-type request delivered %q", f.rec.bytes())
	}
}

// TestSemanticActionsRequireThisRunnersIncarnation pins the guarantee that a
// pathname's new occupant never executes an action decided about its
// predecessor: the daemon pins (endpoint, incarnation) and the runner refuses
// anything addressed to somebody else, before a single byte can be written.
//
// The refusal is a distinct code from every indeterminate one on purpose — it
// is the one delivery failure that is provably a non-delivery, so a caller may
// re-pin and retry without risking a duplicate prompt.
func TestSemanticActionsRequireThisRunnersIncarnation(t *testing.T) {
	// The refusal code is a wire literal the daemon matches on by spelling
	// (services/gmuxd's discovery client has its own copy — separate module, no
	// shared symbol). Renaming this constant would turn a guaranteed
	// non-delivery into an opaque 409 the daemon must treat as indeterminate, so
	// the text is pinned here rather than only used through the constant.
	if CodeIncarnationMismatch != "incarnation_mismatch" {
		t.Fatalf("CodeIncarnationMismatch = %q; the daemon matches the literal \"incarnation_mismatch\"", CodeIncarnationMismatch)
	}
	cases := []struct {
		name, path, body string
	}{
		{"prompt", "/prompt", `{"prompt":"hi","delivery":"now","require":"inactive"}`},
		{"cancel", "/cancel", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name+" for another incarnation is refused", func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			f.state.SetStatus(&adapter.Status{Active: true}) // cancel needs a turn
			code, ec := f.postExpect(t, "incarnation-of-a-dead-predecessor", tc.path, tc.body)
			if code != http.StatusConflict || ec != CodeIncarnationMismatch {
				t.Fatalf("%s = %d/%s, want 409/%s", tc.path, code, ec, CodeIncarnationMismatch)
			}
			if n := len(f.rec.snapshot()); n != 0 {
				t.Fatalf("%d writes reached the PTY, want 0: %q", n, f.rec.bytes())
			}
			if f.state.ReservationHeld() {
				t.Fatal("a refused action took a delivery reservation")
			}
		})
		t.Run(tc.name+" without the header is a caller bug", func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			f.state.SetStatus(&adapter.Status{Active: true})
			code, ec := f.postExpect(t, "", tc.path, tc.body)
			if code != http.StatusBadRequest || ec != CodeInvalidRequest {
				t.Fatalf("%s = %d/%s, want 400/%s", tc.path, code, ec, CodeInvalidRequest)
			}
			if n := len(f.rec.snapshot()); n != 0 {
				t.Fatalf("%d writes reached the PTY, want 0: %q", n, f.rec.bytes())
			}
		})
	}
	t.Run("a refusal leaves the runner usable", func(t *testing.T) {
		f := piFixture(t)
		f.ready(t)
		if code, ec := f.postExpect(t, "somebody-else", "/prompt", `{"prompt":"hi","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeIncarnationMismatch {
			t.Fatalf("first = %d/%s, want 409/%s", code, ec, CodeIncarnationMismatch)
		}
		if code, ec := f.post(t, "/prompt", `{"prompt":"hi","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
			t.Fatalf("re-pinned retry = %d/%s, want 204", code, ec)
		}
		if got := f.rec.bytes(); got != "hi\r" {
			t.Fatalf("delivered %q, want the retry's payload only", got)
		}
	})
}

// holdInQueue parks the FIRST semantic request that reaches the delivery slot
// there — slot held, nothing decided — until release is called. Later requests
// run through untouched.
//
// This is the seam for schedules that must change the agent's state while a
// request is queued: the runner then decides that request at its
// transport-start boundary, against the state it is actually delivering into.
func holdInQueue(f *actionFixture) (queued <-chan struct{}, release func()) {
	ch := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	ticket := make(chan struct{}, 1)
	ticket <- struct{}{}
	f.srv.barrier = func() {
		select {
		case <-ticket:
		default:
			return
		}
		once.Do(func() { close(ch) })
		<-rel
	}
	return ch, func() { close(rel) }
}

// holdInWrite parks the FIRST semantic delivery inside its transport write —
// committed, bytes recorded, call not yet returned — until release is called.
//
// That models both a blocked PTY write and the causal race the reservation
// phases exist for: an agent whose hook reports its turn start before the
// runner's write returns.
func holdInWrite(f *actionFixture) (writing <-chan struct{}, release func()) {
	ch := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	ticket := make(chan struct{}, 1)
	ticket <- struct{}{}
	f.rec.setOnWrite(func([]byte) {
		select {
		case <-ticket:
		default:
			return
		}
		once.Do(func() { close(ch) })
		<-rel
	})
	return ch, func() { close(rel) }
}

// --- concurrency and schedules ----------------------------------------------

// TestConcurrentPlainPromptsAdmitOne is the race the admission/reservation
// design exists for.
//
// Every request is ready, every one sees an idle agent, and no hook ever
// reports a turn — the worst case, since the runner has nothing but its own
// reservation to go on. Exactly one may reach the PTY; the rest must be told
// that a delivery is outstanding (a distinct code from "the agent is busy").
func TestConcurrentPlainPromptsAdmitOne(t *testing.T) {
	f := piFixture(t)
	f.ready(t)

	const n = 8
	var wg sync.WaitGroup
	type result struct {
		code int
		ec   string
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, ec := f.post(t, "/prompt", fmt.Sprintf(`{"prompt":"p%d","delivery":"now","require":"inactive"}`, i))
			results <- result{code, ec}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	admitted := 0
	for r := range results {
		switch {
		case r.code == http.StatusNoContent:
			admitted++
		case r.code == http.StatusConflict && r.ec == CodeDeliveryPending:
		default:
			t.Fatalf("unexpected refusal %d/%s, want 409/%s", r.code, r.ec, CodeDeliveryPending)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d of %d concurrent plain prompts were admitted, want exactly 1", admitted, n)
	}
	if got := len(f.rec.snapshot()); got != 1 {
		t.Fatalf("%d writes reached the PTY, want 1 (%q)", got, f.rec.bytes())
	}
}

// TestQueuedRequestIsDecidedAtTheTransportBoundary drives the schedule the
// review called out, and pins the opposite of what an earlier version of this
// code did: a plain prompt that was idle-eligible when it arrived, but whose
// agent went active while it waited in the delivery queue, must be REFUSED. The
// requirement is evaluated at the transport-start boundary, so no request ever
// delivers into a state the runner had already been told about.
func TestQueuedRequestIsDecidedAtTheTransportBoundary(t *testing.T) {
	f := piFixture(t)
	f.ready(t)

	queued, release := holdInQueue(f)
	type res struct {
		code int
		ec   string
	}
	done := make(chan res, 1)
	go func() {
		code, ec := f.post(t, "/prompt", `{"prompt":"first","delivery":"now","require":"inactive"}`)
		done <- res{code, ec}
	}()
	<-queued

	// The turn starts while the request is queued. This status write must not
	// be blocked by the semantic layer: blocking status transitions behind a
	// delivery is the deadlock the design refuses.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	if st := f.state.StatusSnapshot(); st == nil || !st.Active {
		t.Fatal("the hook's turn start did not land while a request was queued")
	}
	release()

	if got := <-done; got.code != http.StatusConflict || got.ec != CodePrecondition {
		t.Fatalf("queued plain prompt = %d/%s, want 409/%s: the requirement is rechecked at the boundary",
			got.code, got.ec, CodePrecondition)
	}
	if n := len(f.rec.snapshot()); n != 0 {
		t.Fatalf("delivered %q into an agent that went active while the request waited", f.rec.bytes())
	}
}

// TestQueuedSteerIsDecidedAtTheTransportBoundary is the inverse: a steer that
// was eligible on arrival must be refused if the turn it meant to steer ends
// while it waits.
func TestQueuedSteerIsDecidedAtTheTransportBoundary(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)

	queued, release := holdInQueue(f)
	type res struct {
		code int
		ec   string
	}
	done := make(chan res, 1)
	go func() {
		code, ec := f.post(t, "/prompt", `{"prompt":"steered","delivery":"now","require":"active"}`)
		done <- res{code, ec}
	}()
	<-queued
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
	release()

	if got := <-done; got.code != http.StatusConflict || got.ec != CodePrecondition {
		t.Fatalf("queued steer = %d/%s, want 409/%s", got.code, got.ec, CodePrecondition)
	}
	if n := len(f.rec.snapshot()); n != 0 {
		t.Fatalf("delivered %q into a turn that had already ended", f.rec.bytes())
	}
}

// TestCancelWhileQueuedDeliversNothing: a request abandoned while waiting for
// the delivery slot must deliver zero bytes. That is why the slot is a
// context-aware channel and not a mutex — Mutex.Lock cannot be abandoned, and
// the request would eventually deliver a prompt nobody is waiting for any more.
func TestCancelWhileQueuedDeliversNothing(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)

	// The first delivery parks inside its write, holding the slot.
	writing, release := holdInWrite(f)
	first := make(chan int, 1)
	go func() {
		code, _ := f.post(t, "/prompt", `{"prompt":"first","delivery":"now","require":"active"}`)
		first <- code
	}()
	<-writing

	// The second request queues for the slot, then its caller hangs up.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", "http://session/prompt",
		strings.NewReader(`{"prompt":"second","delivery":"now","require":"active"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ExpectIncarnationHeader, f.srv.Incarnation())
	secondDone := make(chan error, 1)
	go func() {
		resp, err := f.client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		secondDone <- err
	}()
	time.Sleep(150 * time.Millisecond) // let it reach the slot wait
	cancel()
	if err := <-secondDone; err == nil {
		t.Fatal("the canceled request returned a response; it should have been abandoned")
	}
	// The client's error returns as soon as it closes the connection; give the
	// server's read loop its (immediate, but asynchronous) chance to observe
	// the close and cancel the handler's context before the slot frees up.
	time.Sleep(300 * time.Millisecond)

	release()
	if code := <-first; code != http.StatusNoContent {
		t.Fatalf("first prompt = %d, want 204", code)
	}
	// Give an abandoned-but-still-running handler every chance to write.
	time.Sleep(150 * time.Millisecond)
	writes := f.rec.snapshot()
	if len(writes) != 1 {
		t.Fatalf("%d writes reached the PTY, want 1 \u2014 a canceled request must deliver nothing: %q", len(writes), f.rec.bytes())
	}
	if got := string(writes[0]); !strings.HasPrefix(got, "first") {
		t.Fatalf("the surviving write was %q, want the first prompt", got)
	}
}

// TestDeliveriesAreOrderedByTheSlot: the slot exists so the ORDER of the PTY
// writes matches the order of the decisions. With one delivery parked inside
// its write, a second cannot overtake it.
func TestDeliveriesAreOrderedByTheSlot(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)

	writing, release := holdInWrite(f)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		f.post(t, "/prompt", `{"prompt":"A","delivery":"now","require":"active"}`)
	}()
	<-writing

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		f.post(t, "/prompt", `{"prompt":"B","delivery":"now","require":"active"}`)
	}()
	time.Sleep(150 * time.Millisecond)
	if n := len(f.rec.snapshot()); n != 1 {
		t.Fatalf("%d writes while one delivery held the slot, want 1", n)
	}

	release()
	<-firstDone
	<-secondDone
	if got := f.rec.bytes(); got != "A\rB\r" {
		t.Fatalf("delivered %q, want A then B in decision order", got)
	}
}

// --- reservation phases and evidence ----------------------------------------

// TestReservationIgnoresEvidencePredatingTransport: a turn that starts AND ends
// while a plain prompt is queued is somebody else's turn. It must not be
// consumed as evidence that the queued prompt was accepted, or the next prompt
// would be admitted while the first one still sits unanswered in the composer.
func TestReservationIgnoresEvidencePredatingTransport(t *testing.T) {
	f := piFixture(t)
	f.ready(t)

	queued, release := holdInQueue(f)
	done := make(chan int, 1)
	go func() {
		code, _ := f.post(t, "/prompt", `{"prompt":"mine","delivery":"now","require":"inactive"}`)
		done <- code
	}()
	<-queued
	// A full unrelated turn, entirely before the transport boundary. At the
	// boundary the agent is idle again, so the prompt is admitted — but the
	// active edge belongs to that turn, not to this delivery.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
	release()

	if code := <-done; code != http.StatusNoContent {
		t.Fatalf("prompt = %d, want 204 (the agent is idle at the boundary)", code)
	}
	if !f.state.ReservationHeld() {
		t.Fatal("an active edge that predates the transport was consumed as evidence for this delivery")
	}
	if code, ec := f.post(t, "/prompt", `{"prompt":"second","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeDeliveryPending {
		t.Fatalf("second plain prompt = %d/%s, want 409/%s", code, ec, CodeDeliveryPending)
	}
}

// TestFollowUpReservationSurvivesRepeatedActiveWrites: only an inactive→active
// EDGE is evidence. A hook that re-reports its running turn (or a script's
// `PUT /status` mid-turn) says nothing new, and must not release the
// reservation a follow-up took for the turn its queued text will start later.
func TestFollowUpReservationSurvivesRepeatedActiveWrites(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	if code, ec := f.post(t, "/prompt", `{"prompt":"later","delivery":"after_turn","require":"any"}`); code != http.StatusNoContent {
		t.Fatalf("follow-up = %d/%s, want 204", code, ec)
	}

	// Repeated active reports during the SAME turn.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	f.put(t, "/status", `{"active":true}`)
	if !f.state.ReservationHeld() {
		t.Fatal("a repeated Active=true write released the follow-up's reservation")
	}

	// The current turn ends; the queued text has not run yet.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
	if code, ec := f.post(t, "/prompt", `{"prompt":"now","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeDeliveryPending {
		t.Fatalf("plain prompt while a follow-up is queued = %d/%s, want 409/%s", code, ec, CodeDeliveryPending)
	}

	// The next inactive→active edge IS the queued text's turn.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	if f.state.ReservationHeld() {
		t.Fatal("the queued prompt's own turn did not resolve the reservation")
	}
}

// TestEvidenceRacingTheWriteResolvesAfterASuccessfulWrite: an agent can start
// its turn — and its hook can report it — before the runner's write call
// returns. That edge is recorded while the transport is in flight and consumed
// when the write completes, so the delivery ends with no reservation left and
// the next plain prompt is admitted.
func TestEvidenceRacingTheWriteResolvesAfterASuccessfulWrite(t *testing.T) {
	f := piFixture(t)
	f.ready(t)

	// The hook's turn start is delivered from inside the write call.
	f.rec.setOnWrite(func([]byte) {
		postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	})
	if code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("prompt = %d/%s, want 204", code, ec)
	}
	if f.state.ReservationHeld() {
		t.Fatal("an edge that raced the write was not consumed after the write completed")
	}
	// And the agent is genuinely active now, so a plain prompt is refused on
	// activity rather than on a leftover reservation.
	if code, ec := f.post(t, "/prompt", `{"prompt":"again","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodePrecondition {
		t.Fatalf("prompt during the started turn = %d/%s, want 409/%s", code, ec, CodePrecondition)
	}
}

// TestEvidenceRacingAFailedWriteLeavesNoReservation: same race, but the write
// fails. The reservation must be gone — its job is to prevent duplicating a
// DELIVERED prompt — and the recorded edge must not leave the session in a
// state where the next prompt is refused as pending.
func TestEvidenceRacingAFailedWriteLeavesNoReservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *actionFixture)
	}{
		{"failed write", func(f *actionFixture) { f.rec.err = errors.New("input/output error") }},
		{"short write", func(f *actionFixture) { f.rec.short = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			tc.setup(f)
			f.rec.setOnWrite(func([]byte) {
				postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
			})
			if code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"now","require":"inactive"}`); code != http.StatusInternalServerError || ec != CodeTransportError {
				t.Fatalf("%s = %d/%s, want 500/%s", tc.name, code, ec, CodeTransportError)
			}
			if f.state.ReservationHeld() {
				t.Fatalf("%s left a delivery reservation behind", tc.name)
			}
		})
	}
}

// TestReservationSurvivesUnrelatedStatusWrites: the reservation is released by
// ONE thing only — a fresh active turn. An unrelated inactive report (a
// script's `PUT /status`, a turn end, a cleared status) is not evidence that
// the delivered prompt was consumed, and re-admitting on it would duplicate a
// prompt into the same composer. There is deliberately no timeout either.
func TestReservationSurvivesUnrelatedStatusWrites(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	if code, ec := f.post(t, "/prompt", `{"prompt":"one","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("first prompt = %d/%s, want 204", code, ec)
	}

	for _, write := range []struct{ name, method, path, body string }{
		{"an idle PUT /status", "PUT", "/status", `{"active":false}`},
		{"a cleared status", "PUT", "/status", `null`},
		{"an interrupted status", "PUT", "/status", `{"active":false,"interrupted":true}`},
		{"a hook turn end", "POST", "/hook/event", `{"op":"turn","phase":"end","outcome":"completed"}`},
	} {
		req, err := http.NewRequest(write.method, "http://session"+write.path, strings.NewReader(write.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		resp.Body.Close()
		if !f.state.ReservationHeld() {
			t.Fatalf("%s released the delivery reservation", write.name)
		}
		if code, ec := f.post(t, "/prompt", `{"prompt":"two","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeDeliveryPending {
			t.Fatalf("prompt after %s = %d/%s, want 409/%s", write.name, code, ec, CodeDeliveryPending)
		}
	}

	// No timeout: waiting does not re-admit. (The old design silently
	// re-admitted after 2s, which could duplicate a prompt the agent was
	// simply slow to report.)
	time.Sleep(2100 * time.Millisecond)
	if code, ec := f.post(t, "/prompt", `{"prompt":"two","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeDeliveryPending {
		t.Fatalf("prompt after waiting = %d/%s, want 409/%s: time is not evidence", code, ec, CodeDeliveryPending)
	}

	// A late turn start — well beyond any old timeout — is the evidence, and
	// after its turn ends the next prompt is admitted normally.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	if f.state.ReservationHeld() {
		t.Fatal("a fresh active turn must release the reservation")
	}
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
	if code, ec := f.post(t, "/prompt", `{"prompt":"two","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("prompt after the delivered turn ran = %d/%s, want 204", code, ec)
	}
	if got := f.rec.bytes(); got != "one\rtwo\r" {
		t.Fatalf("delivered %q, want %q", got, "one\rtwo\r")
	}
}

// TestFollowUpReservesTheQueuedTurn: a send-after-turn delivered into a running
// turn will run later, so the plain prompt that follows must not be admitted
// when the CURRENT turn ends — only once the queued prompt's own turn is seen.
func TestFollowUpReservesTheQueuedTurn(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	if code, ec := f.post(t, "/prompt", `{"prompt":"later","delivery":"after_turn","require":"any"}`); code != http.StatusNoContent {
		t.Fatalf("follow-up = %d/%s, want 204", code, ec)
	}
	// The pre-existing turn finishes. The queued prompt has not run yet.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
	if code, ec := f.post(t, "/prompt", `{"prompt":"now","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeDeliveryPending {
		t.Fatalf("plain prompt while a follow-up is queued = %d/%s, want 409/%s", code, ec, CodeDeliveryPending)
	}
	// The queued prompt runs: its turn is the evidence.
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
	postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
	if code, ec := f.post(t, "/prompt", `{"prompt":"now","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("plain prompt after the queued turn ran = %d/%s, want 204", code, ec)
	}
}

// TestSteerAndCancelDoNotReserve: neither an action that joins a running turn
// nor one that aborts it starts a future turn, so neither may block the next
// plain prompt.
func TestSteerAndCancelDoNotReserve(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"steer", "/prompt", `{"prompt":"also","delivery":"now","require":"active"}`},
		{"send-now while busy", "/prompt", `{"prompt":"also","delivery":"now","require":"any"}`},
		{"cancel", "/cancel", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := piFixture(t)
			f.ready(t)
			postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"start"}`)
			if code, ec := f.post(t, tc.path, tc.body); code != http.StatusNoContent {
				t.Fatalf("%s = %d/%s, want 204", tc.name, code, ec)
			}
			if f.state.ReservationHeld() {
				t.Fatalf("%s reserved a future turn", tc.name)
			}
			postSessionEvent(t, f.srv.sockPath, `{"op":"turn","phase":"end","outcome":"completed"}`)
			if code, ec := f.post(t, "/prompt", `{"prompt":"next","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
				t.Fatalf("plain prompt after %s = %d/%s, want 204", tc.name, code, ec)
			}
		})
	}
}

// --- transport failures -----------------------------------------------------

// TestShortWriteIsATransportError: a truncated payload is not a delivery — the
// agent holds a prompt fragment, possibly without its submit keystroke. It must
// not be reported as 204, and it must not leave a reservation behind (the
// reservation's job is to prevent duplicating a DELIVERED prompt).
func TestShortWriteIsATransportError(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	f.rec.short = true
	code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"now","require":"inactive"}`)
	if code != http.StatusInternalServerError || ec != CodeTransportError {
		t.Fatalf("short write = %d/%s, want 500/%s", code, ec, CodeTransportError)
	}
	if f.state.ReservationHeld() {
		t.Fatal("a truncated write must not leave a delivery reservation")
	}
	f.rec.short = false
	if code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("retry after a failed delivery = %d/%s, want 204", code, ec)
	}
}

// TestPanickingTransportLeavesNoInFlightReservation: an unexpected unwind
// between the commit and the confirm/clear must not orphan a reservation. An
// orphan would be permanent — no timeout releases it and no request is left to
// resolve it — so every later plain prompt in this runner generation would be
// refused as pending. net/http recovers the panic and drops the connection; the
// session must be left exactly as it would be after a failed write.
func TestPanickingTransportLeavesNoInFlightReservation(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	// Silence the panic traceback net/http logs, so a passing test does not
	// look like a failing one.
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	f.rec.setOnWrite(func([]byte) { panic("transport exploded") })
	resp, err := f.postAs(f.srv.Incarnation(), "/prompt", `{"prompt":"go","delivery":"now","require":"inactive"}`)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected the connection to be dropped, got %d", resp.StatusCode)
	}

	if f.state.ReservationHeld() {
		t.Fatal("a panicking transport orphaned an in-flight reservation; every later prompt would be refused")
	}
	// And the runner is usable again: the delivery slot was released too.
	f.rec.setOnWrite(nil)
	if code, ec := f.post(t, "/prompt", `{"prompt":"retry","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("retry after a panicking transport = %d/%s, want 204", code, ec)
	}
	// The panicking write itself is indeterminate — this fake recorded its
	// bytes before exploding, exactly as a real write that failed part-way
	// might have. What matters is that the retry got through afterwards.
	writes := f.rec.snapshot()
	if got := string(writes[len(writes)-1]); got != "retry\r" {
		t.Fatalf("last write = %q, want the retry", got)
	}
}

// TestSuccessfulDeliveryKeepsItsReservation guards the other half of that safety
// net: the deferred abandon must NOT undo a legitimately held reservation. After
// a successful write with no qualifying edge yet, the reservation survives the
// handler returning — that is the whole point of it.
func TestSuccessfulDeliveryKeepsItsReservation(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	if code, ec := f.post(t, "/prompt", `{"prompt":"go","delivery":"now","require":"inactive"}`); code != http.StatusNoContent {
		t.Fatalf("prompt = %d/%s, want 204", code, ec)
	}
	if !f.state.ReservationHeld() {
		t.Fatal("the handler's deferred cleanup released a confirmed reservation")
	}
	if code, ec := f.post(t, "/prompt", `{"prompt":"again","delivery":"now","require":"inactive"}`); code != http.StatusConflict || ec != CodeDeliveryPending {
		t.Fatalf("second prompt = %d/%s, want 409/%s", code, ec, CodeDeliveryPending)
	}
}

// --- raw input is untouched -------------------------------------------------

// TestRawInputIgnoresReadinessAndConditions is the ADR 0027 separation, pinned
// from the runner's side: raw input is unconditional. It works on an unready
// runner, on an active agent, on an adapter with no semantic action support at
// all, and it takes no part in the pending-submit bookkeeping.
func TestRawInputIgnoresReadinessAndConditions(t *testing.T) {
	for _, ad := range []adapter.Adapter{adapters.NewPi(), adapters.NewShell()} {
		f := newActionFixture(t, ad)
		if f.srv.ready() {
			t.Fatal("fixture should start unready")
		}
		f.state.SetStatus(&adapter.Status{Active: true})
		for _, body := range []string{"typed bytes", "\r", "\x1b[27u"} {
			resp, err := f.client.Post("http://session/input", "application/octet-stream", strings.NewReader(body))
			if err != nil {
				t.Fatalf("%s: post /input: %v", ad.Name(), err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("%s: raw /input with %q = %d, want 204 regardless of readiness/activity", ad.Name(), body, resp.StatusCode)
			}
		}
		// Raw writes go straight to the PTY, never through the semantic
		// transport, and leave no pending submit behind.
		if n := len(f.rec.snapshot()); n != 0 {
			t.Errorf("%s: raw input went through the semantic transport (%q)", ad.Name(), f.rec.bytes())
		}
		if f.state.ReservationHeld() {
			t.Errorf("%s: raw input recorded a semantic delivery reservation", ad.Name())
		}
	}
}

// --- child death ------------------------------------------------------------

// TestSemanticActionOnDeadChild: a child that exits before reporting readiness
// is a guaranteed non-delivery: the runner never wrote a byte. The code must be
// CodeNotRunning (zero bytes, safe to retry), not CodeTransportError (which is
// reserved for post-write / indeterminate failures).
func TestSemanticActionOnDeadChild(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "s1", Adapter: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "exit 0"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()
	<-srv.Done()

	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", sockPath)
	}}, Timeout: 5 * time.Second}
	resp, err := client.Do(promptFor(t, srv, `{"prompt":"go","delivery":"now","require":"any"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// 409 Conflict: the child is gone and zero bytes were delivered — a
	// resource-state conflict, not a transport failure.
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (child exited before readiness = guaranteed non-delivery)", resp.StatusCode)
	}
	var payload struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	// CodeNotRunning is the stable non-delivery code for the pre-readiness
	// child-exit path. Mutating this to CodeTransportError would assign the
	// wrong delivery class (indeterminate) to a case that is provably zero.
	if payload.Code != CodeNotRunning {
		t.Fatalf("code = %q, want %q (must not be %q)", payload.Code, CodeNotRunning, CodeTransportError)
	}
}

// TestSemanticRoutesAreDistinctFromRawInput guards the compatibility story: the
// semantic paths are their own routes, so an old runner answers 404 (which the
// daemon turns into an actionable "runner outdated") instead of executing a
// semantic request as raw bytes. Here we pin the flip side — this runner serves
// them, and serves nothing semantic on /input's method/paths by accident.
func TestSemanticRoutesAreDistinctFromRawInput(t *testing.T) {
	f := piFixture(t)
	f.ready(t)
	for _, path := range []string{"/prompt", "/cancel"} {
		resp, err := f.client.Get("http://session" + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		// The mux only registers POST for these paths; a GET must not be
		// served (and must certainly not deliver anything).
		if resp.StatusCode == http.StatusNoContent {
			t.Errorf("GET %s was served like a POST", path)
		}
	}
	// A raw /input request body shaped like a semantic one is just bytes.
	resp, err := f.client.Post("http://session/input", "application/json",
		strings.NewReader(`{"prompt":"go","delivery":"now","require":"inactive"}`))
	if err != nil {
		t.Fatalf("post /input: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("raw /input = %d, want 204", resp.StatusCode)
	}
	if n := len(f.rec.snapshot()); n != 0 {
		t.Errorf("semantic-looking raw input took the semantic path (%q)", f.rec.bytes())
	}
}

// TestCancelWhileWaitingForReadinessDeliversNothing: the readiness wait is the
// other place a request can sit for seconds, so it must honour the caller
// hanging up — and, like every pre-commit exit, deliver nothing.
func TestCancelWhileWaitingForReadinessDeliversNothing(t *testing.T) {
	f := newActionFixture(t, &fakeAgent{
		timeout: 30 * time.Second, // long enough that only cancellation ends the wait
		encode:  map[adapter.AgentAction]string{adapter.ActionSend: "\r"},
	})
	base, cancel := context.WithCancel(context.Background())
	ctx := &observedCancelContext{Context: base, observed: make(chan struct{})}
	req := httptest.NewRequest("POST", "http://session/prompt",
		strings.NewReader(`{"prompt":"go","delivery":"now","require":"any"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ExpectIncarnationHeader, f.srv.Incarnation())
	response := &responseWriteTracker{header: make(http.Header)}
	done := make(chan struct{})
	go func() {
		f.srv.handlePrompt(response, req)
		close(done)
	}()

	// Wait until awaitReady has selected on this exact server-side context.
	select {
	case <-ctx.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("readiness wait never observed the request context")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not end the readiness wait")
	}
	if response.wrote {
		t.Fatal("canceled handler wrote an HTTP response")
	}
	// Becoming ready afterwards must not resurrect the completed request.
	f.ready(t)
	if n := len(f.rec.snapshot()); n != 0 {
		t.Fatalf("an abandoned request delivered %q", f.rec.bytes())
	}
}

// TestChildDeathAfterReadinessIsATransportError covers the other half of the
// death story: a runner that WAS ready and whose child then died must fail on
// the write with an indeterminate-delivery transport error, not claim it was
// never ready (which advertises "safe to retry") and not report success.
//
// This one deliberately uses the real PTY transport — the point is the actual
// write failing.
func TestChildDeathAfterReadinessIsATransportError(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	st := session.New(session.Config{ID: "s1", Adapter: "pi", SocketPath: sockPath})
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 30"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", sockPath)
	}}, Timeout: 10 * time.Second}

	postSessionEvent(t, sockPath, `{"op":"ready"}`)
	for !srv.ready() {
		time.Sleep(5 * time.Millisecond)
	}
	// Kill the child, then close the PTY the way Shutdown would, so the write
	// fails deterministically rather than depending on how quickly the kernel
	// tears down the pty pair.
	_ = srv.cmd.Process.Kill()
	<-srv.Done()
	srv.mu.Lock()
	srv.ptmxClosed = true
	srv.ptmx.Close()
	srv.mu.Unlock()

	resp, err := client.Do(promptFor(t, srv, `{"prompt":"go","delivery":"now","require":"any"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the write failed)", resp.StatusCode)
	}
	var payload struct{ Code, Error string }
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if payload.Code != CodeTransportError {
		t.Fatalf("code = %q, want %q", payload.Code, CodeTransportError)
	}
	if !strings.Contains(payload.Error, "indeterminate") {
		t.Errorf("error %q does not say delivery is indeterminate", payload.Error)
	}
	if st.ReservationHeld() {
		t.Error("a failed write left a delivery reservation behind")
	}
}
