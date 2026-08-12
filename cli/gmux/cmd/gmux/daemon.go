package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/sessionenv"
)

// ensureGmuxd checks if gmuxd is reachable and starts it if not.
// If a daemon is running but reports a different version, it is replaced
// so the child process always talks to a compatible daemon.
// Called once at startup — if gmuxd dies later, we don't restart it.
// Returns true if gmuxd was started (or replaced) by this call.
func ensureGmuxd() bool { return ensureGmuxdContext(context.Background()) }

func ensureGmuxdContext(ctx context.Context) bool {
	if !gmuxdNeedsStartContext(ctx) || ctx.Err() != nil {
		return false
	}

	// Autostart is a cross-process decision. Serialize it, then jitter and
	// recheck: the process that lost the first race should observe the winner's
	// socket instead of spawning a second full bootstrap.
	lock, err := acquireAutostartLock(ctx)
	if err != nil {
		return false
	}
	releaseHere := true
	defer func() {
		if releaseHere {
			releaseAutostartLock(lock)
		}
	}()
	jitter := 25*time.Millisecond + time.Duration(time.Now().UnixNano()%int64(75*time.Millisecond))
	select {
	case <-ctx.Done():
		return false
	case <-time.After(jitter):
	}
	if !gmuxdNeedsStartContext(ctx) || ctx.Err() != nil {
		return false
	}

	gmuxdBin := findGmuxdBin()
	if gmuxdBin == "" {
		log.Printf("warning: gmuxd not found (install it alongside gmux or add it to PATH)")
		return false
	}

	// gmuxd run starts in the foreground; we background it ourselves. Keep the
	// lock until that exact child either exposes a socket or exits. If the
	// caller's request budget ends first, a watcher retains the lock so another
	// CLI cannot overlap the still-bootstrapping child.
	started, done := startGmuxd(gmuxdBin, []string{"run"})
	if !started {
		return false
	}
	if !waitForAutostartCandidate(ctx, done, 30*time.Second) && ctx.Err() != nil {
		releaseHere = false
		go func() {
			waitForAutostartCandidate(context.Background(), done, 30*time.Second)
			releaseAutostartLock(lock)
		}()
	}
	return true
}

// gmuxdNeedsStart checks socket ownership first and health identity second.
// Response latency is never evidence that the socket has no owner.
func gmuxdNeedsStart() bool { return gmuxdNeedsStartContext(context.Background()) }

func gmuxdNeedsStartContext(ctx context.Context) bool {
	// Socket ownership is liveness. A completed connect proves an owner even if
	// its HTTP handler is overloaded; timeout and permission failures are
	// ambiguous and therefore conservative. Only ENOENT/ECONNREFUSED start.
	if !gmuxdSocketOwnedContext(ctx) {
		return true
	}

	// "dev" builds never replace — avoids churn during development and needs
	// no identity response once socket ownership is established.
	if version == "dev" {
		return false
	}

	resp, err := gmuxdHealthGetContext(ctx)
	if err != nil {
		return false // connected owner, identity unavailable: leave it alone
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false // responder is alive even if it reports unhealthy
	}

	var health struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(body, &health) != nil || strings.TrimSpace(health.Data.Version) == "" {
		return false // identity contract unavailable: leave the owner alone
	}

	// Same version: no action needed. Different known version: replace.
	return health.Data.Version != version
}

// findGmuxdBin locates the gmuxd binary: sibling first, then PATH.
func findGmuxdBin() string {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "gmuxd")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("gmuxd"); err == nil {
		return p
	}
	return ""
}

