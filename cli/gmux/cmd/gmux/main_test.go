package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfiguredGmuxdAddr(t *testing.T) {
	tests := []struct {
		name string
		port string
		want string
	}{
		{"default", "", "http://localhost:8790"},
		{"custom port", "9999", "http://localhost:9999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GMUXD_PORT", tt.port)
			if got := configuredGmuxdAddr(); got != tt.want {
				t.Errorf("configuredGmuxdAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}

// healthHandler returns an HTTP handler that serves /v1/health with the given version.
func healthHandler(ver string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"service": "gmuxd",
				"version": ver,
				"status":  "ready",
			},
		})
	})
	return mux
}

func TestGmuxdNeedsStart_NotRunning(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	// No server → not running → needsStart, no replace.
	needsStart, needsReplace := gmuxdNeedsStart("http://127.0.0.1:1")
	if !needsStart {
		t.Error("expected needsStart=true when daemon is unreachable")
	}
	if needsReplace {
		t.Error("expected needsReplace=false when daemon is unreachable")
	}
}

func TestGmuxdNeedsStart_SameVersion(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	srv := httptest.NewServer(healthHandler("0.4.4"))
	defer srv.Close()

	needsStart, needsReplace := gmuxdNeedsStart(srv.URL)
	if needsStart {
		t.Error("expected needsStart=false when versions match")
	}
	if needsReplace {
		t.Error("expected needsReplace=false when versions match")
	}
}

func TestGmuxdNeedsStart_OlderVersion(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	srv := httptest.NewServer(healthHandler("0.4.3"))
	defer srv.Close()

	needsStart, needsReplace := gmuxdNeedsStart(srv.URL)
	if !needsStart {
		t.Error("expected needsStart=true when daemon is older")
	}
	if !needsReplace {
		t.Error("expected needsReplace=true when daemon is older")
	}
}

func TestGmuxdNeedsStart_NewerVersion(t *testing.T) {
	old := version
	version = "0.4.3"
	defer func() { version = old }()

	// Daemon is newer than us — still replace so versions match.
	srv := httptest.NewServer(healthHandler("0.4.4"))
	defer srv.Close()

	needsStart, needsReplace := gmuxdNeedsStart(srv.URL)
	if !needsStart {
		t.Error("expected needsStart=true when versions differ")
	}
	if !needsReplace {
		t.Error("expected needsReplace=true when versions differ")
	}
}

func TestGmuxdNeedsStart_DevNeverReplaces(t *testing.T) {
	old := version
	version = "dev"
	defer func() { version = old }()

	// Daemon running with a release version, but we're dev — should not replace.
	srv := httptest.NewServer(healthHandler("0.4.3"))
	defer srv.Close()

	needsStart, needsReplace := gmuxdNeedsStart(srv.URL)
	if needsStart {
		t.Error("expected needsStart=false for dev build when daemon is healthy")
	}
	if needsReplace {
		t.Error("dev builds must never replace")
	}
}

func TestGmuxdNeedsStart_DevStartsWhenNotRunning(t *testing.T) {
	old := version
	version = "dev"
	defer func() { version = old }()

	needsStart, needsReplace := gmuxdNeedsStart("http://127.0.0.1:1")
	if !needsStart {
		t.Error("expected needsStart=true for dev build when daemon is not running")
	}
	if needsReplace {
		t.Error("dev builds must never replace")
	}
}

func TestGmuxdNeedsStart_UnparseableHealth(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	// Server returns 200 but garbage body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	needsStart, needsReplace := gmuxdNeedsStart(srv.URL)
	if needsStart {
		t.Error("expected needsStart=false when health is unparseable (leave it alone)")
	}
	if needsReplace {
		t.Error("expected needsReplace=false when health is unparseable")
	}
}

func TestGmuxdNeedsStart_Non200(t *testing.T) {
	old := version
	version = "0.4.4"
	defer func() { version = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	needsStart, needsReplace := gmuxdNeedsStart(srv.URL)
	if !needsStart {
		t.Error("expected needsStart=true when health returns non-200")
	}
	if needsReplace {
		t.Error("expected needsReplace=false when health returns non-200 (treat as not running)")
	}
}
