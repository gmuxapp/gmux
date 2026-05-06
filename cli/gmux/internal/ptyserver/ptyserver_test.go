package ptyserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"nhooyr.io/websocket"
)

// mustBindSocket binds a Unix socket at sockPath for tests, failing
// the test on any error. Mirrors run.go's bind-before-setup pattern
// so tests exercise the same Listener-handoff contract real callers
// use.
func mustBindSocket(t *testing.T, sockPath string) net.Listener {
	t.Helper()
	ln, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket %s: %v", sockPath, err)
	}
	return ln
}

func TestPTYServerBasicOutput(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo hello-from-pty; sleep 0.2"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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

// TestInputEndpoint covers `POST /input` — the HTTP shortcut used by
// `gmux --send`. The contract is simple: bytes in the body reach the
// child's stdin as if typed. We exercise that by having the child
// read a line and echo it back; if the POST path works, the echo
// appears in the WS stream.
//
// This doubles as a regression test for the access-control model: the
// endpoint is on the session's owner-only Unix socket, and the fact
// that the test can hit it at all means we correctly didn't add any
// auth wrapper that would break local callers.
func TestInputEndpoint(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "read line; echo got:$line"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Give the child a moment to issue its read() syscall before we
	// deliver bytes. Without this the bytes arrive before the read
	// is posted and get dropped by the tty canonical mode buffer on
	// some kernels.
	time.Sleep(100 * time.Millisecond)

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 2 * time.Second,
	}

	resp, err := client.Post("http://session/input", "application/octet-stream",
		bytes.NewReader([]byte("hello\n")))
	if err != nil {
		t.Fatalf("post /input: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	// Observe the child's echo via a WS attach. We intentionally don't
	// mix channels — posting via HTTP and observing via WS — because
	// that's also what `gmux --send` does (POST) while another client
	// (the web UI or `gmux --attach`) reads (WS).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	var got []byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		got = append(got, data...)
		if contains(got, "got:hello") {
			return
		}
	}
	t.Errorf("expected 'got:hello' in output, got: %q", string(got))
}

// TestInputEndpointEmpty covers the degenerate case: POSTing nothing
// must succeed without writing anything to the PTY. Matters because a
// user piping an empty file into `gmux --send` should be a no-op,
// not a 500.
func TestInputEndpointEmpty(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 1"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: time.Second,
	}
	resp, err := client.Post("http://session/input", "application/octet-stream", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("post /input: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

// TestPTYServerScrollbackPersistence verifies the runner-side
// half of the dead-session replay contract: PTY output is teed to
// the configured Scrollback sink, and the sink is closed AFTER the
// final PTY drain so fast-exiting commands' last bytes land on
// disk. A regression in either flush() or waitChild's close
// ordering surfaces here as missing bytes in the file.
func TestPTYServerScrollbackPersistence(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	scrollbackPath := filepath.Join(t.TempDir(), "persist", scrollback.ActiveName)

	sink, err := scrollback.Open(scrollbackPath)
	if err != nil {
		t.Fatalf("scrollback.Open: %v", err)
	}

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo SCROLLBACK-MARKER-XYZ"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Scrollback: sink,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// Wait for child exit and the post-drain close to run.
	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit in time")
	}
	select {
	case <-srv.PTYDone():
	case <-time.After(time.Second):
		t.Fatal("PTY did not drain in time")
	}
	// PTYDone closing implies waitChild has progressed past
	// <-s.ptyDone, which is where the scrollback Close runs. Give
	// it a moment to land before reading.
	time.Sleep(50 * time.Millisecond)

	data, err := os.ReadFile(scrollbackPath)
	if err != nil {
		t.Fatalf("read scrollback: %v", err)
	}
	if !bytes.Contains(data, []byte("SCROLLBACK-MARKER-XYZ")) {
		t.Errorf("scrollback missing child output.\ngot: %q", data)
	}
}

// TestPTYServerScrollbackNotConfigured verifies the runner serves
// live data normally when no scrollback sink is configured. This
// is the fallback path when scrollback.Open fails (disk full,
// permission denied) and run.go leaves Config.Scrollback unset.
func TestPTYServerScrollbackNotConfigured(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo no-scrollback-here"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		// Scrollback intentionally nil.
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit in time")
	}
	if srv.ExitCode() != 0 {
		t.Errorf("unexpected exit code %d", srv.ExitCode())
	}
}

func TestPTYServerCleanup(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 0.1"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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
		Listener:   mustBindSocket(t, sockPath),
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

// syncBuffer is a bytes.Buffer safe for concurrent Write and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConfigLocalOutReceivesFastExitOutput verifies that a writer
// supplied via Config.LocalOut is wired before the PTY server starts
// reading, so a command that exits before any caller could have
// plausibly called SetLocalOutput still has its output delivered.
//
// Regression test for the race where `gmux echo hi` in a real terminal
// dropped "hi" because SetLocalOutput was called after readPTY had
// already flushed the (then nil) local output.
func TestConfigLocalOutReceivesFastExitOutput(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	var out syncBuffer

	srv, err := New(Config{
		Command:    []string{"bash", "-c", "echo fast-exit-marker"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		LocalOut:   &out,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit")
	}
	select {
	case <-srv.PTYDone():
	case <-time.After(2 * time.Second):
		t.Fatal("PTYDone never closed")
	}

	if !strings.Contains(out.String(), "fast-exit-marker") {
		t.Errorf("expected LocalOut to contain 'fast-exit-marker', got %q", out.String())
	}
}

// TestPTYDoneClosesAfterFinalFlush verifies that PTYDone closes strictly
// after every byte the child produced has been delivered to LocalOut.
// If PTYDone closed before the final flush, callers that wait on it
// before detaching a local terminal would still lose the trailing bytes.
//
// Regression test for the race where output produced immediately before
// the child exited was swallowed because Done() fired, the caller
// detached, and the final readPTY flush arrived at a detached sink.
func TestPTYDoneClosesAfterFinalFlush(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	var out syncBuffer

	// The sleep before the final echo defeats the coalesce timer: the
	// "END-OF-OUTPUT" bytes arrive right before the child exits, so they
	// must survive the Done()-to-ptyDone drain to reach LocalOut.
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 0.3; echo END-OF-OUTPUT"},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		LocalOut:   &out,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit")
	}
	select {
	case <-srv.PTYDone():
	case <-time.After(2 * time.Second):
		t.Fatal("PTYDone never closed after child exit")
	}

	if !strings.Contains(out.String(), "END-OF-OUTPUT") {
		t.Errorf("expected LocalOut to contain 'END-OF-OUTPUT' by the time PTYDone closes, got %q", out.String())
	}
}

// envValue returns the last value for name in env, or "" if not set.
// Mirrors POSIX semantics where later entries shadow earlier ones.
func envValue(env []string, name string) string {
	prefix := name + "="
	val := ""
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			val = e[len(prefix):]
		}
	}
	return val
}

