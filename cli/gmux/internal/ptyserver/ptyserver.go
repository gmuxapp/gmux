// Package ptyserver allocates a PTY, execs a command, and serves
// a WebSocket endpoint on a Unix socket. Replaces abduco + ttyd.
package ptyserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"github.com/gmuxapp/gmux/cli/gmux/internal/agentext"
	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/socklease"
	"nhooyr.io/websocket"
)

// maxScrollback is the number of lines kept in the virtual terminal's
// scrollback buffer. Lines older than this are discarded.
const maxScrollback = 2000

// ErrSocketInUse is returned by BindSocket when the requested socket
// path is already owned by a live listener. Callers should pick a
// different session id and retry. See ADR 0003 "Collision handling".
var ErrSocketInUse = errors.New("socket path already in use by a live runner")

// BoundSocket is the result of BindSocket. It implements net.Listener and
// additionally carries the socket ownership lease (see packages/socklease)
// plus the physical identity of the socket file it created.
//
// Ownership rule, enforced by every removal path in this package: the
// canonical pathname may only be unlinked while the lease is held and only
// while it still names the very socket this process bound. Once ownership is
// released the pathname belongs to whoever leases it next, and this process
// must never touch it again -- its listener stays alive on the (now unnamed)
// inode so in-flight SSE/WebSocket connections can drain.
type BoundSocket struct {
	net.Listener
	lease *socklease.Lease
	// pin holds the socket file this process bound, so ownership can be given
	// up by asking "is this still my file?" rather than "does the pathname
	// still look like it did?". Inode numbers are recycled -- on some
	// filesystems immediately -- so the second question has a wrong answer
	// available to it, and the wrong answer unlinks a live replacement.
	pin          *socklease.Pin
	sockPath     string
	releaseOnce  sync.Once
	releaseError error
}

// LockPath returns the path of the lease's lock file.
func (b *BoundSocket) LockPath() string { return socklease.LockPath(b.sockPath) }

// ReleaseOwnership unlinks the canonical pathname -- but only while it still
// names the socket this BoundSocket bound -- and then drops the lease. It is
// idempotent and safe to call from several goroutines.
//
// It deliberately does not close the listener: after ownership is released the
// listener lives on as an unnamed socket so already-established connections
// (notably the daemon's exit-event subscription) can drain, while a
// replacement runner is free to lease and bind the pathname immediately.
func (b *BoundSocket) ReleaseOwnership() error {
	if b == nil {
		return nil
	}
	b.releaseOnce.Do(func() {
		rmErr := b.lease.RemoveSocket(b.pin)
		b.releaseError = errors.Join(rmErr, b.pin.Close(), b.lease.Release())
	})
	return b.releaseError
}

// leaseWaitBudget bounds how long BindSocket waits for a lease somebody else
// holds. It exists for the daemon's stale-socket reaper, which holds the lease
// for the length of one inspection; without the wait, a runner that starts
// inside that window falls back to a fresh session id and a restart loses its
// identity to a bookkeeping sweep.
const leaseWaitBudget = 750 * time.Millisecond

// bindAttempts bounds the bind/clean-up/retry loop. Each extra iteration
// requires another actor to have taken the pathname in the microseconds
// between our unlink and our listen.
const bindAttempts = 8

// bindBarrierHook is a barrier hook for the mixed-version bind tests: it runs at
// each named phase of BindSocket so a test can drive a pre-lease runner into the
// exact window it wants. It is unset in production.
//
// It carries the socket pathname, and is read under a mutex, for the same
// reasons the daemon's reaper barrier does: binds happen on several goroutines
// (and in several tests) at once, so a hook that fired for any pathname would be
// driven by whichever bind reached the phase first, and a plain variable would be
// a data race between one test's install and another's read. The hook is copied
// out before it is called, so a barrier that blocks never holds the lock.
var bindBarrierHook struct {
	mu sync.Mutex
	fn func(sockPath, phase string)
}

func bindBarrier(sockPath, phase string) {
	bindBarrierHook.mu.Lock()
	fn := bindBarrierHook.fn
	bindBarrierHook.mu.Unlock()
	if fn != nil {
		fn(sockPath, phase)
	}
}

// setBindBarrier installs the barrier hook, replacing any previous one.
func setBindBarrier(fn func(sockPath, phase string)) {
	bindBarrierHook.mu.Lock()
	bindBarrierHook.fn = fn
	bindBarrierHook.mu.Unlock()
}

// BindSocket creates and listens on a Unix socket at sockPath under the socket
// ownership lease.
//
// The lease makes stale-socket cleanup safe *between lease-aware processes*:
// holding it excludes every other runner and the daemon's reaper, so the
// pathname cannot change owner while we work on it. It says nothing at all
// about a runner from before the protocol, which holds no lease and is
// excluded by nothing. That asymmetry decides the whole algorithm:
//
//	bind first, and only clean up after a generation we can prove was
//	lease-aware.
//
// Concretely:
//
//  1. Take the lease (waiting briefly if the reaper holds it).
//  2. Try to listen *without unlinking anything*. A free pathname binds here,
//     which is the overwhelmingly common case.
//  3. EADDRINUSE means something occupies the pathname. If we had to create
//     the lock file, no lease-aware generation ever owned this pathname: its
//     occupant is an unleased runner, possibly one that is mid-startup right
//     now, and unlinking it would disconnect a live listener. Refuse with
//     ErrSocketInUse and let the caller pick a different id (ADR 0003).
//  4. If the lock file already existed, a lease-aware generation owned this
//     pathname and is provably not running (we hold its lease). Probe anyway
//     -- an unleased runner may have taken the pathname over since -- and only
//     if nothing answers, unlink the exact inode we probed and try again.
//
// What this buys, and it is the property the earlier probe-then-unlink version
// could not claim: no pathname belonging to a live pre-lease runner is ever
// unlinked, at any interleaving.
func BindSocket(sockPath string) (*BoundSocket, error) {
	// The lease protocol assumes a directory only this user can write in; if
	// that is not true, nothing below proves anything (see socklease's threat
	// model). Establish it here, where the directory is created.
	if err := socklease.RequireOwnedDir(filepath.Dir(sockPath)); err != nil {
		return nil, fmt.Errorf("BindSocket: %w", err)
	}
	lease, err := socklease.AcquireWait(sockPath, leaseWaitBudget)
	if errors.Is(err, socklease.ErrHeld) {
		return nil, ErrSocketInUse
	}
	if err != nil {
		return nil, fmt.Errorf("BindSocket: lease: %w", err)
	}
	return bindSocketWithLease(sockPath, lease)
}

// BindSocketWithInheritedLease binds using a lease descriptor transferred by
// the spawning daemon. Ownership is continuous across exec, so child startup
// never races a wall-clock lease acquisition timeout against its parent.
func BindSocketWithInheritedLease(sockPath string, leaseFile *os.File) (*BoundSocket, error) {
	if err := socklease.RequireOwnedDir(filepath.Dir(sockPath)); err != nil {
		return nil, fmt.Errorf("BindSocket: %w", err)
	}
	lease, err := socklease.AdoptInherited(sockPath, leaseFile)
	if err != nil {
		return nil, fmt.Errorf("BindSocket: inherited lease: %w", err)
	}
	return bindSocketWithLease(sockPath, lease)
}

