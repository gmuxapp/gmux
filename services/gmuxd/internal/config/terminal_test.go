package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTerminalTheme_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	data, err := LoadTerminalTheme()
	if err != nil {
		t.Fatal(err)
	}
	if data != nil {
		t.Errorf("expected nil for missing file, got %s", data)
	}
}

func TestLoadTerminalTheme_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{"fontSize": 16}`)

	data, err := LoadTerminalTheme()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"fontSize": 16}` {
		t.Errorf("got %s", data)
	}
}

func TestLoadTerminalTheme_StripsComments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{
  // This is the font size
  "fontSize": 16,
  /* block comment */
  "cursorBlink": true
}`)

	data, err := LoadTerminalTheme()
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		t.Fatal("expected non-nil data")
	}
}

func TestLoadTerminalTheme_StripsTrailingCommas(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{
  "fontSize": 16,
  "cursorBlink": true,
}`)

	data, err := LoadTerminalTheme()
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		t.Fatal("expected non-nil data")
	}
}

func TestLoadTerminalTheme_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{invalid json}`)

	_, err := LoadTerminalTheme()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadKeybinds_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	data, err := LoadKeybinds()
	if err != nil {
		t.Fatal(err)
	}
	if data != nil {
		t.Errorf("expected nil for missing file, got %s", data)
	}
}

func TestLoadKeybinds_ValidArray(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "keybinds.jsonc", `[
  // Remap ctrl+t
  { "key": "ctrl+alt+t", "action": "sendKeys", "args": "ctrl+t" },
]`)

	data, err := LoadKeybinds()
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		t.Fatal("expected non-nil data")
	}
}

func writeFile(t *testing.T, xdgDir, name, content string) {
	t.Helper()
	dir := filepath.Join(xdgDir, "gmux")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
