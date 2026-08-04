package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestTailIsAlwaysRaw pins the post-relocation contract: tail requests
// PTY scrollback and nothing else, for every kind of session. It never
// touches /conversation, so no session's adapter can change the shape of
// its output — the property that made the old adapter-dependent default
// unscriptable.
func TestTailIsAlwaysRaw(t *testing.T) {
	d := startStubDaemon(t, localSession()) // an agent (pi) session
	d.on(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("$ ls\nfile.txt\n"))
	})
	stdout := captureStdout(t, func() {
		if code := cmdTail("1va8lvdv", 42); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if stdout != "$ ls\nfile.txt\n" {
		t.Errorf("stdout = %q", stdout)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var sawScrollback, sawRead bool
	for _, r := range d.requests {
		if r.path == "/v1/sessions/1va8lvdv/scrollback" && r.query == "tail=42" {
			sawScrollback = true
		}
		if r.path == "/v1/sessions/1va8lvdv/read" {
			sawRead = true
		}
		if strings.Contains(r.path, "conversation") {
			t.Errorf("tail must never read the conversation, got %s", r.path)
		}
	}
	if !sawScrollback || !sawRead {
		t.Fatalf("requests = %+v, want scrollback followed by read acknowledgement", d.requests)
	}
}

func TestTailDelayedAcknowledgementCannotClearNewerCompletion(t *testing.T) {
	sessions := localSession()
	sessions[0].UnreadToken = "turn-1"
	d := startStubDaemon(t, sessions)
	currentToken := "turn-1"
	d.on(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scrollback"):
			_, _ = w.Write([]byte("turn one\n"))
			// N+1 completes after tail observed N's bytes and token but before
			// the command sends its acknowledgement.
			currentToken = "turn-2"
		case strings.HasSuffix(r.URL.Path, "/read"):
			if r.URL.Query().Get("token") != "turn-1" || currentToken != "turn-2" {
				t.Fatalf("unexpected read schedule: %s current token=%s", r.URL.String(), currentToken)
			}
			writeErrEnvelope(w, http.StatusConflict, "result_changed", "newer result")
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	var out strings.Builder
	if code := cmdTailTo("1va8lvdv", 10, &out); code != 0 || out.String() != "turn one\n" {
		t.Fatalf("exit=%d output=%q", code, out.String())
	}
	if currentToken != "turn-2" {
		t.Fatal("delayed tail acknowledgement cleared the newer result")
	}
}

func TestTailOutputFailurePreservesUnread(t *testing.T) {
	d := startStubDaemon(t, localSession())
	d.on(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/scrollback") {
			_, _ = w.Write([]byte("result\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if code := cmdTailTo("1va8lvdv", 10, failingOutputWriter{}); code == 0 {
		t.Fatal("tail succeeded despite output failure")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, req := range d.requests {
		if strings.HasSuffix(req.path, "/read") {
			t.Fatalf("failed tail consumed unread: %+v", d.requests)
		}
	}
}

// TestTailRejectsRemovedViewFlags: the flags that used to select tail's
// view are refused BY NAME with the replacement spelled out, in the
// spirit of the top-level removedFlags shim — an "unknown flag -e" would
// leave a caller (or a script) guessing where the transcript went.
func TestTailRejectsRemovedViewFlags(t *testing.T) {
	for _, args := range [][]string{
		{"tail", "--raw", "abc"},
		{"tail", "-r", "abc"},
		{"tail", "-e", "abc"},
		{"tail", "abc", "--raw"},      // after the ref too
		{"tail", "--raw=true", "abc"}, // and with a value attached
	} {
		_, err := parseCLI(args)
		if err == nil {
			t.Fatalf("parseCLI(%v) = nil error", args)
		}
		if !strings.Contains(err.Error(), "gmux agent logs") {
			t.Errorf("parseCLI(%v) error %q must name the replacement", args, err)
		}
		if !strings.Contains(err.Error(), "always the raw") {
			t.Errorf("parseCLI(%v) error %q must say tail is now always raw", args, err)
		}
		// The error belongs to tail's help page, like every other verb mistake.
		var ue *usageError
		if !errors.As(err, &ue) || ue.topic != "tail" {
			t.Errorf("parseCLI(%v) error is not tagged with tail's topic: %v", args, err)
		}
	}
	// -n keeps working, and still means lines.
	c, err := parseCLI([]string{"tail", "-n", "5", "abc"})
	if err != nil || c.tailLines != 5 {
		t.Fatalf("parseCLI(tail -n 5 abc) = (%+v, %v)", c, err)
	}
}

// TestErrorCode covers the envelope extractor the agent error taxonomy
// depends on: a well-formed gmuxd envelope yields its code, anything
// else yields "".
func TestErrorCode(t *testing.T) {
	if got := errorCode([]byte(`{"ok":false,"error":{"code":"no_conversation","message":"x"}}`)); got != "no_conversation" {
		t.Errorf("want no_conversation, got %q", got)
	}
	if got := errorCode([]byte("404 page not found\n")); got != "" {
		t.Errorf("non-JSON body: want empty code, got %q", got)
	}
	if got := errorCode([]byte(`{"ok":true,"data":{}}`)); got != "" {
		t.Errorf("success envelope: want empty code, got %q", got)
	}
}
