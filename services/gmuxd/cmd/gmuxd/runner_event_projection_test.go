package main

import (
	"testing"
)

// TestRunnerEventProjectionMetaParsesRunnerWireShape pins the exact snake_case
// payload the runner emits (cli/gmux/internal/session/state.go emitMetaLocked
// and SetSlug). Regression: the projection struct had untagged fields, and
// Go's case-insensitive matching does not bridge "adapter_title" to
// AdapterTitle — every live title/slug update was silently dropped, so
// sessions started after a daemon restart never showed their titles.
func TestRunnerEventProjectionMetaParsesRunnerWireShape(t *testing.T) {
	raw := []byte(`{"title":"docs-audit","shell_title":"π - docs-audit - rest-reads","adapter_title":"docs-audit","subtitle":"sub","slug":"docs-audit"}`)
	ev, ok := runnerEventProjection("meta", raw)
	if !ok {
		t.Fatal("meta event rejected")
	}
	f := ev.Facts
	for name, got := range map[string]*string{
		"shell_title":   f.ShellTitle,
		"adapter_title": f.AdapterTitle,
		"subtitle":      f.Subtitle,
		"slug":          f.Slug,
	} {
		if got == nil {
			t.Errorf("%s not parsed from runner meta event", name)
		}
	}
	if f.AdapterTitle != nil && *f.AdapterTitle != "docs-audit" {
		t.Errorf("adapter_title = %q, want %q", *f.AdapterTitle, "docs-audit")
	}
	if f.ShellTitle != nil && *f.ShellTitle != "π - docs-audit - rest-reads" {
		t.Errorf("shell_title = %q", *f.ShellTitle)
	}

	// unread-only meta events (state.go SetUnread) keep working.
	ev, ok = runnerEventProjection("meta", []byte(`{"unread":true}`))
	if !ok || ev.Facts.Unread == nil || !*ev.Facts.Unread {
		t.Fatal("unread meta event not parsed")
	}
}
