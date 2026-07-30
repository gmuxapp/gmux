package main

import (
	"os"
	"testing"
)

func TestStdinHasPendingData(t *testing.T) {
	t.Run("pipe with data", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if _, err := w.WriteString("input"); err != nil {
			t.Fatal(err)
		}
		if !stdinHasPendingData(r) {
			t.Error("want pending data")
		}
		got := make([]byte, len("input"))
		if _, err := r.Read(got); err != nil {
			t.Fatal(err)
		}
		if string(got) != "input" {
			t.Errorf("read %q after inspection, want original input", got)
		}
	})

	t.Run("empty open pipe", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if stdinHasPendingData(r) {
			t.Error("empty pipe reported pending data")
		}
	})

	t.Run("dev null", func(t *testing.T) {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if stdinHasPendingData(f) {
			t.Error("/dev/null reported pending data")
		}
	})

	t.Run("regular file with remaining content", func(t *testing.T) {
		f := regularInputFile(t, "input")
		if _, err := f.Seek(2, 0); err != nil {
			t.Fatal(err)
		}
		if !stdinHasPendingData(f) {
			t.Error("want pending data")
		}
		offset, err := f.Seek(0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if offset != 2 {
			t.Errorf("offset=%d after inspection, want 2", offset)
		}
		got := make([]byte, 3)
		if _, err := f.Read(got); err != nil {
			t.Fatal(err)
		}
		if string(got) != "put" {
			t.Errorf("remaining content=%q, want %q", got, "put")
		}
	})

	t.Run("regular file at end", func(t *testing.T) {
		f := regularInputFile(t, "input")
		if _, err := f.Seek(0, 2); err != nil {
			t.Fatal(err)
		}
		if stdinHasPendingData(f) {
			t.Error("file at end reported pending data")
		}
	})
}

func regularInputFile(t *testing.T, content string) *os.File {
	t.Helper()
	path := t.TempDir() + "/stdin"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