// autostartLogFile opens the daemon log the autostarted child will inherit as
// its stderr.
//
// The path is paths.DaemonLogPath -- the same file `gmuxd start` uses, the
// same file `gmuxd log-path` prints, and the same file the daemon bounds. It
// used to be $TMPDIR/gmuxd.log, which split the daemon's diagnostics in two by
// launcher: the CLI-autostarted daemon (the overwhelmingly common one) wrote
// somewhere `gmuxd log-path` never mentions, and, because the daemon only
// bounds a log it can confirm is its own, that copy grew without limit.
//
// Append, never truncate: this runs before the child has checked for a healthy
// incumbent, and most autostarts bounce straight off one.
func autostartLogFile() *os.File {
	logPath := paths.DaemonLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		log.Printf("warning: cannot create the state dir for %s: %v", logPath, err)
		return nil
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("warning: cannot open %s: %v", logPath, err)
		return nil
	}
	return f
}

// startGmuxd launches gmuxd in the background with the given args and returns
// a channel closed when that exact child exits.
func startGmuxd(gmuxdBin string, args []string) (bool, <-chan struct{}) {
	// Log gmuxd output to a file so users can diagnose startup failures.
	logFile := autostartLogFile()

	cmd := exec.Command(gmuxdBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = nil
	cmd.Stderr = logFile
	// Strip gmux session-identity vars. ensureGmuxd often fires from a
	// process already inside a session (e.g. a nested `gmux foo`), whose
	// env carries GMUX_SESSION_ID/GMUX_SOCKET/GMUX_ADAPTER for *that*
	// session. Without this the auto-started daemon would inherit them
	// and stamp the stale identity onto every session it later launches.
	// GMUX_SOCKET_DIR is preserved so the daemon scans the same socket
	// directory as the runner that triggered the auto-start. See
	// packages/sessionenv.
	cmd.Env = sessionenv.Strip(os.Environ())
	if err := cmd.Start(); err != nil {
		log.Printf("warning: could not start gmuxd: %v", err)
		if logFile != nil {
			logFile.Close()
		}
		return false, nil
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
		close(done)
	}()

	return true, done
}

func acquireAutostartLock(ctx context.Context) (*os.File, error) {
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.StateDir(), "gmuxd.autostart.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func releaseAutostartLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// waitForAutostartCandidate returns false only when the caller context ended.
// Child exit, socket appearance, and the bootstrap ceiling all resolve this
// candidate and permit the next lock holder to make a fresh decision.
func waitForAutostartCandidate(ctx context.Context, done <-chan struct{}, ceiling time.Duration) bool {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(ceiling)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return true
		case <-deadline.C:
			return true
		case <-tick.C:
			probeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			owned := gmuxdSocketOwnedContext(probeCtx)
			cancel()
			if owned {
				return true
			}
		}
	}
}

// gmuxdClient returns an HTTP client connected to gmuxd via Unix socket.
func gmuxdClient() *http.Client {
	sockPath := paths.SocketPath()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// gmuxdBaseURL returns the base URL for gmuxd HTTP requests.
// The host is ignored by the Unix socket transport.
func gmuxdBaseURL() string {
	return "http://localhost"
}

func gmuxdSocketOwnedContext(ctx context.Context) bool {
	dialCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", paths.SocketPath())
	if err == nil {
		_ = conn.Close()
		return true
	}
	return !(errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED))
}

