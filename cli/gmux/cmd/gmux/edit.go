package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// fallbackEditors are probed in order when GMUX_EDIT_FALLBACK is unset.
// Deliberately NOT $EDITOR/$VISUAL: the whole point of `gmux edit` is to
// BE the user's $EDITOR, so consulting it would recurse.
var fallbackEditors = []string{"nano", "vim", "vi"}

// resolveFallbackEditor picks the terminal editor `gmux edit` wraps
// today (the future browser-editor tab replaces this, not the verb).
// GMUX_EDIT_FALLBACK, when set, wins verbatim and may carry flags
// ("vim -u NONE"); otherwise the first of fallbackEditors on PATH.
// lookPath is injected for tests.
func resolveFallbackEditor(getenv func(string) string, lookPath func(string) (string, error)) ([]string, error) {
	if custom := getenv("GMUX_EDIT_FALLBACK"); custom != "" {
		parts := strings.Fields(custom)
		if _, err := lookPath(parts[0]); err != nil {
			return nil, errors.New("GMUX_EDIT_FALLBACK editor not found: " + parts[0])
		}
		return parts, nil
	}
	for _, ed := range fallbackEditors {
		if _, err := lookPath(ed); err == nil {
			return []string{ed}, nil
		}
	}
	return nil, errors.New("no editor found (tried " + strings.Join(fallbackEditors, ", ") + "); set GMUX_EDIT_FALLBACK")
}

// runEdit implements `gmux edit <file>`: open the file in a managed
// session and block until the editor exits, propagating its exit code
// (runSession os.Exit's with the child's code). ForceForeground keeps
// the blocking contract even inside an existing gmux session, where
// runSession would otherwise detach and return immediately.
func runEdit(file string) {
	abs, err := filepath.Abs(file)
	if err != nil {
		log.Fatalf("cannot resolve path %q: %v", file, err)
	}
	editor, err := resolveFallbackEditor(os.Getenv, exec.LookPath)
	if err != nil {
		log.Fatal(err)
	}
	runSession(append(editor, abs), true, runDirectives{ForceForeground: true})
}