// When the parent has no TERM (e.g. gmuxd launched by systemd, then
// forking a shell into a session) curses programs like lazygit abort
// with "terminal entry not found: term not set". buildChildEnv must
// default TERM=xterm-256color so children always have a usable
// terminfo entry.
func TestBuildChildEnv_DefaultsTermWhenAbsent(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/test"}
	env := buildChildEnv(parent, nil, "1.2.3")

	if got := envValue(env, "TERM"); got != "xterm-256color" {
		t.Errorf("TERM = %q, want xterm-256color", got)
	}
}

// An existing TERM in the parent must win: we never clobber a real
// terminal's terminfo entry.
func TestBuildChildEnv_PreservesParentTerm(t *testing.T) {
	parent := []string{"TERM=screen-256color"}
	env := buildChildEnv(parent, nil, "1.2.3")

	if got := envValue(env, "TERM"); got != "screen-256color" {
		t.Errorf("TERM = %q, want parent value screen-256color", got)
	}
}

// Caller-supplied env (e.g. an adapter) can override a parent TERM
// without ptyserver layering one on top.
func TestBuildChildEnv_ExtraOverridesParentTerm(t *testing.T) {
	parent := []string{"TERM=screen-256color"}
	extra := []string{"TERM=tmux-256color"}
	env := buildChildEnv(parent, extra, "1.2.3")

	if got := envValue(env, "TERM"); got != "tmux-256color" {
		t.Errorf("TERM = %q, want extra value tmux-256color", got)
	}
}

// Terminal capability advertisements always win over the parent: the
// frontend's actual capabilities don't depend on what the parent
// thinks. fastfetch reads TERM_PROGRAM/TERM_PROGRAM_VERSION, so the
// version must reflect the real build, not whatever the parent had.
func TestBuildChildEnv_AdvertisesTerminalCapabilities(t *testing.T) {
	parent := []string{
		"TERM_PROGRAM=iTerm.app",
		"TERM_PROGRAM_VERSION=3.4.0",
		"COLORTERM=",
	}
	env := buildChildEnv(parent, nil, "1.2.3")

	if got := envValue(env, "TERM_PROGRAM"); got != "gmux" {
		t.Errorf("TERM_PROGRAM = %q, want gmux", got)
	}
	if got := envValue(env, "TERM_PROGRAM_VERSION"); got != "1.2.3" {
		t.Errorf("TERM_PROGRAM_VERSION = %q, want 1.2.3", got)
	}
	if got := envValue(env, "COLORTERM"); got != "truecolor" {
		t.Errorf("COLORTERM = %q, want truecolor", got)
	}
	if got := envValue(env, "KITTY_WINDOW_ID"); got != "1" {
		t.Errorf("KITTY_WINDOW_ID = %q, want 1", got)
	}
}