// gmuxdHealthGetContext fetches identity with a generous deadline separate
// from the connect-only liveness probe. Callers own resp.Body.
func gmuxdHealthGetContext(ctx context.Context) (*http.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := gmuxdClient()
	client.Timeout = 15 * time.Second
	req, _ := http.NewRequestWithContext(attemptCtx, http.MethodGet, gmuxdBaseURL()+"/v1/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// registerOutcome classifies the result of a registration attempt so
// callers can react differently to "gmuxd isn't ready yet" versus
// "gmuxd will never accept this id."
type registerOutcome int

const (
	// registerOK: gmuxd accepted the registration (HTTP 200).
	registerOK registerOutcome = iota
	// registerUnavailable: every attempt failed transiently — connection
	// refused, timeout, or a 5xx while gmuxd was still starting. Retrying
	// later (or a discovery-scan pickup) may still succeed, so the runner
	// keeps serving.
	registerUnavailable
	// registerFatal: gmuxd answered with a client error (4xx) — a
	// permanent rejection this id can never recover from, e.g. a
	// malformed session id that fails the daemon's IsValidSessionID
	// guard. Retrying is pointless; a headless runner in this state is
	// an orphan and should exit rather than serve a session gmuxd will
	// never track. See run.go's fatal-registration shutdown.
	registerFatal
	// registerIDConflict is the typed registration-time race backstop. It is
	// too late to change the running child's identity; detached launch fails
	// while foreground keeps serving unregistered.
	registerIDConflict
)

func (o registerOutcome) ok() bool { return o == registerOK }

// registerWithGmuxd posts the session's registration to gmuxd and
// reports the outcome. Transient failures (gmuxd still starting) are
// retried until the caller's context ends; a 4xx is returned immediately
// without burning the retry budget. Callers that care about the
// outcome (the detached (-d) handshake, the orphan-reap path) branch
// on the returned registerOutcome.
func registerWithGmuxd(ctx context.Context, sessionID, socketPath, activeSubagentReservation string) registerOutcome {
	return registerWithClient(ctx, gmuxdClient(), sessionID, socketPath, activeSubagentReservation, 500*time.Millisecond)
}

func registerWithClient(ctx context.Context, client *http.Client, sessionID, socketPath, activeSubagentReservation string, backoff time.Duration) registerOutcome {
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID, "socket_path": socketPath, "active_subagent_reservation": activeSubagentReservation})
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gmuxdBaseURL()+"/v1/register", bytes.NewReader(payload))
		if err != nil {
			return registerFatal
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			switch {
			case status == http.StatusOK:
				return registerOK
			case status == http.StatusConflict:
				return registerIDConflict
			case status >= 400 && status < 500:
				return registerFatal
			case status < 500 || status >= 600:
				return registerFatal // redirects, informational, and non-HTTP protocol statuses
			}
		}
		if ctx.Err() != nil {
			return registerUnavailable
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return registerUnavailable
		case <-timer.C:
		}
	}
}

type sessionIDAvailability int

const (
	sessionIDAvailable sessionIDAvailability = iota
	sessionIDExists
	sessionIDCheckUnavailable
)

// checkSessionIDAvailability performs the read-only, non-reserving durable-ID
// preflight. Unreachable daemons are reported separately so foreground launch
// can preserve daemon-optional behavior.
func checkSessionIDAvailability(ctx context.Context, sessionID string) sessionIDAvailability {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gmuxdBaseURL()+"/v1/session-ids/"+sessionID, nil)
	if err != nil {
		return sessionIDCheckUnavailable
	}
	resp, err := gmuxdClient().Do(req)
	if err != nil {
		return sessionIDCheckUnavailable
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return sessionIDAvailable
	case http.StatusConflict:
		return sessionIDExists
	default:
		return sessionIDCheckUnavailable
	}
}

func deregisterFromGmuxd(sessionID string) {
	baseURL := gmuxdBaseURL()

	payload, _ := json.Marshal(map[string]string{"session_id": sessionID})
	client := gmuxdClient()
	resp, err := client.Post(baseURL+"/v1/deregister", "application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// parseHealthField extracts a string field from the data object
// of a /v1/health JSON response.
func parseHealthField(body []byte, field string) string {
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	raw, ok := resp.Data[field]
	if !ok {
		return ""
	}
	var val string
	if json.Unmarshal(raw, &val) != nil {
		return ""
	}
	return val
}

// parseTailscaleURL extracts the tailscale_url from a /v1/health JSON response.
func parseTailscaleURL(body []byte) string {
	var resp struct {
		Data struct {
			TailscaleURL string `json:"tailscale_url"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &resp) == nil {
		return resp.Data.TailscaleURL
	}
	return ""
}

// parseUpdateAvailable extracts update_available from a /v1/health JSON response.
func parseUpdateAvailable(body []byte) string {
	var resp struct {
		Data struct {
			UpdateAvailable string `json:"update_available"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &resp) == nil {
		return resp.Data.UpdateAvailable
	}
	return ""
}
