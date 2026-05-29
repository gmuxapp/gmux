package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// writeFakeShell writes an executable script to a temp dir and returns
// its path. The script stands in for $SHELL: gmuxd invokes it as
// `shell -l -i -c '<gmuxBin> --dump-env'`, but the fake ignores its
// args and runs body, which can write the env payload to fd 3 (>&3),
// sleep, or exit non-zero to exercise each path.
func writeFakeShell(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeshell")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	return path
}

func TestCaptureLoginEnv_FakeShellPayload(t *testing.T) {
	shell := writeFakeShell(t, `printf 'A=1\0B=two\nlines\0C=\0' >&3`)
	t.Setenv("SHELL", shell)

	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 5*time.Second)
	want := []string{"A=1", "B=two\nlines", "C="}
	if !slices.Equal(got, want) {
		t.Errorf("captureLoginEnv = %v, want %v", got, want)
	}
}

// Banner noise on stdout/stderr must not corrupt the payload — only
// fd 3 is read.
func TestCaptureLoginEnv_IgnoresStdoutNoise(t *testing.T) {
	shell := writeFakeShell(t, "echo banner-on-stdout\necho moo >&2\nprintf 'X=9\\0' >&3")
	t.Setenv("SHELL", shell)

	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 5*time.Second)
	if !slices.Equal(got, []string{"X=9"}) {
		t.Errorf("captureLoginEnv = %v, want [X=9]", got)
	}
}

func TestCaptureLoginEnv_ShellUnset(t *testing.T) {
	t.Setenv("SHELL", "")
	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 5*time.Second)
	if !slices.Equal(got, os.Environ()) {
		t.Errorf("SHELL-unset should fall back to os.Environ()")
	}
}

func TestCaptureLoginEnv_NoBinary(t *testing.T) {
	t.Setenv("SHELL", writeFakeShell(t, "printf 'A=1\\0' >&3"))
	got := captureLoginEnvTimeout("", t.TempDir(), 5*time.Second)
	if !slices.Equal(got, os.Environ()) {
		t.Errorf("empty gmuxBin should fall back to os.Environ()")
	}
}

func TestCaptureLoginEnv_NonZeroExit(t *testing.T) {
	// Exits non-zero without writing fd 3.
	shell := writeFakeShell(t, "exit 7")
	t.Setenv("SHELL", shell)
	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 5*time.Second)
	if !slices.Equal(got, os.Environ()) {
		t.Errorf("non-zero exit should fall back to os.Environ()")
	}
}

func TestCaptureLoginEnv_EmptyDump(t *testing.T) {
	// Exits 0 but writes nothing — treated as a failed capture.
	shell := writeFakeShell(t, "true")
	t.Setenv("SHELL", shell)
	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 5*time.Second)
	if !slices.Equal(got, os.Environ()) {
		t.Errorf("empty dump should fall back to os.Environ()")
	}
}

func TestCaptureLoginEnv_Timeout(t *testing.T) {
	// Holds fd 3 open and sleeps past the timeout. The capture must
	// return promptly via fallback rather than blocking for the sleep.
	shell := writeFakeShell(t, "sleep 30")
	t.Setenv("SHELL", shell)

	start := time.Now()
	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 200*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("capture took %s, expected to abort near the 200ms timeout", elapsed)
	}
	if !slices.Equal(got, os.Environ()) {
		t.Errorf("timeout should fall back to os.Environ()")
	}
}

// A process the rc files spawn in the background can inherit fd 3 and
// keep the pipe open after the shell exits, so io.ReadAll never sees
// EOF. The capture must still return near the timeout (via fallback)
// rather than blocking on the lingering background process.
func TestCaptureLoginEnv_BackgroundHoldsFD3(t *testing.T) {
	shell := writeFakeShell(t, "printf 'A=1\\0' >&3\nsleep 30 &\nexit 0")
	t.Setenv("SHELL", shell)

	start := time.Now()
	got := captureLoginEnvTimeout("/bin/true", t.TempDir(), 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("capture took %s; expected to abort near the 300ms timeout", elapsed)
	}
	if !slices.Equal(got, os.Environ()) {
		t.Errorf("background-held fd 3 should fall back to os.Environ()")
	}
}

func TestParseNulEnv(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"trailing nul", "A=1\x00B=2\x00", []string{"A=1", "B=2"}},
		{"no trailing nul", "A=1\x00B=2", []string{"A=1", "B=2"}},
		{"skips empty segments", "A=1\x00\x00B=2\x00", []string{"A=1", "B=2"}},
		{"newline value", "A=x\ny\x00", []string{"A=x\ny"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNulEnv([]byte(c.in))
			if !slices.Equal(got, c.want) {
				t.Errorf("parseNulEnv(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/gmux":          "'/usr/bin/gmux'",
		"/path with spaces/gmux": "'/path with spaces/gmux'",
		"/weird/o'brien/gmux":    `'/weird/o'\''brien/gmux'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
