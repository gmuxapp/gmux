package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewLogdWriterTeesLocalWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var local bytes.Buffer
	w := newLogdWriter(&local, srv.URL+"/log")

	msg := []byte("2026/06/03 10:00:00 hello logd\n")
	n, err := w.Write(msg)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("Write returned n=%d, want %d", n, len(msg))
	}
	if got := local.String(); got != string(msg) {
		t.Errorf("local writer got %q, want %q", got, string(msg))
	}
}

func TestNewLogdWriterPostsToEndpoint(t *testing.T) {
	ready := make(chan struct{ body, source string }, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/log" {
			b, _ := io.ReadAll(r.Body)
			ready <- struct{ body, source string }{string(b), r.Header.Get("X-Log-Source")}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var local bytes.Buffer
	w := newLogdWriter(&local, srv.URL+"/log")

	msg := []byte("2026/06/03 10:00:00 hello logd\n")
	w.Write(msg) //nolint:errcheck

	select {
	case got := <-ready:
		if got.body != string(msg) {
			t.Errorf("POST body = %q, want %q", got.body, string(msg))
		}
		if got.source != "backend" {
			t.Errorf("X-Log-Source = %q, want %q", got.source, "backend")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logd POST never arrived")
	}
}

func TestNewLogdWriterPostFailureWritesToLocal(t *testing.T) {
	// Port 1 guarantees connection refused immediately.
	var local bytes.Buffer
	w := newLogdWriter(&local, "http://127.0.0.1:1/log")

	msg := []byte("2026/06/03 10:00:00 test message\n")
	n, err := w.Write(msg)
	if err != nil {
		t.Fatalf("Write must not return error on POST failure, got: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("Write returned n=%d, want %d", n, len(msg))
	}

	// Original message written synchronously before the goroutine fires.
	if !strings.Contains(local.String(), string(msg)) {
		t.Errorf("local missing original message, got %q", local.String())
	}

	// Wait for the goroutine to fail and write the error.
	time.Sleep(300 * time.Millisecond)
	if !strings.Contains(local.String(), "logd:") {
		t.Errorf("local missing logd error after failure, got %q", local.String())
	}
}
