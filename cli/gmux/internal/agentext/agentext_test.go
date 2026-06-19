package agentext

import (
	"os"
	"strings"
	"testing"
)

func TestPathMaterializesReadableExtension(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if !strings.HasSuffix(p, ".mjs") {
		t.Errorf("expected .mjs path, got %q", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read materialized ext: %v", err)
	}
	if !strings.Contains(string(data), "session_switch") {
		t.Error("materialized extension missing session_switch handler")
	}

	// Idempotent: a second call returns the same path.
	p2, err := Path()
	if err != nil || p2 != p {
		t.Errorf("Path not idempotent: %q/%v vs %q", p2, err, p)
	}
}
