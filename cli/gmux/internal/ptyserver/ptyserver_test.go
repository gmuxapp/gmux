package ptyserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	// Drain any initial prompt output
	time.Sleep(100 * time.Millisecond)

	// Send input
	err = conn.Write(ctx, websocket.MessageBinary, []byte("hello\n"))
	if err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Read output until we see "got:hello" or timeout
	var got []byte
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			goto check
		default:
		}
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			continue
		}
		got = append(got, data...)
		if contains(got, "got:hello") {
			break
		}
	}
check:
	if !contains(got, "got:hello") {
		t.Errorf("expected 'got:hello' in output, got: %q", string(got))
	}

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
