package ptyserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestPTYServerBasicOutput(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo hello-from-pty; sleep 0.2"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	if srv.Pid() == 0 {
		t.Fatal("expected non-zero pid")
	}

	// Connect via WebSocket over Unix socket
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read output — first frame is the reset sequence (always sent on connect),
	// then PTY output follows. Read until we see "hello-from-pty".
	var got []byte
	for i := 0; i < 20; i++ {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		got = append(got, data...)
		if contains(got, "hello-from-pty") {
			break
		}
	}

	if len(got) == 0 {
		t.Fatal("expected output from PTY")
	}
	if !contains(got, "hello-from-pty") {
		t.Errorf("expected 'hello-from-pty' in output, got: %q", string(got))
	}

	// Wait for process to exit
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for child exit")
	}

	if srv.ExitCode() != 0 {
		t.Errorf("expected exit code 0, got %d", srv.ExitCode())
	}
}

func TestPTYServerResize(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 1"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
		Cols:       80,
		Rows:       25,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	msg := ResizeMsg{Type: "resize", Cols: 120, Rows: 40, Source: "web_client"}
	data, _ := json.Marshal(msg)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	for i := 0; i < 5; i++ {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read terminal_resize: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}

		var ack map[string]any
		if err := json.Unmarshal(data, &ack); err != nil {
			continue
		}
		if ack["type"] == "terminal_resize" {
			if ack["cols"] != float64(120) || ack["rows"] != float64(40) {
				t.Fatalf("unexpected terminal_resize payload: %v", ack)
			}
			if ack["source"] != "web_client" {
				t.Fatalf("expected source web_client, got %v", ack["source"])
			}
			return
		}
	}

	t.Fatal("expected terminal_resize event")
}

func TestPTYServerInput(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// cat will echo back what we send
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "read line; echo got:$line"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read all WS messages via a background goroutine. Using context-based
	// read timeouts with nhooyr/websocket closes the connection on cancel,
	// so we use a long-lived reader and poll the accumulated buffer instead.
	var mu sync.Mutex
	var got []byte
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, data...)
			mu.Unlock()
		}
	}()

	// Wait for the initial prompt to settle before sending input.
	time.Sleep(100 * time.Millisecond)

	// Send input
	err = conn.Write(ctx, websocket.MessageBinary, []byte("hello\n"))
	if err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Poll until we see "got:hello" or timeout.
	deadline := time.After(3 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		found := contains(got, "got:hello")
		mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Errorf("expected 'got:hello' in output, got: %q", string(got))
			mu.Unlock()
			goto done
		default:
		}
	}
done:
	<-srv.Done()
}

func TestPTYServerCleanup(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 0.1"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	<-srv.Done()
	srv.Shutdown()

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("expected socket to be removed after shutdown")
	}
}

func TestPTYServerScrollbackReplay(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo replay-test-output; sleep 2"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Wait for output to be produced and buffered
	time.Sleep(300 * time.Millisecond)

	// Now connect — should receive the buffered output immediately via replay
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// First read should contain the replayed scrollback
	var got []byte
	for i := 0; i < 5; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}
		got = append(got, data...)
		if contains(got, "replay-test-output") {
			break
		}
	}

	if !contains(got, "replay-test-output") {
		t.Errorf("expected scrollback replay to contain 'replay-test-output', got: %q", string(got))
	}
}

// TestPTYServerSnapshotBeforeLiveData verifies that a client connecting while
// the child is actively producing output always receives the BSU-wrapped
// snapshot frame as its very first message, not interleaved live data.
func TestPTYServerSnapshotBeforeLiveData(t *testing.T) {
	// BSU = \x1b[?2026h  (Begin Synchronized Update)
	bsu := []byte("\x1b[?2026h")

	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Child produces continuous output so readPTY is always active.
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "while true; do echo active-output-line; done"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Let the child fill the scrollback buffer.
	time.Sleep(200 * time.Millisecond)

	// Connect multiple clients concurrently to increase race probability.
	// Before the fix, at least some of these would receive live data before
	// the snapshot frame.
	const numClients = 20
	type result struct {
		firstBSU bool
		err      error
	}
	results := make(chan result, numClients)

	for i := 0; i < numClients; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
				HTTPClient: &http.Client{
					Transport: &http.Transport{
						DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
							return net.Dial("unix", sockPath)
						},
					},
				},
			})
			if err != nil {
				results <- result{err: err}
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			conn.SetReadLimit(256 * 1024)

			// Read the first message — it must start with BSU.
			_, data, err := conn.Read(ctx)
			if err != nil {
				results <- result{err: err}
				return
			}

			startsBSU := len(data) >= len(bsu)
			if startsBSU {
				for j := 0; j < len(bsu); j++ {
					if data[j] != bsu[j] {
						startsBSU = false
						break
					}
				}
			}
			results <- result{firstBSU: startsBSU}
		}()
	}

	for i := 0; i < numClients; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("client error: %v", r.err)
		}
		if !r.firstBSU {
			t.Errorf("client %d: first message did not start with BSU (snapshot frame); got live data before snapshot", i)
		}
	}
}