func bindSocketWithLease(sockPath string, lease *socklease.Lease) (*BoundSocket, error) {
	bindBarrier(sockPath, "lease-held")

	for range bindAttempts {
		ln, listenErr := listenUnixOwnerOnly(sockPath)
		if listenErr == nil {
			// Pin the socket we just bound, and hold the pin for as long as we
			// own the pathname: it is what lets Shutdown unlink our own socket
			// and nobody else's, even if the number is handed out again.
			pin, pinErr := lease.PinSocket()
			if pinErr != nil {
				// The pathname we just bound is not a socket any more.
				// Something outside the protocol is rewriting the directory;
				// refuse to own a pathname we cannot identify rather than
				// unlink it later on a guess.
				_ = ln.Close()
				releaseAfterFailedBind(lease, false)
				return nil, fmt.Errorf("BindSocket: %s vanished or is not a socket right after listen: %w", sockPath, pinErr)
			}
			return &BoundSocket{Listener: ln, lease: lease, pin: pin, sockPath: sockPath}, nil
		}
		if !errors.Is(listenErr, syscall.EADDRINUSE) {
			releaseAfterFailedBind(lease, false)
			return nil, fmt.Errorf("BindSocket: listen: %w", listenErr)
		}
		bindBarrier(sockPath, "address-in-use")

		if lease.CreatedLockFile() {
			// No lease-aware generation ever owned this pathname, so its
			// occupant is outside the protocol. Never unlink it.
			releaseAfterFailedBind(lease, true)
			return nil, ErrSocketInUse
		}

		// A lease-aware generation owned this pathname and is gone. Confirm
		// nothing answers before touching it: an unleased runner may have
		// taken the pathname over in the meantime, and it is not ours to
		// remove.
		// Pin the occupant *before* probing it, so the removal below can be
		// conditioned on the file we probed rather than on a description of it.
		// A pre-lease runner may replace the pathname between the two, and on a
		// filesystem that recycles inodes its socket can carry the very same
		// device and inode -- which is how an identity check came to unlink a
		// live runner.
		stale, pinErr := socklease.PinSocket(sockPath)
		if pinErr != nil {
			// Whatever occupies the pathname now, it is not the socket the
			// lease-aware generation left: that history is stale.
			releaseAfterFailedBind(lease, true)
			return nil, fmt.Errorf("BindSocket: %s is occupied by something that is not a socket: %w", sockPath, pinErr)
		}
		bindBarrier(sockPath, "before-probe")
		// Only an explicit refusal authorises the removal below. A timeout, a
		// permission error, or a successful connect all mean the occupant is
		// not provably gone -- and a timeout in particular is a live owner
		// with a full backlog, not a dead one. This is the same predicate the
		// daemon's reaper uses, deliberately: an asymmetry here is how a live
		// socket gets unlinked by one side and spared by the other.
		//
		// Probed through the pin rather than the pathname: pin-then-probe is
		// the load-bearing order, and passing the pin is what makes the other
		// order unexpressible (socklease.ProbeRefusedPinned).
		probeErr := probeRefusedUnderPin(sockPath, stale)
		if probeErr != nil {
			// A live occupant with a free lease is an unleased runner that
			// took this pathname over, so the lease history describes a dead
			// generation rather than the occupant: drop it. Anything else
			// taught us nothing, so keep it.
			_ = stale.Close()
			releaseAfterFailedBind(lease, errors.Is(probeErr, socklease.ErrSocketLive))
			return nil, ErrSocketInUse
		}
		bindBarrier(sockPath, "before-remove")
		// Identity-checked: if the pathname was rebound since the probe, the
		// removal is declined and the next iteration probes the newcomer
		// instead of unlinking it.
		rmErr := lease.RemoveSocket(stale)
		_ = stale.Close()
		if rmErr != nil && !errors.Is(rmErr, socklease.ErrIdentityChanged) {
			releaseAfterFailedBind(lease, false)
			return nil, fmt.Errorf("BindSocket: removing stale socket: %w", rmErr)
		}
	}
	releaseAfterFailedBind(lease, false)
	return nil, fmt.Errorf("BindSocket: %s was re-taken on every attempt", sockPath)
}

// releaseAfterFailedBind drops a lease taken by a bind that did not happen.
//
// The lock file is removed when this attempt created it -- leaving it would
// invent a lease-aware history for a pathname that never had one -- or when
// the attempt proved the current occupant is not lease-aware, which makes any
// existing history false. Otherwise the lock file is left exactly as found:
// erasing it would make a genuinely reclaimable leftover permanently
// unreclaimable.
func releaseAfterFailedBind(lease *socklease.Lease, occupantProvenUnleased bool) {
	if lease.CreatedLockFile() || occupantProvenUnleased {
		_ = lease.Release()
		return
	}
	_ = lease.ReleaseKeepingLockFile()
}

// listenUnixOwnerOnly binds sockPath with an owner-only mode and without Go's
// automatic unlink-on-close: in this package every removal of a canonical
// pathname is explicit, leased and identity-checked.
func listenUnixOwnerOnly(sockPath string) (net.Listener, error) {
	oldUmask := syscall.Umask(0o077)
	ln, err := net.Listen("unix", sockPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	return ln, nil
}

// probeRefusedUnderPin probes the leftover the pin holds.
//
// The pin is a parameter, not something this function takes for itself, because
// pin-then-probe is the load-bearing order and a signature guards an order
// better than a comment does: there is nothing to pass if the pin has not been
// taken yet.
//
// The barrier fires here, at the end of the probe step, rather than at the call
// site. That placement is deliberate -- it is the only point a caller cannot get
// in front of. A caller that pinned *after* probing would have its pin land
// after this line, so a test can hand the pathname to a live unleased runner
// here and watch such a caller pin, and then unlink, the newcomer.
func probeRefusedUnderPin(sockPath string, pin *socklease.Pin) error {
	err := socklease.ProbeRefusedPinned(pin, probeTimeout)
	bindBarrier(sockPath, "probed")
	return err
}

// probeTimeout bounds the connect used to decide whether a leftover pathname
// is abandoned. It runs while the lease is held, so it also bounds how long a
// replacement runner can be kept waiting.
const probeTimeout = 250 * time.Millisecond

// newScreen builds the replay emulator and starts the DSR drain goroutine. It
// returns the emulator and a channel that is closed when the drain goroutine
// exits, so shutdown can join it (see stopScreenDrain).
func newScreen(cols, rows int, cursorCb func(visible bool)) (*vt.Emulator, chan struct{}) {
	// Default to 80x24 when launched non-interactively (no terminal).
	// The first resize from a connecting client will set the real size.
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	e := vt.NewEmulator(cols, rows)
	e.SetScrollbackSize(maxScrollback)
	e.SetCallbacks(vt.Callbacks{
		CursorVisibility: cursorCb,
	})
	// The emulator writes responses (e.g. DSR cursor position reports)
	// to an internal pipe. If nothing reads them, Write blocks. We don't
	// need the responses, so drain them in the background. The goroutine
	// exits when the pipe is closed during shutdown (see stopScreenDrain),
	// at which point drainDone is closed.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		_, _ = io.Copy(io.Discard, e)
	}()
	return e, drainDone
}

// stopScreenDrain unblocks and joins the DSR drain goroutine, then closes the
// emulator.
//
// We must NOT call e.Close() while the drain goroutine's io.Copy is still
// calling e.Read(): vt.Emulator guards its `closed` flag with no
// synchronization, so a concurrent Read (drain) and Close (shutdown) is a data
// race (github.com/charmbracelet/x/vt, still present as of the latest version).
//
// Instead we close the emulator's pipe directly via InputPipe(), which uses
// only io.Pipe's concurrency-safe methods. That returns EOF from the pending
// e.Read(), so the drain goroutine's io.Copy returns and drainDone closes. Once
// the drain goroutine has exited, no Read is in flight and e.Close() is safe.
func stopScreenDrain(e *vt.Emulator, drainDone chan struct{}) {
	if c, ok := e.InputPipe().(io.Closer); ok {
		_ = c.Close()
	} else {
		// Fallback: no closable pipe exposed. Close() races the drain, but
		// it's the only lever left. Should not happen with current vt.
		_ = e.Close()
	}
	<-drainDone
	_ = e.Close()
}

// renderScreen produces the ANSI snapshot: scrollback lines followed by
// the visible screen. The scrollback gives reconnecting clients context
// (previous output they can scroll up to). The visible screen is rendered
// row-by-row via CellAt (not Render()) because the emulator's internal
// buffer can grow beyond the declared height. Rows are joined with \r\n
// since bare \n wouldn't return the cursor to column 0.
func renderScreen(e *vt.Emulator) string {
	var sb strings.Builder

	// Scrollback: lines that scrolled off the top of the screen.
	if scrollback := e.Scrollback(); scrollback != nil {
		for _, line := range scrollback.Lines() {
			sb.WriteString(line.Render())
			sb.WriteString("\r\n")
		}
	}

	// Visible screen.
	w, h := e.Width(), e.Height()
	for y := 0; y < h; y++ {
		if y > 0 {
			sb.WriteString("\r\n")
		}
		line := make(uv.Line, w)
		for x := 0; x < w; x++ {
			if c := e.CellAt(x, y); c != nil {
				line[x] = *c
			}
		}
		sb.WriteString(line.Render())
	}
	return sb.String()
}

// ResizeMsg is the JSON message clients send to resize the terminal.
type ResizeMsg struct {
	Type        string `json:"type"`
	Cols        uint16 `json:"cols"`
	Rows        uint16 `json:"rows"`
	PixelWidth  uint16 `json:"pixelWidth,omitempty"`
	PixelHeight uint16 `json:"pixelHeight,omitempty"`
	// Source identifies who triggered the resize: "local_tty" or "web_client".
	Source string `json:"source,omitempty"`
}

