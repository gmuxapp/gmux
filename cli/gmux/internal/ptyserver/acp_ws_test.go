package ptyserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/acp"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"nhooyr.io/websocket"
)

// TestACPWebSocketSnapshotThenStream is an end-to-end transport test: a client
// attaching to /acp gets the session/load snapshot (durable JSONL history)
// first, then live session/update deltas ingested afterward (ADR 0021 §3).
func TestACPWebSocketSnapshotThenStream(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	convFile := filepath.Join(dir, "conv.jsonl")
	if err := os.WriteFile(convFile, []byte(
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hi there"}]}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	st := session.New(session.Config{ID: "s1", Adapter: "pi", SocketPath: sockPath})
	st.SetConversationFile(convFile)
	srv, err := New(Config{
		Command:    []string{node, "-e", "setTimeout(()=>{},5000)"},
		Cwd:        dir,
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		Adapter:    adapters.NewPi(),
		State:      st,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", sockPath)
	}}}
	conn, _, err := websocket.Dial(ctx, "ws://unix/acp", &websocket.DialOptions{HTTPClient: dialer})
	if err != nil {
		t.Fatalf("dial /acp: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// First frame: session/load with the JSONL history.
	load := readNote(t, ctx, conn)
	if load.Method != acp.MethodSessionLoad {
		t.Fatalf("first frame method = %q, want %q", load.Method, acp.MethodSessionLoad)
	}
	var lp acp.LoadParams
	if err := json.Unmarshal(load.Params, &lp); err != nil {
		t.Fatal(err)
	}
	if len(lp.Messages) != 1 || lp.Messages[0].Content[0].Text != "hi there" {
		t.Fatalf("snapshot messages = %+v", lp.Messages)
	}

	// Now ingest a live delta and expect a session/update frame.
	srv.acp.ingest(acpIngest{Op: "message_start", MessageID: "m1"})
	srv.acp.ingest(acpIngest{Op: "chunk", MessageID: "m1", Delta: "yo"})

	upd := readNote(t, ctx, conn)
	if upd.Method != acp.MethodSessionUpdate {
		t.Fatalf("live frame method = %q, want %q", upd.Method, acp.MethodSessionUpdate)
	}
	var up acp.UpdateParams
	if err := json.Unmarshal(upd.Params, &up); err != nil {
		t.Fatal(err)
	}
	if up.Update.Content.Text != "yo" {
		t.Fatalf("live delta = %q", up.Update.Content.Text)
	}
}

func readNote(t *testing.T, ctx context.Context, conn *websocket.Conn) acp.Notification {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var n acp.Notification
	if err := json.Unmarshal(data, &n); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return n
}
