package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// writeJSONL writes pi session lines to path.
func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
	f.Close()
}

func TestAttributeFromShimIsAuthoritative(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "sess.jsonl")
	writeJSONL(t, path,
		`{"type":"session","id":"tool-1","timestamp":"2026-06-16T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hello there"}]}}`,
	)

	fm.AttributeFromShim("sess-pi", path)

	if fm.attributions[path] != "sess-pi" {
		t.Fatalf("attribution = %v, want sess-pi", fm.attributions[path])
	}
	if fm.shimFiles[path] != "sess-pi" {
		t.Fatalf("shimFiles = %v, want sess-pi", fm.shimFiles[path])
	}
	if fm.candidateFiles[path] {
		t.Errorf("file should be removed from candidateFiles")
	}
	if !fm.sessionHasShimLocked("sess-pi") {
		t.Errorf("session should be marked shim-attributed")
	}
	// The daemon must NOT derive title/status for a shim-covered session:
	// that's the runner's job now (ADR 0011 phase 1, sessionFileReader). The
	// daemon only records the attribution and relies on /events for state.
	if sess, _ := s.Get("sess-pi"); sess.AdapterTitle != "" {
		t.Errorf("daemon derived title %q for shim session; runner owns it", sess.AdapterTitle)
	}
}

func TestAttributeFromShimRebindDropsOldFile(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, _, dir := setupPiFileMonitor(t)
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	writeJSONL(t, oldPath, `{"type":"session","id":"tool-old","timestamp":"2026-06-16T10:00:00Z"}`)
	writeJSONL(t, newPath, `{"type":"session","id":"tool-new","timestamp":"2026-06-16T11:00:00Z"}`)

	fm.AttributeFromShim("sess-pi", oldPath)
	fm.AttributeFromShim("sess-pi", newPath) // /resume rebind

	if _, ok := fm.shimFiles[oldPath]; ok {
		t.Errorf("old file still in shimFiles after rebind")
	}
	if _, ok := fm.attributions[oldPath]; ok {
		t.Errorf("old file still attributed after rebind")
	}
	if fm.shimFiles[newPath] != "sess-pi" || fm.attributions[newPath] != "sess-pi" {
		t.Errorf("new file not attributed after rebind")
	}
}

func TestSessionDeathClearsShimAttribution(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, _, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "sess.jsonl")
	writeJSONL(t, path, `{"type":"session","id":"tool-1","timestamp":"2026-06-16T10:00:00Z"}`)

	fm.AttributeFromShim("sess-pi", path)
	fm.NotifySessionDied("sess-pi")

	if _, ok := fm.shimFiles[path]; ok {
		t.Errorf("shimFiles not cleared on session death")
	}
	if _, ok := fm.attributions[path]; ok {
		t.Errorf("attributions not cleared on session death")
	}
	if fm.sessionHasShimLocked("sess-pi") {
		t.Errorf("session still marked shim-attributed after death")
	}
}

func TestShimAttributedSessionExcludedFromScrollback(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, _, dir := setupPiFileMonitor(t)
	shimPath := filepath.Join(dir, "held.jsonl")
	writeJSONL(t, shimPath, `{"type":"session","id":"tool-1","timestamp":"2026-06-16T10:00:00Z"}`)
	fm.AttributeFromShim("sess-pi", shimPath)

	// A second, unattributed file appears in the same dir. With the only
	// live session shim-attributed, scrollback matching has no candidate
	// to offer, so the file stays unattributed (no mis-attribution).
	other := filepath.Join(dir, "other.jsonl")
	writeJSONL(t, other,
		`{"type":"session","id":"tool-2","timestamp":"2026-06-16T10:05:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"some long message content here for matching"}]}}`,
	)
	fm.candidateFiles[other] = true

	fm.tryAttributeUnmatched()

	if sid, ok := fm.attributions[other]; ok {
		t.Errorf("unattributed file was attributed to %q despite only session being shim-held", sid)
	}
}