// Server holds a PTY and serves WebSocket connections.
type Server struct {
	cmd      *exec.Cmd
	ptmx     *os.File
	sockPath string
	listener net.Listener
	// bound carries socket pathname ownership (lease + bound identity). It is
	// nil when a test injected a bare listener; such a server owns no
	// pathname and must never unlink one.
	bound           *BoundSocket
	screen          *vt.Emulator  // virtual terminal for replay snapshots (guarded by mu)
	screenDrainDone chan struct{} // closed when the DSR drain goroutine exits
	state           *session.State
	// promptMarks derives Status from OSC 133 prompt marks for every
	// session whose adapter is not hook-driven (adapter.HookDriven — the
	// agent adapters own their turn state via hooks; everything else gets
	// the runner's default turn model). Nil for hook-driven sessions. Fed
	// exclusively from readPTY's flush, so it needs no locking.
	promptMarks *adapter.PromptMarkTracker

	// adapter is the session's adapter, retained for the semantic action
	// layer (actions.go): POST /prompt and /cancel ask it to encode an
	// AgentAction and for its readiness deadline.
	adapter adapter.Adapter

	// Semantic-action state (ADR 0027), all runner-generation-local and
	// never persisted or projected as session status. See actions.go.
	//
	// readyCh is closed once the agent reports {"op":"ready"}; readyOnce
	// makes repeated ready events free and keeps the close race-free.
	readyCh   chan struct{}
	readyOnce sync.Once
	// deliverSlot is a capacity-1, context-aware semaphore serializing
	// semantic deliveries against each other — and nothing else. Raw /input
	// never takes it, and neither does any status writer. A channel rather
	// than a Mutex so a queued request can be abandoned by its caller with a
	// guarantee that it wrote nothing.
	deliverSlot chan struct{}
	// deliverBytes is the semantic layer's transport: the ONE place a
	// semantic action's bytes leave the runner. Production is WritePTY;
	// tests substitute a recorder, which is what makes "exactly one ordered
	// write of exactly these bytes" and "zero bytes on a refusal" assertable
	// rather than inferred from a child's echo.
	deliverBytes func(p []byte) (int, error)
	// barrier, when set by a test, runs while a semantic delivery holds the
	// delivery slot and nothing has been decided yet, so a schedule can be
	// driven deterministically (e.g. land a status transition before the
	// commit). Unset in production, and installed before any request is served.
	barrier func()

	// incarnation is this runner's ephemeral identity: a random value minted
	// once per process, exposed on every HTTP response and in /meta, and
	// required as proof by /kill. See newIncarnation.
	incarnation string

	shutdownOnce sync.Once

	mu             sync.Mutex
	clients        map[*wsClient]struct{}
	localOut       io.Writer      // optional local terminal output sink
	scrollback     io.WriteCloser // optional persistent scrollback sink (closed in waitChild)
	ptyCols        uint16         // last applied PTY cols (guarded by mu)
	ptyRows        uint16         // last applied PTY rows (guarded by mu)
	cursorHidden   bool           // tracks DECTCEM via callback (guarded by mu)
	ptmxClosed     bool           // true once ptmx is closed by Shutdown (guarded by mu)
	screenPending  []byte         // raw PTY data not yet fed to screen (guarded by mu)
	lastClientLeft time.Time      // when the last WS client disconnected (guarded by mu)

	acp *acpHub // ACP conversation stream synthesizer (ADR 0021)

	done    chan struct{} // closed when child exits
	ptyDone chan struct{} // closed when readPTY finishes draining
	err     error         // child exit error
}

type wsClient struct {
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	readonly bool
	writeMu  sync.Mutex
}

func (c *wsClient) write(typ websocket.MessageType, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(c.ctx, typ, data)
}

// Config for creating a new PTY server.
type Config struct {
	Command           []string
	CommandWrapper    []string // internal argv prefix applied after adapter command extension
	CommandWrapperEnv []string // private env consumed and unset by the internal wrapper before exec
	Cwd               string
	Env               []string
	ExtraFiles        []*os.File // inherited control fds for internal command wrappers
	// Listener is the pre-bound Unix socket the server serves
	// HTTP/WebSocket on. Required. Callers obtain one via
	// BindSocket so they can react to ErrSocketInUse (e.g.,
	// regenerate the session id) before any sessionID-dependent
	// setup runs. The server takes ownership: Close is called on
	// shutdown.
	Listener   net.Listener
	SocketPath string
	Cols       uint16
	Rows       uint16
	Adapter    adapter.Adapter
	State      *session.State
	// Version is reported to children via TERM_PROGRAM_VERSION.
	// Defaults to "dev" when empty.
	Version string
	// LocalOut, if non-nil, receives a copy of every PTY output chunk
	// from the moment the server starts reading. Set this at construction
	// time (rather than calling SetLocalOutput after New) when you need
	// to guarantee that fast-exiting commands can't race the wiring and
	// have their output dropped on the floor.
	LocalOut io.Writer
	// Scrollback, if non-nil, receives a copy of every PTY output
	// chunk for persistence to disk. Wired the same way as LocalOut
	// so fast-exiting commands can't lose output. The server takes
	// ownership: Close is called once after the final PTY drain in
	// waitChild, so callers must not Close it themselves and must
	// not pass a writer they need to keep using.
	Scrollback io.WriteCloser
}

// agentHookDisabled reports whether the user opted out of injecting the gmux
// agent hook via GMUX_NO_AGENT_HOOK — an escape hatch for when an agent release
// breaks the extension (e.g. pi changes its extension API and the hook fails to
// load, which could otherwise stop the agent from starting). With the hook off,
// the agent runs unmodified; gmux just loses hook-driven title/status/
// attribution for it. Any value other than "" or "0" disables.
//
// Read in the runner process, so it covers foreground (`gmux -- pi`) and
// detached (`-d`) launches; for daemon-initiated launches (resume/restart/UI)
// the var must be present in the daemon's environment.
func agentHookDisabled() bool {
	v := os.Getenv("GMUX_NO_AGENT_HOOK")
	return v != "" && v != "0"
}

// New creates and starts a PTY server.
func New(cfg Config) (*Server, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("no command specified")
	}
	if cfg.Cols == 0 {
		cfg.Cols = 80
	}
	if cfg.Rows == 0 {
		cfg.Rows = 25
	}

	if cfg.Listener == nil {
		return nil, fmt.Errorf("ptyserver.New: cfg.Listener is required (use BindSocket)")
	}
	listener := cfg.Listener

	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cfg.Cwd
	cmd.Env = buildChildEnv(os.Environ(), cfg.Env, cfg.Version)
	cmd.ExtraFiles = cfg.ExtraFiles

	// Inject the gmux session hook for adapters with a native extension/hook
	// API: it reports session/title/status authoritatively over POST
	// /hook/event (ADR 0011; tool-neutral protocol in
	// docs/runner-hook-protocol.md). Two seams by how the agent loads hooks:
	// SessionExtender splices a loader flag (pi: -e <materialized ext>);
	// SessionHookCommand injects a command hook via the agent's config-override
	// flags with the gmux binary as the hook program (codex: -c hooks.X=...).
	// Both are ephemeral, scoped to this launch, and no-op without
	// GMUX_SESSION_SOCK. GMUX_NO_AGENT_HOOK opts out.
	if agentHookDisabled() {
		_, isExt := cfg.Adapter.(adapter.SessionExtender)
		_, isHC := cfg.Adapter.(adapter.SessionHookCommand)
		if isExt || isHC {
			log.Printf("ptyserver: GMUX_NO_AGENT_HOOK set; launching %s without the gmux hook (no hook-driven title/status/attribution)", cfg.Adapter.Name())
		}
	} else if ext, ok := cfg.Adapter.(adapter.SessionExtender); ok {
		// Argv injection (pi): splice the gmux extension into the launch argv.
		if extPath, err := agentext.Path(); err != nil {
			log.Printf("ptyserver: cannot materialize %s session hook: %v", cfg.Adapter.Name(), err)
		} else if extended := ext.ExtendCommand(cmd.Args, extPath); len(extended) > len(cmd.Args) {
			cmd.Args = extended
			cmd.Env = append(cmd.Env, "GMUX_SESSION_SOCK="+cfg.SocketPath)
		}
	} else if hc, ok := cfg.Adapter.(adapter.SessionHookCommand); ok {
		// Config injection (codex): inject a command hook on the launch argv via
		// the agent's config-override flags. The hook program is the gmux binary
		// itself (`gmux __codex-hook <Event>`), so the runner passes its own path.
		if self, err := os.Executable(); err != nil {
			log.Printf("ptyserver: cannot resolve gmux binary for %s hook: %v", cfg.Adapter.Name(), err)
		} else if extended, ok := hc.HookCommand(cmd.Args, self); ok {
			cmd.Args = extended
			cmd.Env = append(cmd.Env, "GMUX_SESSION_SOCK="+cfg.SocketPath)
		}
	}

	if len(cfg.CommandWrapper) > 0 {
		wrapped := append(append([]string{}, cfg.CommandWrapper...), cmd.Args...)
		wrappedEnv := append(cmd.Env, cfg.CommandWrapperEnv...)
		cmd = exec.Command(wrapped[0], wrapped[1:]...)
		cmd.Dir = cfg.Cwd
		cmd.Env = wrappedEnv
		cmd.ExtraFiles = cfg.ExtraFiles
	}

	// Start command in a new PTY
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cfg.Cols,
		Rows: cfg.Rows,
	})
	if err != nil {
		listener.Close()
		if bs, ok := listener.(*BoundSocket); ok {
			// Leased removal: the pathname is only unlinked while it still
			// names the socket we bound.
			if relErr := bs.ReleaseOwnership(); relErr != nil {
				log.Printf("ptyserver: releasing %s after failed pty start: %v", cfg.SocketPath, relErr)
			}
		}
		return nil, fmt.Errorf("start pty: %w", err)
	}

	s := &Server{
		cmd:         cmd,
		ptmx:        ptmx,
		sockPath:    cfg.SocketPath,
		listener:    listener,
		screen:      nil, // set below after s is constructed
		state:       cfg.State,
		clients:     make(map[*wsClient]struct{}),
		localOut:    cfg.LocalOut,   // wired before readPTY starts so early output is never lost
		scrollback:  cfg.Scrollback, // same: wired pre-readPTY so fast-exit output is never lost
		ptyCols:     cfg.Cols,
		ptyRows:     cfg.Rows,
		done:        make(chan struct{}),
		ptyDone:     make(chan struct{}),
		incarnation: newIncarnation(),

		adapter:     cfg.Adapter,
		readyCh:     make(chan struct{}),
		deliverSlot: make(chan struct{}, 1),
	}
	s.deliverBytes = s.WritePTY
	// Retain the ownership handle from BindSocket. Tests may inject a bare
	// net.Listener; those servers own no pathname and never unlink one.
	if bs, ok := listener.(*BoundSocket); ok {
		s.bound = bs
	}
	// ACP conversation hub (ADR 0021): synthesizes session/load + session/update
	// from the read-only pi extension's token deltas. sessionID drives the
	// sessionId field on every ACP frame.
	if cfg.State != nil {
		s.acp = newACPHub(cfg.State.ID)
	}

	// The callback fires under s.mu (held during drainScreenLocked → screen.Write).
	s.screen, s.screenDrainDone = newScreen(int(cfg.Cols), int(cfg.Rows), func(visible bool) {
		s.cursorHidden = !visible
	})

	// Non-hook-driven sessions get their busy/idle Status derived from
	// OSC 133 prompt marks in the output stream: Active=true when a
	// command starts executing, false when the next prompt returns.
	// Each transition is a separate SetStatus call, so a fast command
	// whose marks land in a single PTY read still emits the full
	// active→idle pulse downstream — the daemon's send --wait requires
	// observing both edges (issue #373).
	//
	// A genuine active→idle transition is a completed turn, so it also
	// flags the session unread ("waiting on you") — the same policy
	// applyTurnEnd implements for agent turns. The first idle mark (the
	// shell's initial prompt) closes only the launch phase, not a
	// command the user is waiting on, so it does not set unread.
	if !adapter.HookDriven(cfg.Adapter) && cfg.State != nil {
		sawActive := false
		s.promptMarks = adapter.NewPromptMarkTracker(func(active bool) {
			s.state.SetStatus(&adapter.Status{Active: active})
			if active {
				sawActive = true
			} else if sawActive {
				s.state.SetUnread(true)
			}
		})
	}

	go s.readPTY()
	go s.waitChild()
	go s.processScreen()
	go s.serve()

	return s, nil
}