// TestPTYServerResizeDedup verifies that sending a resize with the same
// dimensions as the current PTY does NOT deliver SIGWINCH to the child,
// while a resize with different dimensions does.
func TestPTYServerResizeDedup(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// The child uses a SIGWINCH trap that writes a marker to stdout.
	// This lets us observe whether SIGWINCH was actually delivered.
	srv, err := New(Config{
		Command: []string{"bash", "-c", `
			trap 'echo WINCH_FIRED' SIGWINCH
			echo ready
			# Keep running so we can send resize messages.
			while true; do sleep 0.1; done
		`},
		Cwd:        "/tmp",
		SocketPath: sockPath,
		Cols:       80,
		Rows:       25,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Read all WS messages into a shared buffer via a background goroutine.
	var mu sync.Mutex
	var allOutput []byte
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			allOutput = append(allOutput, data...)
			mu.Unlock()
		}
	}()

	// Wait until we see "ready" in the output, confirming the trap is set.
	deadline := time.After(5 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		ready := contains(allOutput, "ready")
		mu.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("child never became ready, got: %q", allOutput)
			mu.Unlock()
		default:
		}
	}

	// Send a resize with the SAME dimensions (80x25). This should NOT
	// trigger SIGWINCH, so no "WINCH_FIRED" output should appear.
	sameResize, _ := json.Marshal(ResizeMsg{Type: "resize", Cols: 80, Rows: 25})
	if err := conn.Write(ctx, websocket.MessageText, sameResize); err != nil {
		t.Fatalf("write same-size resize: %v", err)
	}

	// Give the child time to receive and process a SIGWINCH if one were sent.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if contains(allOutput, "WINCH_FIRED") {
		t.Error("same-size resize should not trigger SIGWINCH, but WINCH_FIRED was received")
	}
	mu.Unlock()

	// Now send a resize with DIFFERENT dimensions. This should trigger SIGWINCH.
	diffResize, _ := json.Marshal(ResizeMsg{Type: "resize", Cols: 120, Rows: 40})
	if err := conn.Write(ctx, websocket.MessageText, diffResize); err != nil {
		t.Fatalf("write different-size resize: %v", err)
	}

	deadline = time.After(3 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		fired := contains(allOutput, "WINCH_FIRED")
		mu.Unlock()
		if fired {
			return // success
		}
		select {
		case <-deadline:
			t.Error("different-size resize should trigger SIGWINCH, but WINCH_FIRED was not received")
			return
		default:
		}
	}
}

func TestLastDECTCEM(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantVis bool
		wantOK  bool
	}{
		{"show cursor", []byte("\x1b[?25h"), true, true},
		{"hide cursor", []byte("\x1b[?25l"), false, true},
		{"no sequence", []byte("hello world"), false, false},
		{"show then hide", []byte("\x1b[?25hstuff\x1b[?25l"), false, true},
		{"hide then show", []byte("\x1b[?25lstuff\x1b[?25h"), true, true},
		{"embedded in TUI frame", []byte("\x1b[?25l\x1b[1;1HContent\x1b[?25h"), true, true},
		{"too short", []byte("\x1b[?2"), false, false},
		{"empty", []byte{}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vis, ok := lastDECTCEM(tt.data)
			if ok != tt.wantOK {
				t.Errorf("found = %v, want %v", ok, tt.wantOK)
			}
			if ok && vis != tt.wantVis {
				t.Errorf("visible = %v, want %v", vis, tt.wantVis)
			}
		})
	}
}

// TestPTYServerCursorStateReplay verifies that the replay frame restores
// DECTCEM cursor visibility. A child that hides the cursor should produce
// a replay frame ending with ESC[?25l (before ESU).
func TestPTYServerCursorStateReplay(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Child hides the cursor and stays alive.
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "printf '\x1b[?25l'; sleep 5"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Let the child produce output.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sockPath)
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The frame should end with: ESC[?25l (cursor restore) + ESC[?2026l (ESU)
	hideThenESU := "\x1b[?25l\x1b[?2026l"
	if !contains(data, hideThenESU) {
		t.Errorf("replay frame should contain cursor-hide before ESU, got tail: %q", data[max(0, len(data)-30):])
	}
}

func contains(data []byte, substr string) bool {
	return len(data) > 0 && len(substr) > 0 &&
		stringContains(string(data), substr)
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
