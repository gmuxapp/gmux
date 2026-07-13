package adapters

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
)

func init() {
	All = append(All, NewEditor())
}

// Compile-time interface checks.
var (
	_ adapter.CommandTitler    = (*Editor)(nil)
	_ adapter.Launchable       = (*Editor)(nil)
	_ adapter.SessionRegistrar = (*Editor)(nil)
)

// Editor is the adapter for `gmux edit` sessions: a file opened for
// editing, launched blocking so the verb is usable as $EDITOR (git
// commit and friends). Today the session wraps a terminal editor via
// the internal `gmux __edit-child` helper; a future release renders a
// browser-based editor tab instead, keeping this adapter identity
// ("editor" kind, /editor/ URLs, launcher entry) unchanged.
//
// Editor sessions are ephemeral: gmuxd auto-dismisses them from the
// sidebar when their runner deregisters (see the /v1/deregister
// handler), so no session state file is written and nothing is
// rediscovered after a daemon restart.
type Editor struct{}

func NewEditor() *Editor { return &Editor{} }

func (e *Editor) Name() string { return "editor" }

// Discover reports whether an editor session can actually run here:
// either the user pinned a fallback editor or one of the built-in
// candidates is on PATH.
func (e *Editor) Discover() bool {
	_, err := ResolveFallbackEditor(os.Getenv, exec.LookPath)
	return err == nil
}

// Match claims the internal `gmux __edit-child [path]` command that
// both `gmux edit` and the launcher entry run. Matching the sentinel
// (rather than the wrapped editor binary) keeps plain `gmux -- nano x`
// sessions on the shell adapter.
func (e *Editor) Match(command []string) bool {
	return len(command) >= 2 &&
		adapter.BaseName(command[0]) == "gmux" &&
		command[1] == "__edit-child"
}

func (e *Editor) Env(_ adapter.EnvContext) []string { return nil }

// editFilePath extracts the file argument from a matched
// `gmux __edit-child [path]` command; empty when launched without a
// path (the child prompts for one interactively).
func editFilePath(command []string) string {
	if len(command) >= 3 {
		return command[2]
	}
	return ""
}

// CommandTitle shows the file being edited, not the internal command.
func (e *Editor) CommandTitle(command []string) string {
	if p := editFilePath(command); p != "" {
		return filepath.Base(p)
	}
	return "editor"
}

func (e *Editor) Launchers() []adapter.Launcher {
	return []adapter.Launcher{{
		ID:          "editor",
		Label:       "Editor",
		Command:     []string{"gmux", "__edit-child"},
		Description: "Edit a file",
	}}
}

// OnRegister derives the slug from the edited file's name so URLs read
// /<project>/editor/<file-slug>. No state file is written: editor
// sessions are ephemeral by design (auto-dismissed on close).
func (e *Editor) OnRegister(_, _ string, command []string) (adapter.RegistrationInfo, error) {
	slug := adapter.Slugify(filepath.Base(editFilePath(command)))
	if slug == "" {
		slug = "editor"
	}
	return adapter.RegistrationInfo{Slug: slug}, nil
}

// FallbackEditors are probed in order when GMUX_EDIT_FALLBACK is unset.
// Deliberately NOT $EDITOR/$VISUAL: the whole point of `gmux edit` is
// to BE the user's $EDITOR, so consulting it would recurse.
var FallbackEditors = []string{"nano", "vim", "vi"}

// ResolveFallbackEditor picks the terminal editor an editor session
// wraps today (the future browser-editor tab replaces this, not the
// verb). GMUX_EDIT_FALLBACK, when set, wins verbatim and may carry
// flags ("vim -u NONE"); otherwise the first of FallbackEditors on
// PATH. getenv and lookPath are injected for tests.
func ResolveFallbackEditor(getenv func(string) string, lookPath func(string) (string, error)) ([]string, error) {
	// strings.Fields first: a whitespace-only value yields no parts and
	// is treated like unset instead of panicking on parts[0].
	if parts := strings.Fields(getenv("GMUX_EDIT_FALLBACK")); len(parts) > 0 {
		if _, err := lookPath(parts[0]); err != nil {
			return nil, errors.New("GMUX_EDIT_FALLBACK editor not found: " + parts[0])
		}
		return parts, nil
	}
	for _, ed := range FallbackEditors {
		if _, err := lookPath(ed); err == nil {
			return []string{ed}, nil
		}
	}
	return nil, errors.New("no editor found (tried " + strings.Join(FallbackEditors, ", ") + "); set GMUX_EDIT_FALLBACK")
}
