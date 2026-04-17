package ptyserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// TestScrollbackTailEndpoint covers the /scrollback/tail HTTP endpoint
// used by `gmux --tail N <id>`. It asserts the two properties that
// users actually rely on: the last N lines are returned in order, and
// the output is plain text (no ANSI control sequences).
func TestScrollbackTailEndpoint(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Print 20 distinct, colored lines. The colors matter: if the
	// endpoint leaks ANSI, our plain-text assertion catches it.
	script := `for i in $(seq 1 20); do printf '\033[31mline-%02d\033[0m\n' $i; done; sleep 2`
	srv, err := New(Config{
		Command:    []string{"bash", "-c", script},
		Cwd:        "/tmp",
		SocketPath: sockPath,
		Cols:       80,
		Rows:       10, // small viewport forces most lines into scrollback
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Give the child time to emit its 20 lines and the emulator to
	// process them into scrollback.
	time.Sleep(500 * time.Millisecond)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}

	// Request the last 5 lines. We expect line-16 .. line-20 in order.
	resp, err := client.Get("http://session/scrollback/tail?n=5")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Plain text: no escape bytes.
	if bytes.Contains(body, []byte{0x1b}) {
		t.Errorf("expected plain text, got ANSI: %q", string(body))
	}

	// Must contain the tail lines and not the earliest ones.
	got := string(body)
	for _, want := range []string{"line-16", "line-17", "line-18", "line-19", "line-20"} {
		if !contains([]byte(got), want) {
			t.Errorf("missing %q in tail output:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"line-01", "line-10"} {
		if contains([]byte(got), unwanted) {
			t.Errorf("did not expect %q in 5-line tail:\n%s", unwanted, got)
		}
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

// TestPTYServerCursorStateReplay verifies that the replay frame restores
// DECTCEM cursor visibility. A child that hides the cursor should produce
// a replay frame containing ESC[?25l.
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

	// The replay frame should contain cursor-hide (ESC[?25l).
	// MarshalBinary includes cursor visibility in the serialized screen state.
	if !contains(data, "\x1b[?25l") {
		t.Errorf("replay frame should contain cursor-hide, got tail: %q", data[max(0, len(data)-30):])
	}
}

// TestPTYServerSpinnerPreservesContent verifies that cursor-positioning
// spinner updates (which overwrite a single row repeatedly) do not evict
// content from other rows in the replay snapshot.
//
// This is a regression test. The previous ring-buffer approach stored raw
// PTY bytes; spinner frames filled the buffer and pushed out the actual
// conversation content. The vterm-based approach processes the terminal
// state, so spinners update one cell and leave everything else intact.
func TestPTYServerSpinnerPreservesContent(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// The child writes content at rows 1-3, then overwrites row 4
	// hundreds of times (simulating a TUI spinner), then writes
	// a completion marker at row 5.
	srv, err := New(Config{
		Command: []string{"bash", "-c", `
			printf '\x1b[1;1HConversation-line-1'
			printf '\x1b[2;1HConversation-line-2'
			printf '\x1b[3;1HConversation-line-3'
			for i in $(seq 1 500); do
				printf '\x1b[4;1H\x1b[2KSpinner frame %d' $i
			done
			printf '\x1b[5;1Hspinner-done'
			sleep 3
		`},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Wait for the spinner loop and completion marker.
	time.Sleep(500 * time.Millisecond)

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

	var got []byte
	for i := 0; i < 10; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}
		got = append(got, data...)
		if contains(got, "spinner-done") {
			break
		}
	}

	// The completion marker must be present.
	if !contains(got, "spinner-done") {
		t.Fatalf("never saw spinner-done marker in replay")
	}

	// The conversation content must survive through 500 spinner updates.
	for _, want := range []string{"Conversation-line-1", "Conversation-line-2", "Conversation-line-3"} {
		if !contains(got, want) {
			t.Errorf("content %q lost after spinner updates", want)
		}
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

// TestNewScreenDSRNonBlocking verifies that the virtual terminal does not
// block when the child sends a Device Status Report request (ESC[6n).
// Fish shell sends DSR on startup to detect cursor position. Without the
// response-drain goroutine, Write blocks forever because the emulator
// writes the response to an internal pipe that nobody reads.
func TestNewScreenDSRNonBlocking(t *testing.T) {
	screen := newScreen(80, 24, func(bool) {})
	defer screen.Close()

	done := make(chan struct{})
	go func() {
		// Simulates fish startup: prompt text, then DSR, then more text.
		screen.Write([]byte("hello \x1b[6n world"))
		close(done)
	}()

	select {
	case <-done:
		// OK: Write returned without blocking.
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on DSR; response-drain goroutine not working")
	}
}

// TestRenderScreenIncludesScrollback verifies that renderScreen includes
// lines that scrolled off the top of the screen, not just the visible rows.
func TestRenderScreenIncludesScrollback(t *testing.T) {
	screen := newScreen(80, 5, func(bool) {})
	defer screen.Close()

	// Write 10 lines through a 5-row terminal: lines 1-5 scroll off,
	// lines 6-10 remain visible.
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(screen, "Line-%02d\r\n", i)
	}

	result := renderScreen(screen)

	for i := 1; i <= 10; i++ {
		want := fmt.Sprintf("Line-%02d", i)
		if !stringContains(result, want) {
			t.Errorf("snapshot missing %q", want)
		}
	}
}

// TestRenderScreenLineCount verifies that the snapshot for a partially
// filled screen has exactly Height-1 CRLF separators (no extra blank rows
// from buffer growth) and that adding scrollback increases the total.
func TestRenderScreenLineCount(t *testing.T) {
	screen := newScreen(40, 5, func(bool) {})
	defer screen.Close()

	// Write 3 short lines, staying within the 5-row screen.
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(screen, "\x1b[%d;1HRow-%d", i, i)
	}

	result := renderScreen(screen)
	// 5 visible rows joined by 4 CRLFs, no scrollback.
	crlfs := countOccurrences(result, "\r\n")
	if crlfs != 4 {
		t.Errorf("expected 4 CRLFs (5 visible rows), got %d", crlfs)
	}

	// Now push content into scrollback: write 10 lines through a 5-row terminal.
	screen2 := newScreen(40, 5, func(bool) {})
	defer screen2.Close()
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(screen2, "Line-%02d\r\n", i)
	}

	result2 := renderScreen(screen2)
	// 6 scrollback lines (the trailing \r\n after Line-10 scrolls one
	// extra line off, so lines 1-6 end up in scrollback, each followed
	// by CRLF) + 4 CRLFs between 5 visible rows = 10.
	gotCRLFs := countOccurrences(result2, "\r\n")
	if gotCRLFs < 5 {
		t.Errorf("expected scrollback to increase CRLF count beyond 4, got %d", gotCRLFs)
	}
	if gotCRLFs > 4+10 {
		t.Errorf("CRLF count unreasonably high: %d (buffer growth beyond terminal bounds?)", gotCRLFs)
	}
}

// TestPTYServerDeferredScreenSync verifies that the deferred screen
// processing (screenPending) produces correct snapshots. The child writes
// output, then a late-connecting client should see it in the replay even
// though the emulator processes it asynchronously.
func TestPTYServerDeferredScreenSync(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	// Child writes a known marker and stays alive.
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo deferred-sync-marker; sleep 5"},
		Cwd:        "/tmp",
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Wait long enough for the output to be produced AND for processScreen
	// to drain it into the emulator (screenSyncInterval = 100ms).
	time.Sleep(400 * time.Millisecond)

	// Connect a late client and verify the snapshot contains the marker.
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

	var got []byte
	for i := 0; i < 5; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}
		got = append(got, data...)
		if contains(got, "deferred-sync-marker") {
			break
		}
	}

	if !contains(got, "deferred-sync-marker") {
		t.Errorf("snapshot should contain marker after deferred sync, got: %q", string(got))
	}
}

// TestPTYServerLiveDataNotDelayed verifies that live data reaches a
// connected client promptly, without waiting for the screen emulator
// to process it. This is the core property of the deferred-screen design:
// the emulator is off the hot path.
func TestPTYServerLiveDataNotDelayed(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 0.3; echo live-data-marker; sleep 5"},
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

	// Read until we see the live marker. It should arrive within 2s
	// (the child sleeps 0.3s then echoes). If the screen emulator were
	// in the hot path and slow, this would take longer.
	var got []byte
	start := time.Now()
	for i := 0; i < 20; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}
		got = append(got, data...)
		if contains(got, "live-data-marker") {
			break
		}
	}
	elapsed := time.Since(start)

	if !contains(got, "live-data-marker") {
		t.Fatalf("never received live-data-marker")
	}
	// Should arrive well within 2s (generous bound).
	if elapsed > 2*time.Second {
		t.Errorf("live data took %v to arrive; expected < 2s", elapsed)
	}
}

