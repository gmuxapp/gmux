package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestSendPromptDeliversTheRunnerContract(t *testing.T) {
	var gotPath, gotCT string
	var gotBody promptBody
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.cleanup()
	if err := SendPrompt(context.Background(), srv.socketPath, "inc-1", "review this\nbranch", "after_turn", "any", ""); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if gotPath != "/prompt" || gotCT != "application/json" {
		t.Fatalf("path=%q ct=%q", gotPath, gotCT)
	}
	if gotBody.Prompt != "review this\nbranch" || gotBody.Delivery != "after_turn" || gotBody.Require != "any" {
		t.Fatalf("body=%+v", gotBody)
	}
}

// Unknown enum values are NOT validated client-side: a value this daemon does
// not know must reach a runner that may, and be refused loudly there.
func TestSendPromptPassesEnumsThroughVerbatim(t *testing.T) {
	var got promptBody
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.cleanup()
	if err := SendPrompt(context.Background(), srv.socketPath, "inc-1", "x", "tomorrow", "whenever", ""); err != nil {
		t.Fatal(err)
	}
	if got.Delivery != "tomorrow" || got.Require != "whenever" {
		t.Fatalf("rewritten: %+v", got)
	}
}

func TestSendCancelSendsNoBody(t *testing.T) {
	var length int64 = -1
	var method, path string
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		length, method, path = int64(len(raw)), r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.cleanup()
	if err := SendCancel(context.Background(), srv.socketPath, "inc-1"); err != nil {
		t.Fatal(err)
	}
	if length != 0 || method != http.MethodPost || path != "/cancel" {
		t.Fatalf("len=%d %s %s", length, method, path)
	}
}

// Every semantic call names the runner it was decided about, and a runner that
// refuses because it is somebody else reports a guaranteed non-delivery rather
// than an opaque 409 refusal.
func TestSemanticCallsAreConditionalOnTheRunnersIdentity(t *testing.T) {
	t.Run("the expectation travels with the request", func(t *testing.T) {
		got := make(chan string, 2)
		srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got <- r.Header.Get(expectIncarnationHeader)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.cleanup()
		if err := SendPrompt(context.Background(), srv.socketPath, "inc-A", "x", "now", "inactive", ""); err != nil {
			t.Fatal(err)
		}
		if err := SendCancel(context.Background(), srv.socketPath, "inc-A"); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if h := <-got; h != "inc-A" {
				t.Fatalf("%s = %q, want inc-A", expectIncarnationHeader, h)
			}
		}
	})
	t.Run("a mismatch is a non-delivery, not a refusal", func(t *testing.T) {
		srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			// The LITERAL the runner sends, not this package's constant:
			// mutating codeIncarnationMismatch must fail here rather than
			// silently stop recognizing the runner's refusal.
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "incarnation_mismatch", "error": "owned by a different runner"})
		}))
		defer srv.cleanup()
		err := SendPrompt(context.Background(), srv.socketPath, "inc-A", "x", "now", "inactive", "")
		if !errors.Is(err, ErrRunnerIncarnationMismatch) {
			t.Fatalf("SendPrompt = %v, want ErrRunnerIncarnationMismatch", err)
		}
		if errors.Is(err, ErrRunnerSemanticActionsUnsupported) {
			t.Fatal("a mismatch is not a version fact")
		}
	})
	t.Run("an unconditional call is refused locally", func(t *testing.T) {
		reached := false
		srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.cleanup()
		if err := SendPrompt(context.Background(), srv.socketPath, "", "x", "now", "inactive", ""); err == nil {
			t.Fatal("an unconditional prompt must not be sent at all")
		}
		if err := SendCancel(context.Background(), srv.socketPath, ""); err == nil {
			t.Fatal("an unconditional cancel must not be sent at all")
		}
		if reached {
			t.Fatal("bytes reached a runner nobody had identified")
		}
	})
}

// An old runner has no semantic routes. Every status here means "this runner
// cannot do this", which is a version fact the daemon turns into
// runner_outdated -- not a refusal.
func TestOldRunnerStatusesBecomeUnsupported(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired, http.StatusNotImplemented} {
		srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		err := SendPrompt(context.Background(), srv.socketPath, "inc-1", "x", "now", "inactive", "")
		srv.cleanup()
		if !errors.Is(err, ErrRunnerSemanticActionsUnsupported) {
			t.Fatalf("status %d: %v", code, err)
		}
		var actErr *RunnerActionError
		if errors.As(err, &actErr) {
			t.Fatalf("status %d must not look like a structured refusal", code)
		}
	}
}

// A runner that understands the request and refuses it hands back a stable
// code, which must survive the trip unchanged: it encodes whether bytes were
// delivered, which nothing else can know.
func TestStructuredRefusalIsPreserved(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		msg    string
	}{
		{http.StatusConflict, "precondition_failed", "agent is active"},
		{http.StatusConflict, "delivery_pending", "a delivered prompt has not produced a turn yet"},
		{http.StatusGatewayTimeout, "not_ready", "agent did not report readiness"},
		{http.StatusUnprocessableEntity, "unsupported_adapter", "adapter has no semantic actions"},
		{http.StatusInternalServerError, "transport_error", "short write; delivery is indeterminate"},
	} {
		srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "error": tc.msg})
		}))
		err := SendCancel(context.Background(), srv.socketPath, "inc-1")
		srv.cleanup()
		var actErr *RunnerActionError
		if !errors.As(err, &actErr) {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if actErr.Status != tc.status || actErr.Code != tc.code || actErr.Message != tc.msg {
			t.Fatalf("%s: got %+v", tc.code, actErr)
		}
		if errors.Is(err, ErrRunnerSemanticActionsUnsupported) {
			t.Fatalf("%s: refusal must not read as unsupported", tc.code)
		}
	}
}

// A refusal without a parseable code still surfaces the runner's words rather
// than being flattened into a generic transport failure.
func TestUnparseableRefusalKeepsTheRunnersWords(t *testing.T) {
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.cleanup()
	err := SendPrompt(context.Background(), srv.socketPath, "inc-1", "x", "now", "inactive", "")
	var actErr *RunnerActionError
	if !errors.As(err, &actErr) {
		t.Fatalf("want structured error, got %v", err)
	}
	if actErr.Code != "" || actErr.Message != "not json at all" || actErr.Status != 400 {
		t.Fatalf("got %+v", actErr)
	}
	if actErr.Error() == "" {
		t.Fatal("Error() must say something")
	}
}

// The semantic routes must NOT inherit the 3 s control-call timeout: the runner
// legitimately blocks there for the adapter readiness window (10 s for pi).
// Cutting it off would turn a slow admission into an indeterminate transport
// error, which forbids a safe retry.
func TestSemanticCallsAreBoundedOnlyByContext(t *testing.T) {
	srv := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(runnerRequestTimeout + 500*time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.cleanup()
	if err := SendCancel(context.Background(), srv.socketPath, "inc-1"); err != nil {
		t.Fatalf("slow-but-successful delivery failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := SendPrompt(ctx, srv.socketPath, "inc-1", "x", "now", "inactive", ""); err == nil {
		t.Fatal("caller deadline must bound the call")
	}
}

func TestUnreachableRunnerIsATransportFailure(t *testing.T) {
	err := SendCancel(context.Background(), "/nonexistent/gmux-test.sock", "inc-1")
	if err == nil {
		t.Fatal("want error")
	}
	var actErr *RunnerActionError
	if errors.As(err, &actErr) || errors.Is(err, ErrRunnerSemanticActionsUnsupported) {
		t.Fatalf("a dial failure is neither a refusal nor a version fact: %v", err)
	}
}
