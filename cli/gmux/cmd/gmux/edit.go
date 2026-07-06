package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// runEdit implements `gmux edit [file]`: open the file in a managed
// editor session (adapter "editor") and block until the editor exits,
// propagating its exit code (runSession os.Exit's with the child's
// code). That blocking contract is what makes the verb usable as
// $EDITOR for git commit and friends.
//
// The session's child command is the internal `gmux __edit-child`
// sentinel (see editChild), which the editor adapter matches — the
// same command the launcher entry runs, so CLI- and UI-launched editor
// sessions are indistinguishable. ForceForeground keeps the blocking
// contract even inside an existing gmux session, where runSession
// would otherwise detach and return immediately. In that nested case
// the invoking session's id is recorded as the editor session's
// parent, so the UI can place the editor next to the session that
// spawned it.
func runEdit(file string) {
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot find own binary: %v", err)
	}
	args := []string{self, "__edit-child"}
	if file != "" {
		abs, err := filepath.Abs(file)
		if err != nil {
			log.Fatalf("cannot resolve path %q: %v", file, err)
		}
		args = append(args, abs)
	}
	var parent string
	if os.Getenv("GMUX") == "1" {
		parent = os.Getenv("GMUX_SESSION_ID")
	}
	runSession(args, true, runDirectives{
		ForceForeground: true,
		ParentSessionID: parent,
	})
}

// editChild is the child process of an editor session (internal
// `gmux __edit-child [path]`). It prompts for a path when none was
// given (the + launcher menu can't parameterize one), resolves the
// fallback terminal editor, and execs it — so the editor's exit code
// is the session's exit code, verbatim.
func editChild(path string) int {
	if path == "" {
		fmt.Print("File to edit: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		path = strings.TrimSpace(line)
		if path == "" {
			if err != nil {
				fmt.Fprintln(os.Stderr, "gmux: no file given")
			} else {
				fmt.Fprintln(os.Stderr, "gmux: empty path")
			}
			return 1
		}
	}
	abs, err := filepath.Abs(expandTilde(path))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gmux: cannot resolve path %q: %v\n", path, err)
		return 1
	}
	editor, err := adapters.ResolveFallbackEditor(os.Getenv, exec.LookPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	bin, err := exec.LookPath(editor[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gmux: %s: %v\n", editor[0], err)
		return 1
	}
	argv := append(editor, abs)
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "gmux: exec %s: %v\n", bin, err)
		return 1
	}
	return 0 // unreachable
}

// expandTilde resolves a leading ~/ against $HOME so the interactive
// prompt accepts the same shorthand a shell would.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}
