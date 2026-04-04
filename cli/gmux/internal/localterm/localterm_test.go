package localterm

import (
	"bytes"
	"os"
	"sync"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// TestFocusInDetection verifies the focusIn byte pattern matches correctly.
func TestFocusInDetection(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{"focus in alone", []byte("\x1b[I"), true},
		{"focus in with surrounding data", []byte("hello\x1b[Iworld"), true},
		{"focus out only", []byte("\x1b[O"), false},
		{"plain input", []byte("ls -la\n"), false},
		{"empty", []byte{}, false},
		{"partial escape", []byte("\x1b["), false},
		{"focus in at end", []byte("data\x1b[I"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bytes.Contains(tt.input, focusIn)
			if got != tt.want {
				t.Errorf("Contains(%q, focusIn) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestReadStdinFocusInTriggersResize verifies that ESC [I in the input
// stream triggers a resize callback with the terminal's current dimensions,
// and that all data (including the focus sequence) is forwarded to the PTY.
func TestReadStdinFocusInTriggersResize(t *testing.T) {
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer pts.Close()

	oldState, err := term.MakeRaw(int(pts.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Restore(int(pts.Fd()), oldState)

	pty.Setsize(ptmx, &pty.Winsize{Cols: 132, Rows: 43})

	var ptyBuf safeBuffer
	var mu sync.Mutex
	var resizeCols, resizeRows uint16
	resizeCh := make(chan struct{}, 1)

	devNull, _ := os.Open(os.DevNull)
	defer devNull.Close()

	a := &Attach{
		stdin:     pts,
		stdout:    devNull,
		ptyWriter: &ptyBuf,
		resizeFn: func(cols, rows uint16) {
			mu.Lock()
			resizeCols = cols
			resizeRows = rows
			mu.Unlock()
			select {
			case resizeCh <- struct{}{}:
			default:
			}
		},
		done: make(chan struct{}),
	}

	// Run readStdin in background.
	go a.readStdin()

	// Write focus-in through the master side.
	ptmx.Write([]byte("before\x1b[Iafter"))

	// Wait for the resize callback.
	<-resizeCh

	// Close master to terminate readStdin.
	ptmx.Close()
	<-a.done

	// Verify data was forwarded.
	got := ptyBuf.String()
	if !bytes.Contains([]byte(got), focusIn) {
		t.Errorf("PTY writer should contain the focus sequence, got %q", got)
	}

	// Verify resize was called with the PTY dimensions.
	mu.Lock()
	defer mu.Unlock()
	if resizeCols != 132 || resizeRows != 43 {
		t.Errorf("resizeFn called with (%d, %d), want (132, 43)", resizeCols, resizeRows)
	}
}

// TestReadStdinNoFocusNoResize verifies that normal input without ESC [I
// does not trigger a resize callback.
func TestReadStdinNoFocusNoResize(t *testing.T) {
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer pts.Close()

	oldState, err := term.MakeRaw(int(pts.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Restore(int(pts.Fd()), oldState)

	var ptyBuf safeBuffer
	resizeCalled := false

	devNull, _ := os.Open(os.DevNull)
	defer devNull.Close()

	a := &Attach{
		stdin:     pts,
		stdout:    devNull,
		ptyWriter: &ptyBuf,
		resizeFn:  func(cols, rows uint16) { resizeCalled = true },
		done:      make(chan struct{}),
	}

	go a.readStdin()

	ptmx.Write([]byte("hello world"))
	// Brief yield to let readStdin process.
	// Then close master to terminate readStdin.
	ptmx.Close()
	<-a.done

	if resizeCalled {
		t.Error("resizeFn should not be called without focus-in sequence")
	}
}

// safeBuffer is a bytes.Buffer safe for concurrent Write and String.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
