package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	tests := []struct {
		input string
		want  string
	}{
		{"~", home},
		{"~/dev/gmux", home + "/dev/gmux"},
		{"/opt/data", "/opt/data"},
		{"", ""},
		// Already absolute: unchanged.
		{home + "/dev/gmux", home + "/dev/gmux"},
	}
	for _, tt := range tests {
		got := NormalizePath(tt.input)
		if got != tt.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCanonicalizePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	tests := []struct {
		input string
		want  string
	}{
		{home, "~"},
		{home + "/dev/gmux", "~/dev/gmux"},
		{home + "/", "~"},
		{"/opt/data", "/opt/data"},
		{"/tmp/../tmp", "/tmp"},
		{"", ""},
		// Already canonical: passes through unchanged.
		{"~/dev/gmux", "~/dev/gmux"},
		{"~", "~"},
	}
	for _, tt := range tests {
		got := CanonicalizePath(tt.input)
		if got != tt.want {
			t.Errorf("CanonicalizePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSessionSocketDir(t *testing.T) {
	t.Run("GMUX_SOCKET_DIR overrides everything", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		t.Setenv("GMUX_SOCKET_DIR", "/tmp/custom-sockets")
		if got := SessionSocketDir(); got != "/tmp/custom-sockets" {
			t.Errorf("SessionSocketDir() = %q, want %q", got, "/tmp/custom-sockets")
		}
	})

	t.Run("falls back to XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("GMUX_SOCKET_DIR", "")
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		want := "/run/user/1000/gmux/sessions"
		if got := SessionSocketDir(); got != want {
			t.Errorf("SessionSocketDir() = %q, want %q", got, want)
		}
	})

	t.Run("per-uid temp dir when no XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("GMUX_SOCKET_DIR", "")
		t.Setenv("XDG_RUNTIME_DIR", "")
		got := SessionSocketDir()
		want := filepath.Join(os.TempDir(), fmt.Sprintf("gmux-sessions-%d", os.Getuid()))
		if got != want {
			t.Errorf("SessionSocketDir() = %q, want %q", got, want)
		}
		// Must not be the old world-shared path.
		if got == "/tmp/gmux-sessions" {
			t.Errorf("SessionSocketDir() must not default to the shared /tmp/gmux-sessions")
		}
	})
}
