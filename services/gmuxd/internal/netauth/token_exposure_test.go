package netauth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAuthTokenNotExposedOnTCP verifies the critical security property:
// the health endpoint must include the auth token when accessed via Unix
// socket (local IPC), but must NOT include it when accessed via TCP
// (even with valid authentication).
//
// This is the core invariant of the new architecture. If this test fails,
// the auth token can leak to network clients.
func TestAuthTokenNotExposedOnTCP(t *testing.T) {
	const authToken = "secret-token-do-not-leak-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Build a health handler that mirrors the real one's gating logic.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"service": "gmuxd",
			"version": "test",
			"status":  "ready",
			"listen":  "127.0.0.1:8790",
		}
		// Same gating logic as main.go: include token only on Unix socket.
		if r.RemoteAddr == "@" || strings.HasPrefix(r.RemoteAddr, "/") || r.RemoteAddr == "" {
			data["auth_token"] = authToken
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})

	// Start Unix socket listener (no auth, like the real daemon).
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "gmuxd.sock")
	sockLn, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	sockSrv := &http.Server{Handler: mux}
	go sockSrv.Serve(sockLn)
	defer sockSrv.Close()

	// Start TCP listener (with netauth middleware, like the real daemon).
	authedHandler := Middleware(authToken, mux)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpSrv := &http.Server{Handler: authedHandler}
	go tcpSrv.Serve(tcpLn)
	defer tcpSrv.Close()

	time.Sleep(50 * time.Millisecond)

	// Unix socket: auth_token MUST be present.
	t.Run("unix socket includes auth_token", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		}
		resp, err := client.Get("http://localhost/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var health struct {
			Data map[string]any `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&health)

		tok, ok := health.Data["auth_token"]
		if !ok {
			t.Fatal("auth_token missing from Unix socket health response")
		}
		if tok != authToken {
			t.Errorf("auth_token = %q, want %q", tok, authToken)
		}
	})

	// TCP with valid bearer: auth_token MUST NOT be present.
	t.Run("tcp omits auth_token even with valid bearer", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://"+tcpLn.Addr().String()+"/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+authToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		var health struct {
			Data map[string]any `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&health)

		if _, ok := health.Data["auth_token"]; ok {
			t.Fatal("auth_token MUST NOT be present in TCP health response — token would leak to network clients")
		}
	})

	// TCP without auth: must get 401 (can't even reach the health handler).
	t.Run("tcp without auth cannot reach health", func(t *testing.T) {
		resp, err := http.Get("http://" + tcpLn.Addr().String() + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 401 {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	// Cleanup
	os.Remove(sockPath)
}
