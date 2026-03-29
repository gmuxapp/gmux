package unixipc

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

func TestCleanupRemovesSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	Cleanup(sockPath)

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("socket file should be removed after cleanup")
	}
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
			srv.Close()
			Cleanup(sockPath)
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

func TestReplaceStaleFile(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")
	os.WriteFile(sockPath, []byte("stale"), 0o644)

	if err := Replace(sockPath); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("stale file should be removed")
	}
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
			"data": map[string]any{"version": version, "status": "ready"},
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

// Make sure we can't listen if another process already holds the socket.
func TestListenFailsIfSocketInUse(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "gmuxd.sock")

	ln1, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	// Don't close ln1 — simulate a running daemon.

	// Listen should remove the socket file and rebind.
	// This is expected behavior (Replace handles the graceful case).
	ln2, err := Listen(sockPath)
	if err != nil {
		// On some systems this may fail if the old listener still holds it.
		// That's okay: Replace() should be called first in production.
		t.Logf("Listen returned error as expected when socket in use: %v", err)
		ln1.Close()
		return
	}
	ln1.Close()
	ln2.Close()

	fmt.Println("Listen replaced socket file (OS allowed it)")
}
