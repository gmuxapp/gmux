package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gmuxapp/gmux/cli/gmux/internal/localterm"
	"github.com/gmuxapp/gmux/cli/gmux/internal/ptyserver"
	"nhooyr.io/websocket"
)

// cmdAttach implements `gmux --attach <id>`.
//
// Opens a WebSocket to gmuxd's /ws/{id} handler via the local Unix
// socket. gmuxd proxies to the session's own Unix socket for local
// sessions and over the network for peer sessions, so this one code
// path works regardless of where the session lives.
//
// The local terminal is put into raw mode via the same localterm
// helper that `gmux <cmd>` uses, so attach feels the same as an
// original launch: transparent I/O, SIGWINCH → resize, SIGHUP →
// detach (session keeps running), Ctrl-C goes to the child.
func cmdAttach(ref, host string) int {
	sess, err := resolveSession(ref, host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}

	// A dead session has no backend socket for gmuxd to dial, so the WS
	// upgrade would fail with a cryptic "backend unavailable". Surface the
	// real reason up front and point the user at the UI where they can
	// resume the session if the adapter supports it.
	if !sess.Alive {
		fmt.Fprintf(os.Stderr, "gmux: session %s is not running (open the UI to resume)\n", shortID(sess.ID))
		return 1
	}

	if !localterm.IsInteractive() {
		fmt.Fprintln(os.Stderr, "gmux: --attach requires an interactive terminal")
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Dial gmuxd's WS endpoint through its Unix socket. nhooyr's Dial
	// accepts any HTTPClient, so we reuse the unix-socket transport
	// that gmuxdClient uses for plain HTTP calls.
	dialHTTP := gmuxdClient()
	// A long-lived WS can exceed the default 5s timeout; clear it here.
	dialHTTP.Timeout = 0

	wsURL := "ws://gmuxd/ws/" + sess.ID
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: dialHTTP,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gmux: connect %s: %v\n", shortID(sess.ID), err)
		return 1
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	// The server may send a scrollback snapshot that exceeds the default
	// 32KiB read limit (especially for TUIs with lots of state); match
	// the browser client's limit.
	conn.SetReadLimit(10 * 1024 * 1024)

	// Wire the local tty to the WS.
	attach, err := localterm.New(localterm.Config{
		PTYWriter: wsBinaryWriter{ctx: ctx, conn: conn},
		ResizeFn: func(cols, rows uint16) {
			sendResize(ctx, conn, cols, rows)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gmux: attach: %v\n", err)
		return 1
	}
	defer attach.Detach()
	attach.Start()

	// Send an initial resize so the PTY adopts our viewport. Without
	// this the child TUI might keep the previous attach's dimensions.
	if cols, rows, err := localterm.TerminalSize(); err == nil {
		sendResize(ctx, conn, cols, rows)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Reader pump: forward server frames to the terminal. Binary frames
	// are PTY output; text frames are JSON control messages (we currently
	// ignore them — the browser client consumes status/title updates via
	// SSE, not WS).
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if typ == websocket.MessageBinary {
				attach.Write(data)
			}
		}
	}()

	select {
	case <-readErr:
		// Server closed or connection broke. Either the session exited
		// or gmuxd hung up. Either way, we're done.
	case <-attach.Done():
		// Local tty closed (stdin EOF). Detach cleanly; session keeps
		// running just like gmux <cmd> with SIGHUP.
	case sig := <-sigCh:
		if sig == syscall.SIGHUP {
			// Terminal closed. Detach; session stays alive.
		} else {
			// SIGINT/SIGTERM from outside raw mode (shouldn't happen
			// much because raw mode forwards them to the child via
			// WS input, but we still drop the WS cleanly).
		}
	}
	return 0
}

// wsBinaryWriter adapts a WebSocket connection to io.Writer by sending
// each Write as a binary frame. Used to feed local stdin into the
// remote PTY.
type wsBinaryWriter struct {
	ctx  context.Context
	conn *websocket.Conn
}

func (w wsBinaryWriter) Write(p []byte) (int, error) {
	if err := w.conn.Write(w.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// sendResize delivers a resize message over the WS in the same JSON
// shape the ptyserver accepts. Source "local_tty" distinguishes it
// from browser resizes in server-side logging.
func sendResize(ctx context.Context, conn *websocket.Conn, cols, rows uint16) {
	msg := ptyserver.ResizeMsg{
		Type:   "resize",
		Cols:   cols,
		Rows:   rows,
		Source: "local_tty",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, data)
}

