package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
)

// Covers the HTTP surface of async indexing: 503+Retry-After for misses while
// the snapshot runs, 200 for entries indexed so far, 404 only once complete,
// and the health conversation_index payload in both states.
func TestConversationLookupAndHealthDuringIndexing(t *testing.T) {
	// Isolate every adapter root so Snapshot() below scans an empty corpus
	// and just flips completeness.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	idx := conversations.New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/conversations/{adapter}/{slug}", handleConversationLookup(idx))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(t *testing.T, slug string) (int, http.Header, map[string]any) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/v1/conversations/pi/" + slug)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, body
	}
	errCode := func(body map[string]any) string {
		e, _ := body["error"].(map[string]any)
		code, _ := e["code"].(string)
		return code
	}

	// --- indexing in progress (snapshot not complete) ---
	if h := conversationIndexHealth(idx); h["status"] != "indexing" || h["indexed"] != 0 {
		t.Fatalf("health during indexing = %v", h)
	}
	status, hdr, body := get(t, "no-such-slug")
	if status != http.StatusServiceUnavailable || errCode(body) != "indexing" {
		t.Fatalf("miss during indexing: status=%d body=%v (want 503/indexing)", status, body)
	}
	if hdr.Get("Retry-After") == "" {
		t.Fatal("503 during indexing must carry Retry-After")
	}

	// Entries already scanned serve immediately, mid-indexing.
	idx.Upsert(conversations.Info{ConversationID: "conv-1", Slug: "hello-world", Adapter: "pi", Title: "Hello World", Cwd: "/tmp/x", Ref: "/tmp/x/conv-1.jsonl", ResumeCommand: []string{"pi", "--resume", "conv-1"}, Created: time.Unix(1700000000, 0)})
	status, _, body = get(t, "hello-world")
	if status != http.StatusOK {
		t.Fatalf("indexed entry during indexing: status=%d body=%v (want 200)", status, body)
	}
	if data, _ := body["data"].(map[string]any); data["title"] != "Hello World" || data["adapter"] != "pi" {
		t.Fatalf("payload = %v", body["data"])
	}
	if h := conversationIndexHealth(idx); h["status"] != "indexing" || h["indexed"] != 1 {
		t.Fatalf("health mid-indexing = %v", h)
	}

	// --- snapshot complete (empty adapter roots; flips completeness) ---
	idx.Snapshot()
	if !idx.SnapshotComplete() {
		t.Fatal("Snapshot did not mark completeness")
	}
	if h := conversationIndexHealth(idx); h["status"] != "ready" || h["indexed"] != 1 {
		t.Fatalf("health after indexing = %v", h)
	}
	status, _, body = get(t, "no-such-slug")
	if status != http.StatusNotFound || errCode(body) != "not_found" {
		t.Fatalf("miss after indexing: status=%d body=%v (want 404/not_found)", status, body)
	}
	if status, _, _ = get(t, "hello-world"); status != http.StatusOK {
		t.Fatalf("indexed entry after indexing: status=%d (want 200)", status)
	}
}
