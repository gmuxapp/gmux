package main

import (
	"net/http"
	"testing"
)

// TestClassifyConversationResponse pins the CLI half of the tail
// fallback contract (the daemon half lives in gmuxd's conversation
// handler tests): only a 200 serves markdown; a 404 falls back to PTY
// scrollback unless the daemon explicitly says the *session* is gone;
// and — crucially for version skew — a plain non-JSON 404 from an
// older gmuxd or peer that predates the /conversation endpoint also
// falls back, so `gmux tail` keeps working instead of erroring.
func TestClassifyConversationResponse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   convOutcome
	}{
		{"200 serves markdown", http.StatusOK, "## User\n\nhi\n", convServe},
		{"no_conversation falls back to scrollback", http.StatusNotFound,
			`{"ok":false,"error":{"code":"no_conversation","message":"session has no conversation file"}}`, convFallback},
		{"session not_found is an error, not a fallback", http.StatusNotFound,
			`{"ok":false,"error":{"code":"not_found","message":"session not found"}}`, convError},
		{"plain 404 from an old daemon falls back", http.StatusNotFound,
			"404 page not found\n", convFallback},
		{"500 is an error", http.StatusInternalServerError,
			`{"ok":false,"error":{"code":"internal","message":"conversation render failed"}}`, convError},
		{"400 is an error", http.StatusBadRequest,
			`{"ok":false,"error":{"code":"bad_request","message":"tail must be a positive integer"}}`, convError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyConversationResponse(tt.status, []byte(tt.body)); got != tt.want {
				t.Errorf("classifyConversationResponse(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}

// TestErrorCode covers the envelope extractor the classification
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