// LifetimeTurnOpen reports whether this session is still on the
// default lifetime-as-turn model (no prompt marks ever observed), so
// the process exit is what closes its single turn. The caller (run.go)
// emits the closing status/unread before SetExited. Only meaningful
// after the final PTY flush — read it after PTYDone.
func (s *Server) LifetimeTurnOpen() bool {
	return s.promptMarks != nil && !s.promptMarks.SawMark()
}

// drainScreenLocked feeds all pending raw PTY data to the virtual terminal
// emulator. This is the only place where screen.Write is called, ensuring the
// emulator stays off the hot path (readPTY flush). Caller must hold s.mu.
func (s *Server) drainScreenLocked() {
	if len(s.screenPending) == 0 {
		return
	}
	s.screen.Write(s.screenPending)
	s.screenPending = s.screenPending[:0]
}

// screenSyncInterval controls how often the background goroutine feeds
// pending PTY data to the virtual terminal emulator. Keeping this short
// bounds the amount of data that must be drained synchronously when a
// client connects (snapshot) or the scrollback text is requested.
const screenSyncInterval = 100 * time.Millisecond

// processScreen runs in a background goroutine, periodically draining
// screenPending into the vt.Emulator. This keeps the emulator roughly
// up-to-date without blocking the readPTY hot path.
func (s *Server) processScreen() {
	ticker := time.NewTicker(screenSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.drainScreenLocked()
			s.mu.Unlock()
		case <-s.ptyDone:
			// Final drain after PTY output is fully read.
			s.mu.Lock()
			s.drainScreenLocked()
			s.mu.Unlock()
			return
		}
	}
}

// Pid returns the child process PID.
func (s *Server) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// SocketPath returns the Unix socket path.
func (s *Server) SocketPath() string {
	return s.sockPath
}

// Done returns a channel that is closed when the child process exits.
// Note: when Done closes, the PTY readout may still have buffered
// output that hasn't been flushed to LocalOut / WS clients yet. Wait
// on PTYDone() as well if you need to see the child's final bytes.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// PTYDone returns a channel that is closed after the PTY has been fully
// drained, meaning all output the child ever produced has been flushed
// through LocalOut and to every WS client. Always closes strictly after
// Done(). Callers that want to detach a local terminal without dropping
// the child's trailing output should wait on this before detaching.
func (s *Server) PTYDone() <-chan struct{} {
	return s.ptyDone
}

// ExitCode returns the child process exit code (only valid after Done).
func (s *Server) ExitCode() int {
	if s.err == nil {
		return 0
	}
	if exitErr, ok := s.err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

// SetLocalOutput sets a writer that receives a copy of all PTY output.
// Used for transparent local terminal attach. Pass nil to detach.
//
// For the initial wiring, prefer Config.LocalOut: calling this after
// New leaves a race window in which a fast-exiting child's output can
// be flushed before the writer is attached and be silently dropped.
// SetLocalOutput is the right tool for *changing* the sink mid-session
// (e.g. detaching when stdin closes), not for the first attach.
func (s *Server) SetLocalOutput(w io.Writer) {
	s.mu.Lock()
	detaching := s.localOut != nil && w == nil
	s.localOut = w
	noViewers := detaching && len(s.clients) == 0
	s.mu.Unlock()

	if noViewers {
		s.shrinkForReconnect()
	}
}

// WritePTY writes raw bytes to the PTY input (as if typed by the user).
func (s *Server) WritePTY(p []byte) (int, error) {
	return s.ptmx.Write(p)
}

// Resize changes the PTY window size and signals the child.
// Called by the local terminal (localterm) on SIGWINCH — always tagged as local_tty.
func (s *Server) Resize(cols, rows uint16) {
	s.resize(ResizeMsg{Cols: cols, Rows: rows, Source: "local_tty"})
}

// releaseSocketOwnership gives up the canonical socket pathname: it unlinks
// the pathname while the lease still names this server's own socket, then
// releases the lease so a replacement runner can bind immediately.
//
// It is idempotent, and it is the *only* place in the runner that unlinks a
// canonical pathname. Both callers (the /kill handler, which releases early so
// a restart's replacement runner never has to wait, and Shutdown, which covers
// every other exit) may run concurrently.
func (s *Server) releaseSocketOwnership(reason string) {
	if s.bound == nil {
		return // test-injected listener: this server owns no pathname
	}
	if err := s.bound.ReleaseOwnership(); err != nil {
		log.Printf("ptyserver: %s: releasing socket %s: %v", reason, s.sockPath, err)
	}
}

// Shutdown releases socket ownership, then closes the listener and all
// connections.
//
// Ownership protocol:
//
//  1. Unlink the canonical pathname while holding the lease and only while it
//     still names this server's socket, then drop the lease. A concurrent
//     BindSocket cannot observe a half-released pathname: it either fails to
//     take the lease (pathname still ours) or takes it after the unlink.
//  2. Close the listener. Existing connections are unaffected by the unlink;
//     closing the listener only stops new ones.
//
// Ordering note (resume/restart safety): the daemon learns a session died from
// the runner's exit event, which run.go emits *before* it deregisters and
// calls Shutdown. A restart therefore spawns the replacement runner while this
// process is still finishing up -- which is exactly why /kill releases
// ownership before it responds instead of waiting for Shutdown.
func (s *Server) Shutdown() {
	// Idempotent: Shutdown can now be invoked from more than one
	// goroutine — a signal handler and the registration goroutine's
	// fatal-rejection reap can race — so the whole teardown runs once.
	s.shutdownOnce.Do(func() {
		s.releaseSocketOwnership("shutdown")
		// Close the listener. Existing connections are not affected.
		s.listener.Close()

		s.mu.Lock()
		// Close ptmx and mark it closed under mu. pty.Setsize (setPtySize) calls
		// (*os.File).Fd(), which races (*os.File).Close(); serializing both under
		// mu, plus the ptmxClosed guard, keeps resize/shrink from touching the fd
		// once Shutdown has closed it. (Read/Write go through the reference-counted
		// poll.FD and are already safe against Close.)
		s.ptmxClosed = true
		s.ptmx.Close()
		stopScreenDrain(s.screen, s.screenDrainDone) // unblocks + joins the DSR drain goroutine, then closes
		for c := range s.clients {
			c.cancel()
		}
		s.mu.Unlock()
	})
}

// setPtySize applies a window size to the PTY unless Shutdown has already closed
// it. pty.Setsize calls (*os.File).Fd(), which is not safe against a concurrent
// (*os.File).Close(); holding mu (which Shutdown also holds while closing) and
// skipping once closed serializes the two.
func (s *Server) setPtySize(ws *pty.Winsize) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmxClosed {
		return
	}
	_ = pty.Setsize(s.ptmx, ws)
}

