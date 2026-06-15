package main

import (
	"bytes"
	"testing"
)

func TestParseCLIDumpEnv(t *testing.T) {
	c, err := parseCLI([]string{"__dump-env"})
	if err != nil {
		t.Fatalf("parseCLI(__dump-env) error: %v", err)
	}
	if c.mode != modeDumpEnv {
		t.Errorf("mode = %v, want modeDumpEnv", c.mode)
	}
}

func TestWriteNulEnv(t *testing.T) {
	var buf bytes.Buffer
	// A value with an embedded newline (as bash exports functions) must
	// survive: that's the reason for NUL delimiting over newlines.
	env := []string{"A=1", "B=two\nlines", "C="}
	if err := writeNulEnv(&buf, env); err != nil {
		t.Fatalf("writeNulEnv: %v", err)
	}
	want := "A=1\x00B=two\nlines\x00C=\x00"
	if buf.String() != want {
		t.Errorf("writeNulEnv = %q, want %q", buf.String(), want)
	}
}
