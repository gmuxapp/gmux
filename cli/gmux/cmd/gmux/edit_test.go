package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionEditorEnv(t *testing.T) {
	self := func() (string, error) { return "/opt/gmux/bin/gmux", nil }
	envWith := func(set ...string) func(string) (string, bool) {
		m := map[string]bool{}
		for _, k := range set {
			m[k] = true
		}
		return func(k string) (string, bool) {
			if m[k] {
				return "something", true
			}
			return "", false
		}
	}

	t.Run("defaults both when neither is set", func(t *testing.T) {
		got := sessionEditorEnv(envWith(), self)
		want := []string{"EDITOR=/opt/gmux/bin/gmux edit", "VISUAL=/opt/gmux/bin/gmux edit"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("never overrides a user-set value", func(t *testing.T) {
		if got := sessionEditorEnv(envWith("EDITOR", "VISUAL"), self); len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("fills only the unset one", func(t *testing.T) {
		got := sessionEditorEnv(envWith("EDITOR"), self)
		if len(got) != 1 || got[0] != "VISUAL=/opt/gmux/bin/gmux edit" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("falls back to bare gmux without a self path", func(t *testing.T) {
		noSelf := func() (string, error) { return "", os.ErrNotExist }
		got := sessionEditorEnv(envWith("VISUAL"), noSelf)
		if len(got) != 1 || got[0] != "EDITOR=gmux edit" {
			t.Errorf("got %v", got)
		}
	})
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~/notes.txt":  filepath.Join(home, "notes.txt"),
		"~":            home,
		"/abs/path":    "/abs/path",
		"relative.txt": "relative.txt",
		"~user/notes":  "~user/notes", // ~user expansion not supported
	}
	for in, want := range cases {
		if got := expandTilde(in); got != want {
			t.Errorf("expandTilde(%q) = %q, want %q", in, got, want)
		}
	}
}