// IncarnationHeader carries the runner's ephemeral incarnation on every
// response, so a client learns which runner answered without a second request.
// ExpectIncarnationHeader carries a client's requirement back: /kill acts only
// if the value names this runner.
const (
	IncarnationHeader       = "X-Gmux-Incarnation"
	ExpectIncarnationHeader = "X-Gmux-Expect-Incarnation"
)

// newIncarnation mints a runner's ephemeral identity.
//
// It exists because a socket pathname, and even a socket inode, cannot
// identify the process behind an endpoint. A pathname is reusable; an inode
// can be hard-linked, unlinked and restored. A daemon that talks to an
// endpoint twice -- Subscribe, then Meta -- has no filesystem evidence that
// both calls reached the same runner, and stat-bracketing the pair only proves
// the pathname looked the same before and after.
//
// The nonce closes that: it is minted once per runner process and returned
// with every response, so two calls that report the same incarnation provably
// reached the same runner. It is deliberately ephemeral -- never persisted,
// never reused across restarts -- because its whole job is to distinguish this
// process from its own successor at the same pathname.
//
// A failure to read the system CSPRNG degrades to the empty string, which
// every consumer treats as "unknown", exactly like a runner that predates the
// protocol: conservative, never a match.
func newIncarnation() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		log.Printf("ptyserver: cannot mint an incarnation (%v); the daemon will treat this runner as unidentifiable", err)
		return ""
	}
	return hex.EncodeToString(buf[:])
}

// Incarnation returns this runner's ephemeral identity.
func (s *Server) Incarnation() string { return s.incarnation }

