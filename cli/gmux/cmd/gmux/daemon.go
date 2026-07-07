package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
func ensureGmuxd() bool {
	if !gmuxdNeedsStart() {
		return false
	}

	gmuxdBin := findGmuxdBin()
	if gmuxdBin == "" {
		log.Printf("warning: gmuxd not found (install it alongside gmux or add it to PATH)")
		return false
	}

	// gmuxd run starts in the foreground; we background it ourselves.
	return startGmuxd(gmuxdBin, []string{"run"})
}

// gmuxdNeedsStart checks the running daemon.
func gmuxdNeedsStart() bool {
	// "dev" builds never replace — avoids churn during development.
	if version == "dev" {
		return !gmuxdHealthy(500 * time.Millisecond)
	}

	client := gmuxdClient()
	client.Timeout = 500 * time.Millisecond
	resp, err := client.Get(gmuxdBaseURL() + "/v1/health")
	if err != nil {
		return true // not running
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true // not healthy
	}

	var health struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(body, &health) != nil {
		return false // can't parse, leave it alone
	}

	// Same version: no action needed. Different version: replace.
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

// startGmuxd launches gmuxd in the background with the given args.
func startGmuxd(gmuxdBin string, args []string) bool {
	// Log gmuxd output to a file so users can diagnose startup failures.
	logPath := filepath.Join(os.TempDir(), "gmuxd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logFile = nil
	}

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
		return false
	}
	go func() {
		cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
	}()

	return true
}

// gmuxdClient returns an HTTP client connected to gmuxd via Unix socket.
func gmuxdClient() *http.Client {
	sockPath := paths.SocketPath()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 2*time.Second)
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

func gmuxdHealthy(timeout time.Duration) bool {
	client := gmuxdClient()
	client.Timeout = timeout
	resp, err := client.Get(gmuxdBaseURL() + "/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
)

func (o registerOutcome) ok() bool { return o == registerOK }

// registerWithGmuxd posts the session's registration to gmuxd and
// reports the outcome. Transient failures (gmuxd still starting) are
// retried a handful of times; a 4xx is fatal and returned immediately
// without burning the retry budget. Callers that care about the
// outcome (the detached (-d) handshake, the orphan-reap path) branch
// on the returned registerOutcome.
func registerWithGmuxd(sessionID, socketPath string) registerOutcome {
	baseURL := gmuxdBaseURL()

	payload, _ := json.Marshal(map[string]string{
		"session_id":  sessionID,
		"socket_path": socketPath,
	})

	// Retry a few times — gmux may start before the HTTP server is ready
	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		client := gmuxdClient()
		resp, err := client.Post(baseURL+"/v1/register", "application/json", bytes.NewReader(payload))
		if err != nil {
			continue
		}
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusOK {
			return registerOK
		}
		// A 4xx is a permanent verdict on this id — the daemon
		// understood the request and refused it (invalid session id,
		// malformed body). No amount of retrying changes the answer, so
		// short-circuit instead of wasting the budget. 5xx (and any
		// other status) stays in the transient bucket: gmuxd may still
		// be coming up.
		if status >= 400 && status < 500 {
			return registerFatal
		}
	}
	return registerUnavailable
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
