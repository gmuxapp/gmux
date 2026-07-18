package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

type fakeRunnerServer struct {
	listener net.Listener
	server   *http.Server
	metaGate <-chan struct{}
	meta     map[string]any
}

func startFakeRunnerServer(t *testing.T, socketPath string, metaGate <-chan struct{}, meta map[string]any) *fakeRunnerServer {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, r *http.Request) {
		if metaGate != nil {
			<-metaGate
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return &fakeRunnerServer{listener: ln, server: srv, metaGate: metaGate, meta: meta}
}

func (f *fakeRunnerServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.server.Shutdown(ctx)
	_ = f.listener.Close()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

func readFirstSSEEvent(t *testing.T, sc *bufio.Scanner) (string, []byte) {
	t.Helper()
	var event string
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if event != "" {
				return event, data.Bytes()
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("no SSE event received")
	return "", nil
}

func TestServeCentralWaitsForConvergenceBeforeListenersAndServesSQLiteState(t *testing.T) {
	base, err := os.MkdirTemp("", "s5-switch-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	stateHome := filepath.Join(base, "state")
	configHome := filepath.Join(base, "config")
	home := filepath.Join(base, "home")
	for _, dir := range []string{stateHome, configHome, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("GMUX_SOCKET_DIR", filepath.Join(base, "run"))
	port := freePort(t)
	cfgDir := filepath.Join(configHome, "gmux")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("port = %d\n[discovery]\ndevcontainers = false\n[tailscale]\nenabled = false\n", port)
	if err := os.WriteFile(filepath.Join(cfgDir, "host.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	metaGate := make(chan struct{})
	runnerSock := filepath.Join(paths.SessionSocketDir(), "runner-switch.sock")
	runner := startFakeRunnerServer(t, runnerSock, metaGate, map[string]any{
		"id":             "sess-switch-test",
		"adapter":        "shell",
		"alive":          true,
		"created_at":     time.Unix(1, 0).UTC().Format(time.RFC3339),
		"pid":            4242,
		"runner_version": "dev",
		"binary_hash":    "abc123",
		"cwd":            home,
		"command":        []string{"/bin/sh"},
		"remotes":        map[string]string{},
		"status":         map[string]any{"working": true},
	})
	defer runner.Close()

	done := make(chan int, 1)
	go func() { done <- serveCentral(io.Discard) }()

	sock := paths.SocketPath()
	if unixipc.Healthy(sock) {
		t.Fatal("daemon became healthy before convergence was released")
	}
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("TCP listener accepted connections before convergence completed")
	}

	close(metaGate)
	waitUntil(t, 10*time.Second, func() bool { return unixipc.Healthy(sock) }, "daemon never became healthy after convergence")

	client := unixipc.Client(sock)
	resp, err := client.Get("http://localhost/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sessionsEnv struct {
		OK   bool `json:"ok"`
		Data struct {
			Sessions []struct {
				ID    string `json:"id"`
				Alive bool   `json:"alive"`
			} `json:"sessions"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessionsEnv); err != nil {
		t.Fatal(err)
	}
	if !sessionsEnv.OK || len(sessionsEnv.Data.Sessions) != 1 || sessionsEnv.Data.Sessions[0].ID != "sess-switch-test" || !sessionsEnv.Data.Sessions[0].Alive {
		t.Fatalf("unexpected /v1/sessions payload: %+v", sessionsEnv)
	}

	ro, err := centralstore.OpenReadOnly(context.Background(), paths.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, err := ro.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "sess-switch-test" {
		t.Fatalf("unexpected sqlite rows: %+v", rows)
	}

	// Validate through the installed production route. This deliberately
	// catches a handler that bypasses decodeProjectState/State.Validate.
	invalidProjects := `{"version":4,"items":[{"slug":"duplicate"},{"slug":"duplicate"}]}`
	putReq, err := http.NewRequest(http.MethodPut, "http://localhost/v1/projects", strings.NewReader(invalidProjects))
	if err != nil {
		t.Fatal(err)
	}
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	var putEnvelope struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(putResp.Body).Decode(&putEnvelope); err != nil {
		putResp.Body.Close()
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusBadRequest || putEnvelope.OK || putEnvelope.Error.Code != "validation_error" {
		t.Fatalf("invalid PUT status=%d envelope=%+v", putResp.StatusCode, putEnvelope)
	}
	catalog, err := ro.ReadSnapshot(context.Background(), centralstore.SnapshotQuery{IncludeProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Projects) != 0 {
		t.Fatalf("invalid PUT changed catalog: %+v", catalog.Projects)
	}

	req, err := http.NewRequest(http.MethodGet, "http://localhost/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	event, data := readFirstSSEEvent(t, scanner)
	if event != "snapshot.sessions" {
		t.Fatalf("first SSE event=%q, want snapshot.sessions", event)
	}
	var sessionsFrame struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &sessionsFrame); err != nil {
		t.Fatal(err)
	}
	if len(sessionsFrame.Sessions) != 1 || sessionsFrame.Sessions[0].ID != "sess-switch-test" {
		t.Fatalf("unexpected snapshot.sessions frame: %s", data)
	}
	event, data = readFirstSSEEvent(t, scanner)
	if event != "snapshot.world" {
		t.Fatalf("second SSE event=%q, want matched snapshot.world", event)
	}
	var worldFrame map[string]json.RawMessage
	if err := json.Unmarshal(data, &worldFrame); err != nil {
		t.Fatal(err)
	}
	if _, ok := worldFrame["projects"]; !ok {
		t.Fatalf("snapshot.world omitted projects: %s", data)
	}

	if !unixipc.Shutdown(sock) {
		t.Fatal("failed to shut daemon down")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serveCentral exit code=%d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit")
	}
}
