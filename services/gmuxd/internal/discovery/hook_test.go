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

func TestAttributeFromHookIsAuthoritative(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, s, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "sess.jsonl")
	writeJSONL(t, path,
		`{"type":"session","id":"tool-1","timestamp":"2026-06-16T10:00:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"hello there"}]}}`,
	)

	fm.AttributeFromHook("sess-pi", path)

	if fm.attributions[path] != "sess-pi" {
		t.Fatalf("attribution = %v, want sess-pi", fm.attributions[path])
	}
	if fm.hookFiles[path] != "sess-pi" {
		t.Fatalf("hookFiles = %v, want sess-pi", fm.hookFiles[path])
	}
	if fm.candidateFiles[path] {
		t.Errorf("file should be removed from candidateFiles")
	}
	if !fm.sessionHasHookLocked("sess-pi") {
		t.Errorf("session should be marked hook-attributed")
	}
	// The daemon must NOT derive title/status for a hook-covered session:
	// that's the runner's job now (ADR 0011 phase 1, sessionFileReader). The
	// daemon only records the attribution and relies on /events for state.
	if sess, _ := s.Get("sess-pi"); sess.AdapterTitle != "" {
		t.Errorf("daemon derived title %q for hook session; runner owns it", sess.AdapterTitle)
	}
}

func TestAttributeFromHookRebindDropsOldFile(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, _, dir := setupPiFileMonitor(t)
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	writeJSONL(t, oldPath, `{"type":"session","id":"tool-old","timestamp":"2026-06-16T10:00:00Z"}`)
	writeJSONL(t, newPath, `{"type":"session","id":"tool-new","timestamp":"2026-06-16T11:00:00Z"}`)

	fm.AttributeFromHook("sess-pi", oldPath)
	fm.AttributeFromHook("sess-pi", newPath) // /resume rebind

	if _, ok := fm.hookFiles[oldPath]; ok {
		t.Errorf("old file still in hookFiles after rebind")
	}
	if _, ok := fm.attributions[oldPath]; ok {
		t.Errorf("old file still attributed after rebind")
	}
	if fm.hookFiles[newPath] != "sess-pi" || fm.attributions[newPath] != "sess-pi" {
		t.Errorf("new file not attributed after rebind")
	}
}

func TestSessionDeathClearsHookAttribution(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, _, dir := setupPiFileMonitor(t)
	path := filepath.Join(dir, "sess.jsonl")
	writeJSONL(t, path, `{"type":"session","id":"tool-1","timestamp":"2026-06-16T10:00:00Z"}`)

	fm.AttributeFromHook("sess-pi", path)
	fm.NotifySessionDied("sess-pi")

	if _, ok := fm.hookFiles[path]; ok {
		t.Errorf("hookFiles not cleared on session death")
	}
	if _, ok := fm.attributions[path]; ok {
		t.Errorf("attributions not cleared on session death")
	}
	if fm.sessionHasHookLocked("sess-pi") {
		t.Errorf("session still marked hook-attributed after death")
	}
}

func TestHookAttributedSessionExcludedFromFallback(t *testing.T) {
	t.Setenv("GMUX_SOCKET_DIR", t.TempDir())
	fm, _, dir := setupPiFileMonitor(t)
	hookPath := filepath.Join(dir, "held.jsonl")
	writeJSONL(t, hookPath, `{"type":"session","id":"tool-1","timestamp":"2026-06-16T10:00:00Z"}`)
	fm.AttributeFromHook("sess-pi", hookPath)

	// A second, unattributed file appears in the same dir. With the only
	// live session hook-attributed, scrollback matching has no candidate
	// to offer, so the file stays unattributed (no mis-attribution).
	other := filepath.Join(dir, "other.jsonl")
	writeJSONL(t, other,
		`{"type":"session","id":"tool-2","timestamp":"2026-06-16T10:05:00Z"}`,
		`{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"some long message content here for matching"}]}}`,
	)
	fm.candidateFiles[other] = true

	fm.tryAttributeUnmatched()

	if sid, ok := fm.attributions[other]; ok {
		t.Errorf("unattributed file was attributed to %q despite only session being hook-held", sid)
	}
}
