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

	t.Run("defaults to state dir, ignoring XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("GMUX_SOCKET_DIR", "")
		// logind/elogind tear down $XDG_RUNTIME_DIR on last logout, so it
		// must never be the socket home for login-outliving runners.
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		t.Setenv("XDG_STATE_HOME", "/home/u/.local/state")
		want := "/home/u/.local/state/gmux/run/sessions"
		if got := SessionSocketDir(); got != want {
			t.Errorf("SessionSocketDir() = %q, want %q", got, want)
		}
		// Must not be the old world-shared path either.
		if got := SessionSocketDir(); got == "/tmp/gmux-sessions" {
			t.Errorf("SessionSocketDir() must not default to the shared /tmp/gmux-sessions")
		}
	})
}

func TestLegacySessionSocketDirs(t *testing.T) {
	t.Run("empty when GMUX_SOCKET_DIR is set", func(t *testing.T) {
		t.Setenv("GMUX_SOCKET_DIR", "/tmp/custom-sockets")
		if got := LegacySessionSocketDirs(); len(got) != 0 {
			t.Errorf("LegacySessionSocketDirs() = %v, want empty", got)
		}
	})

	t.Run("includes XDG runtime dir and per-uid temp dir", func(t *testing.T) {
		t.Setenv("GMUX_SOCKET_DIR", "")
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		got := LegacySessionSocketDirs()
		want := []string{
			"/run/user/1000/gmux/sessions",
			filepath.Join(os.TempDir(), fmt.Sprintf("gmux-sessions-%d", os.Getuid())),
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("LegacySessionSocketDirs() = %v, want %v", got, want)
		}
	})

	t.Run("per-uid temp dir only when no XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("GMUX_SOCKET_DIR", "")
		t.Setenv("XDG_RUNTIME_DIR", "")
		got := LegacySessionSocketDirs()
		if len(got) != 1 {
			t.Fatalf("LegacySessionSocketDirs() = %v, want 1 entry", got)
		}
	})
}

func TestIsValidSessionID(t *testing.T) {
	valid := []string{
		"sess-abcd1234",
		"sess-0",
		"sess-claude",
		"sess-resume_1",
		"sess-codex-2",
	}
	for _, id := range valid {
		if !IsValidSessionID(id) {
			t.Errorf("IsValidSessionID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"abcd1234",          // missing prefix
		"sess-",             // empty suffix
		"sess-../escape",    // path traversal
		"sess-..",           // parent dir
		"../sess-abcd",      // leading traversal
		"sess-a/b",          // separator
		`sess-a\b`,          // backslash separator
		"sess-a::b",         // folder-key separator
		"sess-a b",          // space
		"sess-a\n",          // newline
	}
	for _, id := range invalid {
		if IsValidSessionID(id) {
			t.Errorf("IsValidSessionID(%q) = true, want false", id)
		}
	}
}
