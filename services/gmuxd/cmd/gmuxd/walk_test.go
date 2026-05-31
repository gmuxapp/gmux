package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ── walkProjectPaths ──────────────────────────────────────────────────────────

func TestWalkProjectPaths_empty(t *testing.T) {
	root := t.TempDir()
	paths, err := walkProjectPaths(root, 50_000, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected empty, got %v", paths)
	}
}

func TestWalkProjectPaths_filesAndDirs(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "src", "main.go"), []byte(""), 0o644)

	paths, err := walkProjectPaths(root, 50_000, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantSet := map[string]bool{
		"README.md": true,
		"src/":      true,
		"src/main.go": true,
	}
	if len(paths) != len(wantSet) {
		t.Fatalf("got %v, want keys %v", paths, wantSet)
	}
	for _, p := range paths {
		if !wantSet[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

func TestWalkProjectPaths_dirsHaveTrailingSlash(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)

	paths, err := walkProjectPaths(root, 50_000, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	seen := make(map[string]bool)
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["a/"] {
		t.Errorf("expected a/ in %v", paths)
	}
	if !seen["a/b/"] {
		t.Errorf("expected a/b/ in %v", paths)
	}
}

func TestWalkProjectPaths_maxEntries(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		name := filepath.Join(root, "file"+string(rune('a'+i))+".txt")
		_ = os.WriteFile(name, nil, 0o644)
	}
	paths, err := walkProjectPaths(root, 3, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 3 {
		t.Errorf("expected 3 paths (limit hit), got %d", len(paths))
	}
}

// ── walk endpoint (integration via httptest) ──────────────────────────────────

func walkTestMux(t *testing.T, root string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/fs/{slug}/walk", func(w http.ResponseWriter, r *http.Request) {
		paths, err := walkProjectPaths(root, 50_000, true)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": paths})
	})
	return mux
}

func TestWalkEndpoint_returnsJSON(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "foo.ts"), nil, 0o644)
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)

	srv := httptest.NewServer(walkTestMux(t, root))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/fs/proj/walk")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out struct {
		Ok   bool     `json:"ok"`
		Data []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Ok {
		t.Fatal("ok=false")
	}
	sort.Strings(out.Data)

	want := []string{".git/", "foo.ts"}
	if len(out.Data) != len(want) {
		t.Fatalf("got %v, want %v", out.Data, want)
	}
	for i := range want {
		if out.Data[i] != want[i] {
			t.Errorf("out.Data[%d] = %q, want %q", i, out.Data[i], want[i])
		}
	}
}

// ── parseGitPorcelain ─────────────────────────────────────────────────────────

func TestParseGitPorcelain_empty(t *testing.T) {
	entries := parseGitPorcelain("")
	if len(entries) != 0 {
		t.Fatalf("expected empty, got %v", entries)
	}
}

func TestParseGitPorcelain_basicStatuses(t *testing.T) {
	input := " M src/main.go\n" +
		"A  new-file.ts\n" +
		" D deleted.go\n" +
		"?? untracked.txt\n" +
		"!! ignored.log\n" +
		"R  old.go -> renamed.go\n"

	entries := parseGitPorcelain(input)

	want := map[string]string{
		"src/main.go": "modified",
		"new-file.ts": "added",
		"deleted.go":  "deleted",
		"untracked.txt": "untracked",
		"ignored.log":   "ignored",
		"renamed.go":    "renamed",
	}

	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(want), entries)
	}

	for _, e := range entries {
		wantStatus, ok := want[e.Path]
		if !ok {
			t.Errorf("unexpected path %q", e.Path)
			continue
		}
		if e.Status != wantStatus {
			t.Errorf("path %q: got status %q, want %q", e.Path, e.Status, wantStatus)
		}
	}
}

func TestParseGitPorcelain_quotedNonASCII(t *testing.T) {
	// git porcelain quotes non-ASCII filenames with surrounding double-quotes
	input := `?? "caf\303\251.txt"` + "\n"
	entries := parseGitPorcelain(input)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	// The raw escaped bytes stay as-is; we just strip the surrounding quotes.
	if entries[0].Path == "" {
		t.Error("path should not be empty")
	}
	if entries[0].Status != "untracked" {
		t.Errorf("expected untracked, got %q", entries[0].Status)
	}
}

func TestParseGitPorcelain_shortLine(t *testing.T) {
	// Lines shorter than 4 chars must be skipped without panic.
	entries := parseGitPorcelain("M\n \n??\n")
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %v", entries)
	}
}

// ── walkProjectPaths includeHidden ──────────────────────────────────────────

func TestWalkProjectPaths_hiddenExcludedByDefault(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".git", "objects", "abc"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "src", "main.go"), nil, 0o644)

	paths, err := walkProjectPaths(root, 50_000, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := make(map[string]bool)
	for _, p := range paths {
		seen[p] = true
	}

	if seen[".git/"] {
		t.Error("expected .git/ to be excluded")
	}
	if seen[".git/objects/"] {
		t.Error("expected .git/objects/ to be excluded")
	}
	if seen[".gitignore"] {
		t.Error("expected .gitignore to be excluded")
	}
	if !seen["README.md"] {
		t.Error("expected README.md to be included")
	}
	if !seen["src/"] {
		t.Error("expected src/ to be included")
	}
	if !seen["src/main.go"] {
		t.Error("expected src/main.go to be included")
	}
}

func TestWalkProjectPaths_hiddenIncludedWhenRequested(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(root, "README.md"), nil, 0o644)

	paths, err := walkProjectPaths(root, 50_000, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := make(map[string]bool)
	for _, p := range paths {
		seen[p] = true
	}
	if !seen[".gitignore"] {
		t.Error("expected .gitignore when includeHidden=true")
	}
	if !seen["README.md"] {
		t.Error("expected README.md when includeHidden=true")
	}
}

func TestWalkProjectPaths_capNotExhaustedByHiddenDirs(t *testing.T) {
	// Hidden dirs must not consume the cap when includeHidden=false.
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".hidden"), 0o755)
	for i := 0; i < 10; i++ {
		name := filepath.Join(root, ".hidden", "file"+string(rune('a'+i))+".txt")
		_ = os.WriteFile(name, nil, 0o644)
	}
	_ = os.WriteFile(filepath.Join(root, "visible.txt"), nil, 0o644)

	paths, err := walkProjectPaths(root, 5, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := make(map[string]bool)
	for _, p := range paths {
		seen[p] = true
	}
	if !seen["visible.txt"] {
		t.Errorf("visible.txt missing from %v", paths)
	}
}