// withIncarnation stamps every response with the runner's identity.
func (s *Server) withIncarnation(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.incarnation != "" {
			w.Header().Set(IncarnationHeader, s.incarnation)
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) serve() {
	mux := http.NewServeMux()

	// HTTP endpoints (checked first via explicit paths)
	mux.HandleFunc("GET /meta", s.handleMeta)
	mux.HandleFunc("POST /hook/event", s.handleHookEvent)
	mux.HandleFunc("POST /input", s.handleInput)
	// Semantic agent actions, permanently separate from raw /input (ADR 0027).
	mux.HandleFunc("POST /prompt", s.handlePrompt)
	mux.HandleFunc("POST /cancel", s.handleCancel)
	mux.HandleFunc("PUT /status", s.handlePutStatus)
	mux.HandleFunc("PUT /slug", s.handlePutSlug)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /kill", s.handleKill)
	mux.HandleFunc("POST /reap", s.handleReap)

	// ACP conversation stream (ADR 0021): one-way ingest from the read-only pi
	// extension, and the snapshot-then-stream WebSocket the frontend attaches to.
	mux.HandleFunc("POST /acp/ingest", s.handleACPIngest)
	mux.HandleFunc("/acp", s.handleACP)

	// WebSocket terminal attach (fallback for / with Upgrade header)
	mux.HandleFunc("/", s.handleWS)

	server := &http.Server{Handler: s.withIncarnation(mux)}
	server.Serve(s.listener)
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	data, err := s.state.MarshalJSON()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Splice the incarnation in rather than adding it to session.State: the
	// nonce belongs to this runner process, not to the session, and State is
	// the session's own document. Decoding to RawMessage keeps every existing
	// field byte-identical.
	if s.incarnation != "" {
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) == nil {
			obj["incarnation"], _ = json.Marshal(s.incarnation)
			if merged, mErr := json.Marshal(obj); mErr == nil {
				data = merged
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// screenText returns the visible screen content as plain text.
// Caller must hold s.mu.
func (s *Server) screenText() string {
	return s.screen.String()
}

// maxInputBytes caps the size of a single POST /input request body.
// The socket is owner-only, so this isn't a trust boundary — it just
// keeps a well-meaning `gmux --send` invocation from accidentally
// exhausting memory if someone pipes a huge file into it.
const maxInputBytes = 1 << 20 // 1 MiB

// handleInput writes the request body straight to the child PTY, as if
// the bytes had been typed at the terminal. Backs `gmux --send`.
//
// Access control is delegated to the Unix socket's file permissions
// (owner-only, 0o700): anyone who can connect() to this socket already
// owns the session and could do arbitrary worse things to it.
// hookEvent is the tool-neutral payload an agent's gmux hook posts to
// POST /hook/event. Any agent can target this endpoint; the runner makes no
// per-adapter assumptions. The agent reports facts about itself; the runner
// maps them to sidebar state:
//
//	op "ready"            — the agent can accept input (semantic actions)
//	op "session"          — the bound conversation ref, id, name (on bind)
//	op "turn" phase start   — the agent loop began (→ active), with identity,
//	                          capped user boundary, source bytes and history baseline
//	op "turn" phase iteration — one assistant response completed
//	op "turn" phase steered — an additional capped user boundary entered the loop,
//	                          with its original source-byte length
//	op "turn" phase end     — the turn settled: Outcome, Output, Truncated,
//	                          Diagnostic, title
//
// The turn events are result-bearing (ADR 0027, 2026-07-28 amendment): a
// result-bearing adapter asserts its own turn boundary and delivers the turn's
// outcome, final assistant prose and user-bounded activity span. The runner relays
// those facts in its turn frame (session.TurnFrame) and never reconstructs them
// from the conversation.
//
// Outcome is a stable, agent-agnostic vocabulary ("completed" | "interrupted" |
// "error"); each agent's hook normalizes its own terminal state into it, and
// the runner owns what each means for the sidebar (see applyTurnEnd). The full
// wire contract is documented in docs/runner-hook-protocol.md. pi's hook
// (pi-ext.mjs) is the reference implementation.
type hookEvent struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	Pid  int    `json:"pid"`

	ID      string `json:"id,omitempty"`   // adapter session id; informational (the runner keys on the gmux id)
	Slug    string `json:"slug,omitempty"` // slug source (runner slugifies); empty until the session has a title
	Name    string `json:"name,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Title   string `json:"title,omitempty"`
	Phase   string `json:"phase,omitempty"`   // "start" | "iteration" | "steered" | "end"
	Outcome string `json:"outcome,omitempty"` // "completed" | "interrupted" | "error"

	// Turn identity and asserted result (op "turn"). TurnSeq is the adapter's
	// monotonic turn counter binding start, additional boundaries and close; an
	// adapter that does not assert turn identity leaves it 0, and consumers
	// treat 0 as "unknown" and serve no result for it.
	TurnSeq           uint64 `json:"turn_seq,omitempty"`
	Trigger           string `json:"trigger,omitempty"`      // phase start: what began the turn
	Text              string `json:"text,omitempty"`         // phase steered: the injected message
	SourceBytes       int    `json:"source_bytes,omitempty"` // start/steered: original UTF-8 byte length
	PreviousExchanges *int   `json:"previous_exchanges,omitempty"`
	Output            string `json:"output,omitempty"` // phase end: the turn's final assistant prose
	// Truncated says the adapter capped terminal Output at the source.
	Truncated  bool   `json:"truncated,omitempty"`
	Diagnostic string `json:"diagnostic,omitempty"` // phase end: short reason for a non-completed close
}

// maxHookEventBytes caps one POST /hook/event body. It is sized for the
// worst-case ESCAPED settled event: the adapter caps `output` at 256 KiB
// pre-escape and JSON escaping can expand a byte six-fold, so the limit must
// leave that payload room to arrive intact. An oversized output must never cost
// the close (ADR 0027), and the adapter's own cap is what guarantees the event
// fits here.
const maxHookEventBytes = 4 << 20

// handleHookEvent applies the authoritative session state an agent's gmux hook
// reports: the bound conversation ref + title + slug on every bind, and
// busy/idle/unread/error on every agent-loop transition. There is no
// inference and no per-adapter branching — the agent tells us exactly what it
// holds and does, and the runner relays those facts (ADR 0011). State written
// here (SetConversationRef et al.) is a relay snapshot for /events replay, not
// derived or sticky.
func (s *Server) handleHookEvent(w http.ResponseWriter, r *http.Request) {
	var ev hookEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, maxHookEventBytes)).Decode(&ev); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch ev.Op {
	case "ready":
		// The agent can accept semantic input now (its composer/input
		// handlers are installed). Deliberately independent of any
		// conversation bind: a brand-new session has no conversation file
		// yet, and gating readiness on one would deadlock the first prompt.
		s.markReady()
	case "session":
		// Authoritative bind (e.g. pi's session_start): the file the
		// agent holds, named and slugged.
		// Snapshot the currently-bound conversation before SetConversationRef
		// overwrites it, so the slug logic below can tell a genuine re-bind
		// (different conversation) from a same-conversation refresh.
		priorRef := s.state.ConversationRefSnapshot()
		// A rebind to a DIFFERENT conversation invalidates the turn frame: the
		// previous conversation's answer cannot be attributed under the new ref.
		// Cleared before the ref and slug are published so a subscriber that sees
		// the new conversation can never still see the old result.
		if ev.Path != "" && ev.Path != priorRef {
			s.state.ClearConversationState()
		}
		if ev.Path != "" {
			s.state.SetConversationRef(ev.Path)
		}
		if ev.Name != "" {
			s.state.SetAdapterTitle(ev.Name)
		}
		// The slug is the session's URL identity. Prefer a real title-derived
		// source the agent reports; we NEVER synthesize one from ev.ID (the
		// adapter's session id, a UUID for every real adapter that slugifies
		// into an unreadable URL). With no source, leave the slug empty so the
		// web owns the fallback — the *gmux* session id itself (routing.ts
		// sessionPath), which the runner doesn't know.
		//
		// Clear only on a genuine re-bind to a *different* conversation (pi
		// re-binds through the same runner on switch/new/resume/fork): switching
		// from a titled conversation to a fresh untitled one must drop the old
		// slug. A same-conversation refresh (claude/codex re-send the bind on
		// every turn end) with a transiently empty source must NOT clear an
		// established slug — that would flap the URL on a parse hiccup.
		//
		// A genuine re-bind uses BindSlug (always emits): after a re-register
		// the daemon may hold a stale slug that diverges from this fresh runner
		// state, so a dedup'd SetSlug could leave the store stale. A refresh
		// uses SetSlug (dedup) — runner and store agree there.
		rebind := ev.Path != "" && ev.Path != priorRef
		switch {
		case ev.Slug != "":
			slug := adapter.Slugify(ev.Slug)
			if rebind {
				s.state.BindSlug(slug)
			} else {
				s.state.SetSlug(slug)
			}
		case rebind:
			s.state.BindSlug("")
		}
	case "turn":
		// Agent-loop transition. The extension reports phase + facts; the
		// sidebar policy (what an outcome means) lives here, in testable Go.
		switch ev.Phase {
		case "start":
			// One call, one critical section: the turn's identity and the active
			// edge are published together and in that order.
			s.state.OpenTurnSource(ev.TurnSeq, ev.Trigger, ev.SourceBytes, ev.PreviousExchanges)
		case "steered":
			s.state.NoteInjection(ev.TurnSeq, ev.Text, ev.SourceBytes)
		case "iteration":
			s.state.NoteIteration(ev.TurnSeq)
		default:
			s.applyTurnEnd(ev)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyTurnEnd maps a normalized turn outcome to sidebar state. This is the
// one place gmux decides what an agent's terminal state means:
//
//	completed   — the agent finished on its own; the reply is unread.
//	error       — the agent gave up (e.g. exhausted retries); show a red dot.
//	interrupted — a human or agent intentionally stopped the turn: idle,
//	              nothing unread, but recorded so a synchronous wait can
//	              report the stop instead of a completion or a failure.
//
// SetStatus replaces the status wholesale, so a terminal outcome clears the
// previous turn's interruption exactly like a "turn" start does.
//
// A terminal end only closes an OPEN turn (State.CloseTurnFrame, atomic under the
// state lock). The runner owns turn open/closed state, so an end against an
// already-closed turn is stale — and now that interruption is durable,
// applying it would rewrite a good closure. Claude is the concrete case: a
// clean turn is UserPromptSubmit → Stop → SessionEnd, and SessionEnd (which
// covers exiting mid-turn, where neither Stop nor StopFailure fires) would
// otherwise overwrite the Stop/StopFailure closure with an interruption on
// every normal session exit. The
// hook translator cannot dedupe it: each event is a fresh, stateless
// `gmux __claude-hook` process (ADR 0015), so the runner — the one component
// holding turn state — is the right boundary. Repeated ends become idempotent
// for every agent.
//
// LIMIT, stated explicitly: this is turn POLARITY, not turn IDENTITY. Without
// a turn token the runner cannot tell a logically stale end (end₁ arriving
// after start₂) from a legitimate end of turn 2, so such an end would close
// the new turn. Excluding that ordering is the sender's responsibility, and
// how completely it is excluded differs per agent:
//
//   - pi: guaranteed. The extension serializes hook delivery on one promise
//     chain, so request N+1 is never issued before N settles
//     (cli/gmux/internal/agentext/pi-ext.mjs).
//   - Claude: partial. `Stop` hooks are awaited, so a clean turn's end lands
//     before whatever follows. `StopFailure`'s output and exit code are
//     documented as ignored, so it is not evidently awaited: a session exiting
//     at almost the same moment could deliver `SessionEnd` first and record an
//     API-failed turn as interrupted instead of error. Narrow, accepted here.
//
// A turn token/generation is the durable fix for an agent that cannot
// guarantee ordering; it is deliberately not introduced here.
//
// The title refresh is not turn state and is applied either way: a late end
// carrying a fresher title (Claude's Stop refresh, pi's session name) should
// still land.
// The asserted result travels with the close as ONE event carrying both the
// settled frame and the terminal status (State.CloseTurnFrame), so no subscriber
// can observe the close without the result that closed it — not by reordering,
// and not by the lossy fan-out dropping one of two sends. An adapter that asserts
// no turn identity (Claude, Codex, a raw `PUT /status` child) leaves TurnSeq 0,
// which no waiter can match, so those closes are served result-free instead of
// with somebody else's answer.
func (s *Server) applyTurnEnd(ev hookEvent) {
	outcome, title := ev.Outcome, ev.Title
	if title != "" {
		s.state.SetAdapterTitle(title)
	}
	// One atomic check-and-close: concurrent hook POSTs are served on
	// independent goroutines, so a snapshot-then-set would race.
	closed := s.state.CloseTurnFrame(session.TurnClose{
		TurnSeq:    ev.TurnSeq,
		Outcome:    outcome,
		Output:     ev.Output,
		Truncated:  ev.Truncated,
		Diagnostic: ev.Diagnostic,
	}, &adapter.Status{
		Active:      false,
		Error:       outcome == "error",
		Interrupted: outcome == "interrupted",
	})
	if closed && outcome == "completed" {
		s.state.SetUnread(true)
	}
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInputBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.ptmx.Write(body); err != nil {
		http.Error(w, "write pty: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePutStatus is the generic child self-report channel (`PUT /status` on
// $GMUX_SOCKET). It deliberately does NOT go through the turn gate: it is a raw
// whole-status write, not a turn-lifecycle event, so a script can set any
// combination (including clearing to null) without gmux second-guessing it.
// The gate only constrains hook-reported terminal turn ends, whose stale
// duplicates gmux must ignore.
//
// It does, however, keep the turn frame honest: a raw write that closes an open
// turn abandons the frame's current record, so an idle session never advertises a
// turn that has ended (see State.SetStatusAbandoningTurn). It asserts no result,
// so the last-closed record is left alone and the close stays result-free.
func (s *Server) handlePutStatus(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// "null" clears the status
	if string(body) == "null" {
		s.state.SetStatusAbandoningTurn(nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var status adapter.Status
	if err := json.Unmarshal(body, &status); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.state.SetStatusAbandoningTurn(&status)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePutSlug(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	slug := string(body)
	if slug == "" {
		http.Error(w, "slug is empty", http.StatusBadRequest)
		return
	}
	// Session slugs flow into /@<peer>/<slug> URLs and the
	// ${peer}::${slug} folder key, so normalize to the same URL-safe
	// shape as project slugs: reject "/", "::", newlines, and other
	// separators by slugifying anything not already well-formed.
	if !adapter.IsValidSlug(slug) {
		slug = adapter.Slugify(slug)
	}
	if slug == "" {
		http.Error(w, "slug is invalid", http.StatusBadRequest)
		return
	}
	s.state.SetSlug(slug)
	w.WriteHeader(http.StatusNoContent)
}

// handleReap is termination conditional on identity, and it exists as a
// separate route from /kill for one reason: a runner that predates this
// protocol must be *unable* to obey it.
//
// The daemon's orphan reaper decides about one specific process and then has
// to address it by pathname. Pathnames are reusable, so the occupant at
// delivery time may be somebody else. Asking /kill and adding a header does
// not solve that in a mixed-version fleet: a pre-protocol runner ignores
// unknown headers and dies for a verdict passed on a different process. A
// route it does not serve, by contrast, answers 404 and does nothing at all.
// So the reaper only ever calls this route, and 404 is a safe, informative
// answer that no legacy runner can get wrong.
//
// The expectation is mandatory here -- an unconditional reap is not a thing
// this endpoint offers.
func (s *Server) handleReap(w http.ResponseWriter, r *http.Request) {
	want := r.Header.Get(ExpectIncarnationHeader)
	if want == "" {
		http.Error(w, "reap requires "+ExpectIncarnationHeader, http.StatusBadRequest)
		return
	}
	if want != s.incarnation {
		log.Printf("ptyserver: refusing a reap meant for incarnation %s; this runner is %s", want, s.incarnation)
		http.Error(w, "incarnation mismatch: this pathname is owned by a different runner", http.StatusConflict)
		return
	}
	s.terminateChild(w)
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	// /kill is the compatibility route: an explicit, user-initiated stop of
	// whoever owns this endpoint. It honours an expectation when one is given
	// -- a caller that knows which runner it means gets the stronger
	// behaviour -- but an absent header keeps the original semantics, which
	// `gmux kill` and every pre-protocol client rely on.
	//
	// A caller whose decision was made about one specific process earlier must
	// not use this route at all: see handleReap.
	if want := r.Header.Get(ExpectIncarnationHeader); want != "" && want != s.incarnation {
		log.Printf("ptyserver: refusing /kill meant for incarnation %s; this runner is %s", want, s.incarnation)
		http.Error(w, "incarnation mismatch: this pathname is owned by a different runner", http.StatusConflict)
		return
	}
	s.terminateChild(w)
}

// terminateChild is the shared body of /kill and /reap: signal the child, wait
// for it, hand the socket pathname over, and answer.
func (s *Server) terminateChild(w http.ResponseWriter) {
	if s.cmd.Process == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// SIGHUP matches the "terminal closed" semantics of this endpoint:
	// interactive shells (bash, zsh) ignore SIGTERM but exit cleanly on
	// SIGHUP; TUI adapters treat SIGHUP the same as a graceful shutdown.
	// Sent to the process group so children (e.g. a subshell's commands)
	// receive it too.
	pid := s.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGHUP)
	log.Printf("ptyserver: sent SIGHUP to child pid %d", pid)

	// Block until the child actually exits (or escalate). Dismiss/restart
	// callers rely on this: once this returns, gmuxd immediately removes
	// the session and expects the runner's socket path to be free.
	// Returning early while a shell (e.g. fish) ignores SIGHUP causes
	// the next discovery scan to re-register the dead session.
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		syscall.Kill(-pid, syscall.SIGKILL)
		log.Printf("ptyserver: escalated to SIGKILL for child pid %d", pid)
		<-s.done
	}

	// Release the canonical socket path — and the lease with it — before
	// responding. The runner process will linger briefly for
	// state.SetExited / deregister / scrollback close, and its listener stays
	// up on the (now unnamed) inode for the existing SSE/WS connections that
	// need to drain, notably the daemon's exit-event subscription.
	//
	// Releasing the lease here, rather than in Shutdown, is load-bearing: the
	// daemon's restart path observes death from the exit event this runner is
	// about to emit and spawns the replacement immediately. If the dying
	// runner still held the lease at that moment, the replacement's
	// BindSocket would fail with ErrSocketInUse and fall back to a fresh
	// session id (ADR 0003), silently breaking restart identity. Once
	// released, this process never touches the pathname again: Shutdown's
	// removal is idempotent and cannot unlink the replacement's socket.
	s.releaseSocketOwnership("terminate")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := s.state.Subscribe()
	defer s.state.Unsubscribe(ch)

	// Replay the session's turn state to this (possibly reconnecting) subscriber
	// so a restarted daemon re-learns it with no persisted state: any status
	// emitted before the subscription — the launch-time Active=true of the default
	// turn model, or an agent turn that started while the daemon was down — would
	// otherwise be invisible until the next transition, and the turn frame is what
	// lets a wait armed after the reconnect learn which turn is running and what
	// the last one answered.
	//
	// Both facts are taken from ONE snapshot and sent in ONE event, the same
	// coupled shape a live edge uses (session.ReplayTurnEdge). Two reads could
	// straddle a turn edge, and a replay is exactly where that costs something: a
	// wait armed in the reconnect window would bind turn_seq 0 and resolve
	// result-free. Sent before the conversation ref so a subscriber never sees a
	// ref newer than the frame that belongs to it.
	if typ, payload, ok := session.ReplayTurnEdge(s.state.TurnEdgeSnapshot()); ok {
		if data, err := json.Marshal(payload); err == nil {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
		}
	}

	if file := s.state.ConversationRefSnapshot(); file != "" {
		if data, err := json.Marshal(map[string]string{"path": file}); err == nil {
			fmt.Fprintf(w, "event: conversation_file\ndata: %s\n\n", data)
		}
		// Replay the slug too (the runner owns it, ADR 0011). Once a
		// conversation is bound the slug snapshot is authoritative for it,
		// including an *empty* slug (untitled) — which the daemon honors as an
		// explicit clear. Without this, a slug set/rename/clear emitted while
		// the daemon was down is never re-learned (SetSlug dedups, so it won't
		// re-emit) and the store keeps a stale value. Gated on a bound
		// conversation so a just-started runner (no bind yet) can't transiently
		// clear a good persisted slug before its session hook fires.
		if data, err := json.Marshal(map[string]string{"slug": s.state.SlugSnapshot()}); err == nil {
			fmt.Fprintf(w, "event: meta\ndata: %s\n\n", data)
		}
	}

	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(evt.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // local Unix socket, no origin check needed
	})
	if err != nil {
		log.Printf("ptyserver: ws accept: %v", err)
		return
	}
	conn.SetReadLimit(64 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	client := &wsClient{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
	}

	// Replay screen state, then register for live data.
	// All steps happen under s.mu so readPTY cannot send live data to
	// this client before the snapshot frame.
	//
	// Ordering guarantee: snapshot is always the first message the client
	// receives, followed by any live data from subsequent readPTY cycles.
	//
	// The screen state comes from a virtual terminal (charmbracelet/x/vt)
	// that processes every byte of PTY output. renderScreen serializes
	// the scrollback history followed by the visible screen as ANSI
	// sequences with style diffing.
	//
	// Sequence: BSU → reset → scrollback + screen → cursor → ESU
	s.mu.Lock()
	s.drainScreenLocked()
	snapshot := renderScreen(s.screen)
	cursorSeq := "\x1b[?25h" // show cursor (default)
	if s.cursorHidden {
		cursorSeq = "\x1b[?25l" // hide cursor
	}
	// Position cursor at the emulator's current location.
	pos := s.screen.CursorPosition()
	cursorPos := fmt.Sprintf("\x1b[%d;%dH", pos.Y+1, pos.X+1)
	bsu := "\x1b[?2026h"                     // Begin Synchronized Update
	resetSeq := "\x1b[r\x1b[H\x1b[2J\x1b[3J" // Reset scroll region + cursor home + erase display + erase scrollback
	esu := "\x1b[?2026l"                     // End Synchronized Update
	frame := []byte(bsu + resetSeq + snapshot + cursorPos + cursorSeq + esu)
	if err := client.write(websocket.MessageBinary, frame); err != nil {
		s.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		return
	}
	s.clients[client] = struct{}{}
	s.lastClientLeft = time.Time{} // reset: we have an active viewer
	s.mu.Unlock()

	// Client connected — they'll see the scrollback, so clear unread
	if s.state != nil {
		s.state.SetUnread(false)
	}

	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		noClients := len(s.clients) == 0 && s.localOut == nil
		if len(s.clients) == 0 {
			s.lastClientLeft = time.Now()
		}
		s.mu.Unlock()

		// When the last viewer disconnects, shrink the PTY by 1 column.
		// The next connecting client will send a resize with its real
		// viewport, which will differ from the shrunk size, naturally
		// triggering a SIGWINCH that forces the child TUI to do a full
		// redraw (including re-emitting kitty images). This avoids the
		// need for a visible wiggle on connect.
		if noClients {
			s.shrinkForReconnect()
		}
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
	}()

	// Read from WebSocket, write to PTY
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return // client disconnected
		}

		// Text frames might be resize messages
		if typ == websocket.MessageText {
			var msg ResizeMsg
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
				s.resize(msg)
				continue
			}
		}

		// Write input to PTY
		if _, err := s.ptmx.Write(data); err != nil {
			return
		}
	}
}

// shrinkForReconnect reduces the PTY width by 1 column so that the next
// connecting client's resize (which will carry the real viewport size)
// triggers a genuine dimension change. Most TUI frameworks only do a full
// re-render when width or height actually changes; without this, a client
// whose viewport matches the current PTY size would get a stale snapshot
// (missing kitty images, possible drift from the emulator's reconstruction).
//
// Called when the last viewer (WS client or local terminal) disconnects.
// The shrink happens while no one is watching, so there's no visible
// flicker. The child TUI redraws at cols-1, but nobody sees it.
//
// Safety: re-checks that no viewer has connected between the call-site
// check and the lock acquisition. Also skips if the child has exited
// (pointless to resize a dead process).
//
// State and resize broadcasts are intentionally skipped: the shrunk size
// is an internal detail, not a real terminal size change.
func (s *Server) shrinkForReconnect() {
	// Don't bother if the child has exited.
	select {
	case <-s.done:
		return
	default:
	}

	s.mu.Lock()
	if s.ptyCols <= 1 || s.ptyRows == 0 || len(s.clients) > 0 || s.localOut != nil {
		s.mu.Unlock()
		return
	}
	s.ptyCols--
	cols := s.ptyCols
	rows := s.ptyRows
	s.drainScreenLocked()
	s.screen.Resize(int(cols), int(rows))
	s.mu.Unlock()

	s.setPtySize(&pty.Winsize{Cols: cols, Rows: rows})
	if s.cmd.Process != nil {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGWINCH)
	}
}

func (s *Server) resize(msg ResizeMsg) {
	if msg.Cols == 0 || msg.Rows == 0 {
		return
	}

	// Check if the PTY size actually changed. Skipping redundant SIGWINCH
	// prevents TUI apps from redrawing their entire screen unnecessarily,
	// which is the main source of "rewrite the entire log" slowness on
	// reconnect or duplicate resize events.
	s.mu.Lock()
	sizeChanged := msg.Cols != s.ptyCols || msg.Rows != s.ptyRows
	if sizeChanged {
		s.ptyCols = msg.Cols
		s.ptyRows = msg.Rows
		// Drain pending data first so the emulator processes it at the
		// old size before switching to the new dimensions.
		s.drainScreenLocked()
		s.screen.Resize(int(msg.Cols), int(msg.Rows))
	}
	s.mu.Unlock()

	if sizeChanged {
		s.setPtySize(&pty.Winsize{
			Cols: msg.Cols,
			Rows: msg.Rows,
			X:    msg.PixelWidth,
			Y:    msg.PixelHeight,
		})

		// Send SIGWINCH to the child process group.
		if s.cmd.Process != nil {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGWINCH)
		}
	}

	// Always update state and broadcast so all clients stay in sync,
	// even if the PTY size didn't change (idempotent metadata update).
	if s.state != nil {
		s.state.SetTerminalSize(msg.Cols, msg.Rows)
	}

	// Broadcast terminal_resize to all connected WS clients so every browser
	// can update its xterm size and the proxy can update ownership/store.
	payload, err := json.Marshal(map[string]any{
		"type":   "terminal_resize",
		"cols":   msg.Cols,
		"rows":   msg.Rows,
		"source": msg.Source,
	})
	if err != nil {
		return
	}

	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		if err := c.write(websocket.MessageText, payload); err != nil {
			c.cancel()
		}
	}
}

// activityGrace is the time after the last WS client disconnects before
// activity events are emitted. Suppresses false positives during session
// switching when the old session briefly has zero clients.
const activityGrace = 500 * time.Millisecond

// coalesceMaxBytes is the maximum accumulated data before forcing a flush,
// even if the coalesce timer hasn't fired yet. Keeps latency bounded.
const coalesceMaxBytes = 32 * 1024

// coalesceInterval is how long readPTY waits for more data before flushing.
// Chosen to be below one 60 fps frame (~16 ms) so the browser can still
// render at full frame rate while dramatically reducing WS message count
// during bursts (e.g. TUI redraws after SIGWINCH).
const coalesceInterval = 8 * time.Millisecond

func (s *Server) readPTY() {
	defer close(s.ptyDone)

	buf := make([]byte, 32*1024)
	var accum []byte
	timer := time.NewTimer(coalesceInterval)
	timer.Stop()

	flush := func() {
		if len(accum) == 0 {
			return
		}
		data := accum
		accum = nil

		// Process adapter/title hooks on the accumulated chunk.
		if title := adapters.ParseOSCTitle(data); title != "" {
			s.state.SetShellTitle(title)
		}
		if s.promptMarks != nil {
			s.promptMarks.Feed(data)
		}

		// Queue data for the virtual terminal emulator (processed by
		// processScreen in the background). Snapshot the client list
		// atomically so new clients always see their replay frame first.
		s.mu.Lock()
		s.screenPending = append(s.screenPending, data...)
		localOut := s.localOut
		clients := make([]*wsClient, 0, len(s.clients))
		for c := range s.clients {
			clients = append(clients, c)
		}
		hasRemoteClients := len(clients) > 0
		lastLeft := s.lastClientLeft
		s.mu.Unlock()

		// Emit activity only when no client is viewing and the grace
		// period has elapsed. The grace period suppresses false positives
		// during session switching (brief disconnect window).
		if !hasRemoteClients && s.state != nil {
			if lastLeft.IsZero() || time.Since(lastLeft) > activityGrace {
				s.state.EmitActivity()
			}
		}

		if localOut != nil {
			localOut.Write(data)
		}
		if s.scrollback != nil {
			// Best-effort: scrollback Write contract is no-error,
			// IO failures are sticky and surfaced via Close.
			s.scrollback.Write(data)
		}

		for _, c := range clients {
			if err := c.write(websocket.MessageBinary, data); err != nil {
				c.cancel()
			}
		}
	}

	readCh := make(chan []byte, 4)
	readDone := make(chan error, 1)

	// Separate goroutine for blocking PTY reads so we can select on
	// both incoming data and the coalesce timer.
	go func() {
		for {
			n, err := s.ptmx.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				readCh <- chunk
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	}()

	for {
		select {
		case chunk := <-readCh:
			accum = append(accum, chunk...)
			if len(accum) >= coalesceMaxBytes {
				timer.Stop()
				flush()
			} else {
				// Reset the coalesce timer. On the first chunk this
				// starts the window; on subsequent chunks it extends it.
				timer.Reset(coalesceInterval)
			}

		case <-timer.C:
			flush()

		case <-readDone:
			timer.Stop()
			// Drain any remaining chunks that were queued before the
			// reader goroutine signaled completion.
		drain:
			for {
				select {
				case chunk := <-readCh:
					accum = append(accum, chunk...)
				default:
					break drain
				}
			}
			flush()
			return
		}
	}
}

func (s *Server) waitChild() {
	s.err = s.cmd.Wait()
	close(s.done)

	// Wait for readPTY to finish draining all buffered PTY output before
	// closing client connections. Without this, the coalesce buffer may
	// still hold the child's final output when we close the WebSocket,
	// causing data loss.
	<-s.ptyDone

	// Now that the final flush has run, close the persistent
	// scrollback sink. Any IO error from the lifetime of the
	// writer surfaces here — we log but don't fail, since the
	// child has already exited and the scrollback is best-effort.
	if s.scrollback != nil {
		if err := s.scrollback.Close(); err != nil {
			log.Printf("ptyserver: scrollback close: %v", err)
		}
		s.scrollback = nil
	}

	s.mu.Lock()
	for c := range s.clients {
		c.conn.Close(websocket.StatusNormalClosure, "process exited")
	}
	s.mu.Unlock()
}

// buildChildEnv composes the environment passed to PTY children.
//
// Layering, in order:
//  1. parent (typically os.Environ()) — inherits the daemon/user env;
//  2. caller-supplied extras (cfg.Env from the adapter / runner);
//  3. terminal capability advertisements that always win, because the
//     frontend's actual capabilities don't depend on what the parent
//     thinks: TERM_PROGRAM=gmux, TERM_PROGRAM_VERSION=<version>,
//     COLORTERM=truecolor, KITTY_WINDOW_ID=1 (xterm.js + image addon
//     handles kitty graphics, sixel, and iTerm2 images);
//  4. TERM=xterm-256color, but only if no earlier layer provided one.
//     When gmuxd is launched from a non-interactive context (systemd
//     unit, browser-launched shell inheriting the daemon's env) TERM
//     may be missing, which makes curses programs like lazygit abort
//     with "terminal entry not found: term not set". Defaulting matches
//     what the xterm.js frontend actually renders.
//
// version falls back to "dev" when empty so TERM_PROGRAM_VERSION is
// never a bare "=".
func buildChildEnv(parent, extra []string, version string) []string {
	if version == "" {
		version = "dev"
	}
	env := make([]string, 0, len(parent)+len(extra)+5)
	for _, layer := range [][]string{parent, extra} {
		for _, e := range layer {
			key := e
			if i := strings.IndexByte(e, '='); i >= 0 {
				key = e[:i]
			}
			if strings.HasPrefix(key, "_GMXINTERNAL_") {
				continue
			}
			env = append(env, e)
		}
	}
	env = append(env,
		"TERM_PROGRAM=gmux",
		"TERM_PROGRAM_VERSION="+version,
		"COLORTERM=truecolor",
		"KITTY_WINDOW_ID=1",
	)
	if !hasEnv(env, "TERM") {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// hasEnv reports whether env contains a NAME=... entry for the given name.
func hasEnv(env []string, name string) bool {
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