// An empty version (e.g. someone forgot to wire the ldflag) must not
// produce a bare "TERM_PROGRAM_VERSION=" — fall back to "dev" so
// downstream parsers always see a non-empty value.
func TestBuildChildEnv_EmptyVersionFallsBackToDev(t *testing.T) {
	env := buildChildEnv(nil, nil, "")

	if got := envValue(env, "TERM_PROGRAM_VERSION"); got != "dev" {
		t.Errorf("TERM_PROGRAM_VERSION = %q, want dev", got)
	}
}

// End-to-end check that buildChildEnv's output actually reaches a
// spawned child through cmd.Env. The unit tests above cover composition
// rules; this guards against regressions in how New wires the env into
// exec.Command.
func TestNewSpawnsChildWithComposedEnv(t *testing.T) {
	// t.Setenv registers cleanup to restore the original TERM after the
	// test; we then Unsetenv so os.Environ() truly lacks a TERM entry
	// (TERM="" would still prefix-match buildChildEnv's hasEnv check).
	t.Setenv("TERM", "")
	if err := os.Unsetenv("TERM"); err != nil {
		t.Fatalf("unset TERM: %v", err)
	}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	var out syncBuffer

	srv, err := New(Config{
		Command:    []string{"sh", "-c", `printf "TERM=%s|TPV=%s|TP=%s\n" "$TERM" "$TERM_PROGRAM_VERSION" "$TERM_PROGRAM"`},
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		LocalOut:   &out,
		Version:    "9.9.9-test",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	select {
	case <-srv.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit")
	}
	<-srv.PTYDone()

	want := "TERM=xterm-256color|TPV=9.9.9-test|TP=gmux"
	if !strings.Contains(out.String(), want) {
		t.Errorf("child env: want substring %q, got: %q", want, out.String())
	}
}

func TestBindSocketStaleFile(t *testing.T) {
	// A leftover socket file with no listener is not a real owner;
	// BindSocket should remove it and listen successfully.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stale.sock")
	if err := os.WriteFile(sockPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket on stale file: %v", err)
	}
	defer ln.Close()

	// A second bind on the same path should now see a live owner
	// (the first listener) and refuse with ErrSocketInUse.
	if _, err := BindSocket(sockPath); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("second BindSocket: want ErrSocketInUse, got %v", err)
	}
}

func TestBindSocketLiveOwnerLeftIntact(t *testing.T) {
	// On collision, BindSocket must NOT remove or replace the
	// existing socket file; the live owner has to keep working.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "live.sock")

	first, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("first BindSocket: %v", err)
	}
	defer first.Close()

	if _, err := BindSocket(sockPath); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("second BindSocket: want ErrSocketInUse, got %v", err)
	}

	// The live owner can still accept a connection.
	doneCh := make(chan struct{})
	go func() {
		conn, _ := first.Accept()
		if conn != nil {
			conn.Close()
		}
		close(doneCh)
	}()
	c, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial after collision: %v", err)
	}
	c.Close()
	<-doneCh
}

// TestKillReleasesSocketPathBeforeResponding pins the contract that
// the /restart handler in gmuxd relies on: once POST /kill returns
// 204, the canonical socket path is free for a replacement runner
// to bind. Without this, the daemon's launchGmux can race the
// old runner's lingering listener and the user sees a sibling
// session in the sidebar.
//
// The runner's listener is still alive on the same inode at this
// point (existing SSE / WS connections need to drain so the daemon
// receives the exit event); only the path is unlinked. That's
// exactly what BindSocket needs to succeed: a new file at the same
// path, with the old listener orphaned but functional on its own
// inode.
func TestKillReleasesSocketPathBeforeResponding(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "kill.sock")
	srv, err := New(Config{
		Command:    []string{"bash", "-c", "sleep 30"}, // long-running so /kill has work to do
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	// POST /kill over the runner's own socket. If the handler
	// honours the contract, the response arrives only after the
	// path is gone.
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	resp, err := httpClient.Post("http://x/kill", "", nil)
	if err != nil {
		t.Fatalf("POST /kill: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket path still exists after /kill returned 204: %v", err)
	}

	// And a fresh BindSocket on the same path must succeed:
	// path is gone, no live owner.
	ln, err := BindSocket(sockPath)
	if err != nil {
		t.Fatalf("BindSocket after /kill: %v", err)
	}
	ln.Close()
}
