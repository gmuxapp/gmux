// Package wskeepalive provides a WebSocket ping loop that keeps an
// otherwise-idle connection alive across NAT timeouts and promptly
// detects a dead peer.
//
// Why this exists: the PTY data plane (browser ↔ gmuxd) carries no
// application traffic while a terminal sits idle. Cellular carriers
// drop idle TCP flows behind their NAT after as little as 30–60s.
// Without a keepalive the browser only discovers the dead connection
// on its next write, then reconnects and the server re-ships the full
// multi-megabyte scrollback replay — a reconnect storm that burns
// mobile data and can OOM the mobile browser. A small periodic ping
// keeps the flow warm and surfaces a genuinely dead peer within one
// interval. See issue #241.
package wskeepalive

import (
	"context"
	"time"

	"nhooyr.io/websocket"
)

const (
	// DefaultInterval is how often we ping an idle connection. Chosen
	// below the ~30s low end of carrier NAT idle timeouts so the flow
	// never goes quiet long enough to be reaped.
	DefaultInterval = 20 * time.Second
	// DefaultTimeout bounds how long we wait for a pong before treating
	// the peer as dead.
	DefaultTimeout = 10 * time.Second
)

// Pinger is the subset of *websocket.Conn this package needs. It exists
// so the loop can be unit-tested without a real socket.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Run pings conn every interval until ctx is cancelled or a ping fails
// to be answered within timeout, whichever comes first. On a failed
// ping it invokes onDead once and returns; the caller wires onDead to
// its connection-teardown (e.g. cancelling the proxy context) so the
// read/write goroutines unwind.
//
// Run MUST execute concurrently with a reader on the same connection:
// websocket.Conn.Ping waits for the pong to be read by a Reader call,
// so a connection with no active read loop would block every ping
// until timeout and be falsely declared dead. Both PTY proxy paths
// satisfy this — they always have a client→backend read loop running.
func Run(ctx context.Context, conn Pinger, interval, timeout time.Duration, onDead func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				// Distinguish a dead peer from normal shutdown: if the
				// parent ctx is already done, the error is just the
				// teardown racing the ping — not a NAT drop.
				if ctx.Err() != nil {
					return
				}
				onDead()
				return
			}
		}
	}
}

// Ensure *websocket.Conn satisfies Pinger at compile time.
var _ Pinger = (*websocket.Conn)(nil)
