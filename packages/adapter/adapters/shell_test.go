package adapters

import (
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func TestShellMatchAll(t *testing.T) {
	g := NewShell()
	if !g.Match([]string{"anything"}) {
		t.Fatal("shell should match any command")
	}
	if !g.Match([]string{}) {
		t.Fatal("shell should match empty command")
	}
}

func TestShellName(t *testing.T) {
	if NewShell().Name() != "shell" {
		t.Fatal("expected 'shell'")
	}
}

func TestShellEnvNil(t *testing.T) {
	if env := NewShell().Env(adapter.EnvContext{}); env != nil {
		t.Fatalf("expected nil, got %v", env)
	}
}

func TestShellMonitorPlainOutput(t *testing.T) {
	if NewShell().Monitor([]byte("hello")) != nil {
		t.Fatal("should not report status for plain output")
	}
}

func TestShellDoesNotImplementCapabilities(t *testing.T) {
	var a adapter.Adapter = NewShell()
	if _, ok := a.(adapter.SessionFiler); ok {
		t.Fatal("Shell should not implement SessionFiler")
	}
	if _, ok := a.(adapter.Resumer); ok {
		t.Fatal("Shell should not implement Resumer")
	}
}

// --- OSC title parsing ---

func TestParseOSCTitleBEL(t *testing.T) {
	if title := parseOSCTitle([]byte("\x1b]0;my title\x07 more")); title != "my title" {
		t.Fatalf("expected 'my title', got %q", title)
	}
}

func TestParseOSCTitleST(t *testing.T) {
	if title := parseOSCTitle([]byte("\x1b]2;window title\x1b\\ more")); title != "window title" {
		t.Fatalf("expected 'window title', got %q", title)
	}
}

func TestParseOSCTitleNone(t *testing.T) {
	if title := parseOSCTitle([]byte("hello world")); title != "" {
		t.Fatalf("expected empty, got %q", title)
	}
}

func TestParseOSCTitleEmbedded(t *testing.T) {
	data := []byte("output\r\n\x1b]0;~/dev/gmux\x07prompt $ ")
	if title := parseOSCTitle(data); title != "~/dev/gmux" {
		t.Fatalf("expected '~/dev/gmux', got %q", title)
	}
}

func TestShellMonitorTitleUpdate(t *testing.T) {
	s := NewShell().Monitor([]byte("\x1b]0;fish: ~/dev\x07"))
	if s == nil {
		t.Fatal("should return status")
	}
	if s.Title != "fish: ~/dev" {
		t.Fatalf("expected 'fish: ~/dev', got %q", s.Title)
	}
}
