package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/clipfile"
)

// roundTrip exercises the clipboard handler with a real LocalWriter
// rooted at t.TempDir() and returns the HTTP response plus the parsed
// JSON body.
func roundTrip(t *testing.T, dir string, method string, contentType string, body []byte) (*http.Response, map[string]any) {
	t.Helper()
	w := clipfile.NewLocalWriter(dir)
	h := clipboardHandler(w)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(method, srv.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var parsed map[string]any
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			t.Fatalf("unmarshal %q: %v", respBody, err)
		}
	}
	return resp, parsed
}

func TestClipboardHandler_HappyPath(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'x', 'y', 'z'}
	resp, parsed := roundTrip(t, dir, http.MethodPost, "image/png", payload)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data, ok := parsed["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data in response: %v", parsed)
	}
	pathStr, _ := data["path"].(string)
	if filepath.Base(pathStr) != "paste-1.png" {
		t.Errorf("path basename = %q, want paste-1.png", filepath.Base(pathStr))
	}
	got, err := os.ReadFile(pathStr)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("file contents differ from posted bytes")
	}
}

func TestClipboardHandler_RejectsNonPOST(t *testing.T) {
	dir := t.TempDir()
	resp, _ := roundTrip(t, dir, http.MethodGet, "image/png", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestClipboardHandler_EnforcesSizeCap(t *testing.T) {
	dir := t.TempDir()
	// Just over the 10MB limit.
	oversized := bytes.Repeat([]byte{0xff}, MaxClipboardBytes+1)
	resp, parsed := roundTrip(t, dir, http.MethodPost, "image/png", oversized)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if parsed["ok"] != false {
		t.Errorf("ok field = %v, want false", parsed["ok"])
	}
	// No file should have been written.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "paste-") {
			t.Errorf("oversized request left file %q on disk", e.Name())
		}
	}
}

func TestClipboardHandler_DefaultsContentTypeToBin(t *testing.T) {
	dir := t.TempDir()
	resp, parsed := roundTrip(t, dir, http.MethodPost, "", []byte("opaque"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data := parsed["data"].(map[string]any)
	pathStr := data["path"].(string)
	if !strings.HasSuffix(pathStr, ".bin") {
		t.Errorf("path = %q, want .bin extension for unspecified Content-Type", pathStr)
	}
}

func TestClipboardHandler_RejectsEmptyBody(t *testing.T) {
	dir := t.TempDir()
	resp, parsed := roundTrip(t, dir, http.MethodPost, "image/png", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if parsed["ok"] != false {
		t.Errorf("ok = %v, want false", parsed["ok"])
	}
	// No file written.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "paste-") {
			t.Errorf("empty request left file %q on disk", e.Name())
		}
	}
}
