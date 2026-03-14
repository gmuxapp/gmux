package ringbuf

import (
	"bytes"
	"testing"
)

func TestWriteAndSnapshot(t *testing.T) {
	r := New(10)
	r.Write([]byte("hello"))

	got := r.Snapshot()
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("expected 'hello', got %q", got)
	}
	if r.Len() != 5 {
		t.Errorf("expected len 5, got %d", r.Len())
	}
}

func TestWrapAround(t *testing.T) {
	r := New(8)
	r.Write([]byte("abcdefgh")) // fills exactly
	r.Write([]byte("ij"))       // overwrites first 2

	got := r.Snapshot()
	// Should be: cdefghij (oldest=c, newest=j)
	if !bytes.Equal(got, []byte("cdefghij")) {
		t.Errorf("expected 'cdefghij', got %q", got)
	}
	if r.Len() != 8 {
		t.Errorf("expected len 8, got %d", r.Len())
	}
}

func TestMultipleWraps(t *testing.T) {
	r := New(4)
	r.Write([]byte("abcdef")) // wraps: buf=[efcd], pos=2, full=true
	r.Write([]byte("gh"))     // wraps: buf=[efgh], pos=0... wait

	// After "abcdef" (size 4):
	//   write a→pos0, b→pos1, c→pos2, d→pos3, e→pos0(wrap), f→pos1
	//   buf=[efcd], pos=2, full=true
	// After "gh":
	//   g→pos2, h→pos3
	//   buf=[efgh], pos=0, full=true
	// Snapshot: from pos=0 → efgh
	got := r.Snapshot()
	if !bytes.Equal(got, []byte("efgh")) {
		t.Errorf("expected 'efgh', got %q", got)
	}
}

func TestEmpty(t *testing.T) {
	r := New(10)
	got := r.Snapshot()
	if len(got) != 0 {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestReset(t *testing.T) {
	r := New(10)
	r.Write([]byte("hello"))
	r.Reset()
	if r.Len() != 0 {
		t.Errorf("expected len 0 after reset, got %d", r.Len())
	}
	got := r.Snapshot()
	if len(got) != 0 {
		t.Errorf("expected empty after reset, got %q", got)
	}
}

func TestLargeWrite(t *testing.T) {
	r := New(4)
	// Write more than buffer size in one call
	r.Write([]byte("abcdefghij"))
	// Last 4 bytes should survive: ghij
	got := r.Snapshot()
	if !bytes.Equal(got, []byte("ghij")) {
		t.Errorf("expected 'ghij', got %q", got)
	}
}
