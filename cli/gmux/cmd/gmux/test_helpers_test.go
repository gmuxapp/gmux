package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type stubDaemon struct {
	mu              sync.Mutex
	requests        []recordedRequest
	handler         func(http.ResponseWriter, *http.Request)
	sessionsHandler func(http.ResponseWriter, *http.Request)
	sessions        []cliSession
}
type recordedRequest struct{ method, path, query, body string }

type failingOutputWriter struct{}

func (failingOutputWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

func startStubDaemon(t *testing.T, sessions []cliSession) *stubDaemon {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "gmux")
	_ = os.MkdirAll(dir, 0700)
	ln, err := net.Listen("unix", filepath.Join(dir, "gmuxd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	d := &stubDaemon{sessions: sessions}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"version":"dev"}}`))
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		h := d.sessionsHandler
		d.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": d.sessions})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		token := ""
		for _, sess := range d.sessions {
			if strings.Contains(r.URL.Path, "/"+sess.ID+"/") {
				token = sess.UnreadToken
				break
			}
		}
		w.Header().Set(unreadTokenHeader, token)
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.requests = append(d.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
		h := d.handler
		d.mu.Unlock()
		if h == nil {
			w.WriteHeader(501)
			return
		}
		h(w, r)
	})
	s := &http.Server{Handler: mux}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Close(); _ = ln.Close() })
	return d
}
func (d *stubDaemon) on(h func(http.ResponseWriter, *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handler = h
}
func (d *stubDaemon) onSessions(h func(http.ResponseWriter, *http.Request)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionsHandler = h
}
func (d *stubDaemon) lastRequest(t *testing.T) recordedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		t.Fatal("no request")
	}
	return d.requests[len(d.requests)-1]
}
func localSession() []cliSession {
	return []cliSession{{ID: "1va8lvdv", Adapter: "pi", Alive: true, Slug: "work"}}
}
func writeEnvelope(w http.ResponseWriter, status int, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}
func writeErrEnvelope(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}})
}
func captureStdout(t *testing.T, fn func()) string { return captureStream(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { return captureStream(t, &os.Stderr, fn) }
func captureStream(t *testing.T, target **os.File, fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := *target
	*target = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