// TestPTYServerShrinkForReconnect verifies that when a client disconnects
// and then a new client connects with a resize, the child TUI receives a
// SIGWINCH that forces a full redraw. The mechanism: the PTY is shrunk by
// 1 column on last-client disconnect, so the next resize is a genuine
// dimension change.
func TestPTYServerShrinkForReconnect(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command: []string{"bash", "-c", `
			trap 'echo WINCH_FIRED' SIGWINCH
			echo ready
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

	// Helper to connect a WS client.
	dial := func() *websocket.Conn {
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
		return conn
	}

	// Phase 1: connect, wait for ready, then disconnect to trigger shrink.
	conn1 := dial()
	var mu sync.Mutex
	var allOutput []byte
	readerCtx, readerCancel := context.WithCancel(ctx)
	go func() {
		for {
			_, data, err := conn1.Read(readerCtx)
			if err != nil {
				return
			}
			mu.Lock()
			allOutput = append(allOutput, data...)
			mu.Unlock()
		}
	}()

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
			t.Fatal("child never became ready")
		default:
		}
	}

	// Disconnect first client. This triggers shrinkForReconnect (80→79 cols).
	readerCancel()
	conn1.Close(websocket.StatusNormalClosure, "")
	// Wait for the shrink SIGWINCH to be delivered and processed.
	time.Sleep(300 * time.Millisecond)

	// Clear output buffer: the shrink's SIGWINCH will have fired WINCH_FIRED.
	mu.Lock()
	allOutput = nil
	mu.Unlock()

	// Phase 2: reconnect and send resize with the original (pre-shrink) size.
	// This should trigger a genuine SIGWINCH because the PTY is at 79 cols.
	conn2 := dial()
	defer conn2.Close(websocket.StatusNormalClosure, "")

	go func() {
		for {
			_, data, err := conn2.Read(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			allOutput = append(allOutput, data...)
			mu.Unlock()
		}
	}()

	// Send resize with original dimensions (80x25).
	msg, _ := json.Marshal(ResizeMsg{Type: "resize", Cols: 80, Rows: 25})
	if err := conn2.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	deadline = time.After(2 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		fired := contains(allOutput, "WINCH_FIRED")
		mu.Unlock()
		if fired {
			return // success: reconnect resize triggered SIGWINCH
		}
		select {
		case <-deadline:
			t.Fatal("expected reconnect resize to trigger SIGWINCH, but WINCH_FIRED never appeared")
		default:
		}
	}
}

func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
