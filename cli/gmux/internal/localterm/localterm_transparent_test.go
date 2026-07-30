package localterm

import (
	"os"
	"testing"

	"github.com/creack/pty"
)

func TestIsTransparentFiles(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("allocate PTY: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeR.Close()
	defer pipeW.Close()

	for _, tc := range []struct {
		name        string
		stdin       *os.File
		stdout      *os.File
		transparent bool
	}{
		{"both tty", tty, tty, true},
		{"tty stdin only", tty, pipeW, false},
		{"tty stdout only", pipeR, tty, false},
		{"neither tty", pipeR, pipeW, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransparentFiles(tc.stdin, tc.stdout); got != tc.transparent {
				t.Errorf("IsTransparentFiles()=%v, want %v", got, tc.transparent)
			}
		})
	}
}

func TestNewRejectsExplicitNonTerminalFiles(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := New(Config{Stdin: f, Stdout: f}); err == nil {
		t.Fatal("New accepted non-terminal input and output")
	}
}
