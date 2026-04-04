package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTheme_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	data, err := LoadTheme()
	if err != nil {
		t.Fatal(err)
	}
	if data != nil {
		t.Errorf("expected nil for missing file, got %s", data)
	}
}

func TestLoadTheme_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{"background": "#282a36"}`)

	data, err := LoadTheme()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"background": "#282a36"}` {
		t.Errorf("got %s", data)
	}
}

func TestLoadTheme_StripsComments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{
  // Dark background
  "background": "#282a36",
  /* Dracula foreground */
  "foreground": "#f8f8f2"
}`)

	data, err := LoadTheme()
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		t.Fatal("expected non-nil data")
	}
}

func TestLoadTheme_StripsTrailingCommas(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{
  "background": "#282a36",
  "foreground": "#f8f8f2",
}`)

	data, err := LoadTheme()
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		t.Fatal("expected non-nil data")
	}
}

func TestLoadTheme_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "theme.jsonc", `{invalid json}`)

	_, err := LoadTheme()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadSettings_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	data, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if data != nil {
		t.Errorf("expected nil for missing file, got %s", data)
	}
}

func TestLoadSettings_ValidObject(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFile(t, dir, "settings.jsonc", `{
  // Terminal font
  "fontSize": 16,
  // Remap ctrl+t
  "keybinds": [
    { "key": "ctrl+alt+t", "action": "sendKeys", "args": "ctrl+t" },
  ],
}`)

	data, err := LoadSettings()
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
