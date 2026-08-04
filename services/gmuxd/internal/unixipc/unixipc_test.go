package unixipc

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestListenCreatesSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")

	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestListenCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subdir", "nested")
	sockPath := filepath.Join(dir, "gmuxd.sock")

	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("directory permissions = %o, want 0700", info.Mode().Perm())
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")

	// Create a stale socket file.
	os.WriteFile(sockPath, []byte("stale"), 0o644)

	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Should be a socket now, not a regular file.
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Error("expected socket file type")
	}
}

func TestListenerCloseRemovesOwnedSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("socket file should be removed when its owner closes")
	}
}

func TestOldListenerCleanupPreservesSuccessor(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	old, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the historical socket-steal interleaving: the old listener is
	// still alive, but its pathname is removed and rebound by a successor.
	if err := os.Remove(sockPath); err != nil {
		t.Fatal(err)
	}
	successor, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()

	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("old-owner cleanup unlinked successor: %v", err)
	}
	conn.Close()
}

func TestClientConnectsToSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"version": "1.0.0"},
		})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	// Wait for server to be ready.
	time.Sleep(50 * time.Millisecond)

	client := Client(sockPath)
	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestOneShotClientsCloseConnections(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	var open atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"version": "1.0.0", "pid": 42}})
	})
	srv := &http.Server{Handler: mux, ConnState: func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}}
	go srv.Serve(ln)
	defer srv.Close()

	for range 100 {
		if _, ok := HealthIdentity(sockPath); !ok {
			t.Fatal("health probe failed")
		}
		deadline := time.Now().Add(time.Second)
		for open.Load() != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("open unix IPC connections=%d, want 0", open.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestHealthyReturnsFalseWhenNoSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "nonexistent.sock")
	if Healthy(sockPath) {
		t.Error("expected Healthy to return false for nonexistent socket")
	}
}

func TestHealthVersionReturnsVersion(t *testing.T) {
	sockPath, cleanup := startTestDaemon(t, "0.9.0")
	defer cleanup()

	ver, ok := HealthVersion(sockPath)
	if !ok {
		t.Fatal("expected healthy")
	}
	if ver != "0.9.0" {
		t.Errorf("version = %q, want %q", ver, "0.9.0")
	}
	identity, ok := HealthIdentity(sockPath)
	if !ok || identity.Version != "0.9.0" || identity.PID != 4242 {
		t.Fatalf("identity = %+v, ok=%v", identity, ok)
	}
}

func TestShutdownStopsDaemon(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		go func() {
			_ = srv.Close() // closing the owned listener performs pinned cleanup
		}()
	})
	go srv.Serve(ln)

	time.Sleep(50 * time.Millisecond)

	if !Shutdown(sockPath) {
		t.Fatal("expected Shutdown to succeed")
	}

	if Healthy(sockPath) {
		t.Error("daemon should not be healthy after shutdown")
	}
}

func TestReplaceNoDaemon(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	if err := Replace(sockPath); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceStaleFileDefersRemovalUntilListen(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	os.WriteFile(sockPath, []byte("stale"), 0o644)

	if err := Replace(sockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("Replace should not open a pre-bind removal race: %v", err)
	}
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

func startTestDaemon(t *testing.T, version string) (sockPath string, cleanup func()) {
	t.Helper()
	sockPath = filepath.Join(t.TempDir(), "gmuxd.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"version": version, "pid": 4242, "status": "ready"},
		})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	time.Sleep(50 * time.Millisecond)

	return sockPath, func() {
		srv.Close()
		os.Remove(sockPath)
	}
}

func TestSocketNotAccessibleByOtherUsers(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	// No group or other permissions.
	if perm&0o077 != 0 {
		t.Errorf("socket has group/other permissions: %o", perm)
	}
}

// Verify the directory for the socket is also locked down.
func TestSocketDirectoryPermissions(t *testing.T) {
	base := t.TempDir()
	sockDir := filepath.Join(base, "state", "gmux")
	sockPath := filepath.Join(sockDir, "gmuxd.sock")

	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	info, err := os.Stat(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0o700 {
		t.Errorf("directory permissions = %o, want 0700", perm)
	}
}

// Make sure a second listener can never unlink a live owner.
func TestListenFailsIfSocketInUse(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")

	ln1, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()

	if ln2, err := Listen(sockPath); err == nil {
		ln2.Close()
		t.Fatal("Listen replaced a live socket owner")
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("original listener pathname was disturbed: %v", err)
	}
	conn.Close()
}

func TestHealthIdentityRejectsInvalidContract(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":       `not-json`,
		"empty object":    `{}`,
		"empty data":      `{"data":{}}`,
		"missing pid":     `{"data":{"version":"1.0.0"}}`,
		"missing version": `{"data":{"pid":42}}`,
		"blank version":   `{"data":{"version":" ","pid":42}}`,
		"invalid pid":     `{"data":{"version":"1.0.0","pid":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
			ln, err := Listen(sockPath)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})}
			go srv.Serve(ln)
			defer srv.Close()

			if id, ok := HealthIdentity(sockPath); ok {
				t.Fatalf("invalid health contract accepted as identity: %+v", id)
			}
			if !SocketOwned(sockPath) {
				t.Fatal("invalid identity must not erase proof of a live socket owner")
			}
		})
	}
}

func TestShutdownTimeoutIsFailureAndPreservesOwner(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // deliberately keep serving
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	if shutdownWithin(sockPath, 250*time.Millisecond) {
		t.Fatal("shutdown deadline reported success while incumbent still owned socket")
	}
	if !SocketOwned(sockPath) {
		t.Fatal("shutdown timeout disturbed the incumbent")
	}
}
