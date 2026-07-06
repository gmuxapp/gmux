package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
