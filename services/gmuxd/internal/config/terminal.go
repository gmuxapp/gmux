package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tidwall/jsonc"
)

// LoadTerminalTheme reads ~/.config/gmux/theme.jsonc, strips JSONC comments,
// and returns the raw JSON. Returns nil (not an error) if the file is missing.
func LoadTerminalTheme() (json.RawMessage, error) {
	return loadJSONC(filepath.Join(filepath.Dir(Path()), "theme.jsonc"))
}

// LoadKeybinds reads ~/.config/gmux/keybinds.jsonc, strips JSONC comments,
// and returns the raw JSON. Returns nil (not an error) if the file is missing.
func LoadKeybinds() (json.RawMessage, error) {
	return loadJSONC(filepath.Join(filepath.Dir(Path()), "keybinds.jsonc"))
}

// loadJSONC reads a file, strips // and /* */ comments and trailing commas,
// then validates the result as JSON. Returns nil for missing files.
func loadJSONC(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	stripped := jsonc.ToJSON(data)

	if !json.Valid(stripped) {
		return nil, fmt.Errorf("parsing %s: invalid JSON after stripping comments", path)
	}

	return json.RawMessage(stripped), nil
}
