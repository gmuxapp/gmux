package ptyserver

// incarnation_test.go — the runner half of endpoint identity.
//
// A socket pathname identifies a *location*, not a process. Two things follow,
// and this file pins both:
//
//   - Every response says which runner produced it, so a daemon that makes two
//     calls to one pathname can tell whether both reached the same process.
//   - /kill acts only when the caller can name this runner, so a decision made
//     about one process can never be executed against its successor.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmux/internal/session"
)

// runnerClient dials a runner's own socket.
func runnerClient(sockPath string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
}

func startRunner(t *testing.T, sockPath string, command ...string) *Server {
	t.Helper()
	srv, err := New(Config{
		Command:    command,
		Cwd:        "/tmp",
		Listener:   mustBindSocket(t, sockPath),
		SocketPath: sockPath,
		State:      session.New(session.Config{ID: "1c54cqk8", Command: command, Cwd: "/tmp", Adapter: "shell"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// Mutation: return a constant (or the empty string) from newIncarnation.
func TestIncarnationIsUniquePerRunner(t *testing.T) {
	dir := t.TempDir()
	a := startRunner(t, filepath.Join(dir, "a.sock"), "bash", "-c", "sleep 30")
	b := startRunner(t, filepath.Join(dir, "b.sock"), "bash", "-c", "sleep 30")

	if a.Incarnation() == "" || b.Incarnation() == "" {
		t.Fatal("a runner started without an incarnation")
	}
	if a.Incarnation() == b.Incarnation() {
		t.Fatal("two runners minted the same incarnation")
	}
}

// Mutation: drop the withIncarnation middleware, or the /meta splice.
//
// Both surfaces matter: the daemon reads the header when it subscribes (the
// only response it gets for a stream that never ends) and the JSON field when
// it fetches metadata. Disagreement between them would make the whole
// same-runner comparison meaningless.
func TestIncarnationIsReportedOnEveryResponseAndInMeta(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := startRunner(t, sockPath, "bash", "-c", "sleep 30")
	client := runnerClient(sockPath)

	metaResp, err := client.Get("http://x/meta")
	if err != nil {
		t.Fatalf("GET /meta: %v", err)
	}
	defer metaResp.Body.Close()
	if got := metaResp.Header.Get(IncarnationHeader); got != srv.Incarnation() {
		t.Errorf("/meta header = %q, want %q", got, srv.Incarnation())
	}
	var meta map[string]any
	if err := json.NewDecoder(metaResp.Body).Decode(&meta); err != nil {
		t.Fatalf("decode /meta: %v", err)
	}
	if got, _ := meta["incarnation"].(string); got != srv.Incarnation() {
		t.Errorf("/meta body incarnation = %q, want %q", got, srv.Incarnation())
	}
	// The splice must not disturb the document it splices into.
	if _, ok := meta["id"]; !ok {
		t.Error("/meta lost its existing fields")
	}
	// A PTY runner IS terminal mode (ADR 0033): the drive mode is spliced
	// beside the incarnation so registration can persist the axis.
	if got, _ := meta["drive_mode"].(string); got != "terminal" {
		t.Errorf("/meta drive_mode = %q, want terminal", got)
	}

	// The stream reports it too, in headers, before any event is sent.
	req, _ := http.NewRequest(http.MethodGet, "http://x/events", nil)
	eventsResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	if got := eventsResp.Header.Get(IncarnationHeader); got != srv.Incarnation() {
		t.Errorf("/events header = %q, want %q", got, srv.Incarnation())
	}
}

// Mutation: drop the ExpectIncarnationHeader check in handleKill.
//
// This is the runner half of the reaper's safety: the daemon classifies one
// process as an orphan, but can only address it by a pathname another process
// may own by the time the request lands. The process that answers is the one
// that checks.
func TestKillRefusesAnIncarnationThatIsNotOurs(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := startRunner(t, sockPath, "bash", "-c", "trap '' HUP; sleep 30")
	client := runnerClient(sockPath)

	req, _ := http.NewRequest(http.MethodPost, "http://x/kill", nil)
	req.Header.Set(ExpectIncarnationHeader, "some-other-runners-incarnation")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /kill: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 Conflict", resp.StatusCode)
	}

	// The child is untouched and the runner is still serving.
	if err := syscall.Kill(srv.cmd.Process.Pid, 0); err != nil {
		t.Fatalf("the child was killed by a /kill meant for another runner: %v", err)
	}
	select {
	case <-srv.Done():
		t.Fatal("the runner exited on a /kill meant for another runner")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := net.DialTimeout("unix", sockPath, time.Second); err != nil {
		t.Fatalf("the runner stopped serving: %v", err)
	}
}

// The positive case, and the compatibility case: a caller that names this
// runner is obeyed, and so is a caller that names nobody (an explicit user
// stop, or any client that predates the header).
func TestKillAcceptsOurOwnIncarnationAndAnAbsentExpectation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value func(srv *Server) string
	}{
		{"names this runner", func(srv *Server) string { return srv.Incarnation() }},
		{"names nobody", func(*Server) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sockPath := filepath.Join(t.TempDir(), "sess.sock")
			srv := startRunner(t, sockPath, "bash", "-c", "sleep 30")

			req, _ := http.NewRequest(http.MethodPost, "http://x/kill", nil)
			if v := tc.value(srv); v != "" {
				req.Header.Set(ExpectIncarnationHeader, v)
			}
			resp, err := runnerClient(sockPath).Do(req)
			if err != nil {
				t.Fatalf("POST /kill: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			select {
			case <-srv.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the child was not killed")
			}
		})
	}
}

// A runner that could not mint an incarnation must not become un-killable:
// the empty incarnation means "unidentifiable", and a caller that names a
// specific runner is still refused.
func TestKillRefusesWhenThisRunnerHasNoIncarnation(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := startRunner(t, sockPath, "bash", "-c", "trap '' HUP; sleep 30")
	srv.incarnation = "" // as if the CSPRNG had failed at startup

	req, _ := http.NewRequest(http.MethodPost, "http://x/kill", nil)
	req.Header.Set(ExpectIncarnationHeader, "anything")
	resp, err := runnerClient(sockPath).Do(req)
	if err != nil {
		t.Fatalf("POST /kill: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 Conflict", resp.StatusCode)
	}
	if err := syscall.Kill(srv.cmd.Process.Pid, 0); err != nil {
		t.Fatalf("an unidentifiable runner obeyed a targeted kill: %v", err)
	}
}

// Mutation: serve /reap from handleKill (i.e. make it unconditional), or drop
// the mandatory-expectation check.
//
// /reap exists as a separate route so that a runner which predates it cannot
// obey it. Its contract: no expectation is a client error, a mismatched
// expectation is a conflict, and only the named runner dies.
func TestReapRequiresAndVerifiesTheExpectedIncarnation(t *testing.T) {
	t.Run("no expectation", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "sess.sock")
		srv := startRunner(t, sockPath, "bash", "-c", "trap '' HUP; sleep 30")
		req, _ := http.NewRequest(http.MethodPost, "http://x/reap", nil)
		resp, err := runnerClient(sockPath).Do(req)
		if err != nil {
			t.Fatalf("POST /reap: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: an unconditional reap is not on offer", resp.StatusCode)
		}
		if err := syscall.Kill(srv.cmd.Process.Pid, 0); err != nil {
			t.Fatalf("the child died for an unconditional reap: %v", err)
		}
	})

	t.Run("another runner's incarnation", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "sess.sock")
		srv := startRunner(t, sockPath, "bash", "-c", "trap '' HUP; sleep 30")
		req, _ := http.NewRequest(http.MethodPost, "http://x/reap", nil)
		req.Header.Set(ExpectIncarnationHeader, "some-other-runner")
		resp, err := runnerClient(sockPath).Do(req)
		if err != nil {
			t.Fatalf("POST /reap: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if err := syscall.Kill(srv.cmd.Process.Pid, 0); err != nil {
			t.Fatalf("the child died for another runner's verdict: %v", err)
		}
	})

	t.Run("our own incarnation", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "sess.sock")
		srv := startRunner(t, sockPath, "bash", "-c", "sleep 30")
		req, _ := http.NewRequest(http.MethodPost, "http://x/reap", nil)
		req.Header.Set(ExpectIncarnationHeader, srv.Incarnation())
		resp, err := runnerClient(sockPath).Do(req)
		if err != nil {
			t.Fatalf("POST /reap: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		select {
		case <-srv.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the named runner did not terminate")
		}
		// Ownership is handed over, exactly as on the /kill path.
		if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the socket pathname survived a reap: %v", err)
		}
	})
}

// A runner that could not mint an incarnation is unidentifiable, so it can
// never be the named target of a reap.
func TestReapRefusesWhenThisRunnerHasNoIncarnation(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "sess.sock")
	srv := startRunner(t, sockPath, "bash", "-c", "trap '' HUP; sleep 30")
	srv.incarnation = ""

	req, _ := http.NewRequest(http.MethodPost, "http://x/reap", nil)
	req.Header.Set(ExpectIncarnationHeader, "anything")
	resp, err := runnerClient(sockPath).Do(req)
	if err != nil {
		t.Fatalf("POST /reap: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if err := syscall.Kill(srv.cmd.Process.Pid, 0); err != nil {
		t.Fatalf("an unidentifiable runner obeyed a targeted reap: %v", err)
	}
}

// The header names are a wire contract between two Go modules that cannot
// import each other: the daemon hardcodes the same literals (see
// services/gmuxd, runnerIncarnationHeader / expectIncarnationHeader, each
// pinned by its own test). Renaming a constant on either side is free; changing
// the literal breaks the protocol, so the literal is what is pinned.
func TestIncarnationHeaderNamesAreStable(t *testing.T) {
	if IncarnationHeader != "X-Gmux-Incarnation" {
		t.Errorf("IncarnationHeader = %q; the daemon reads X-Gmux-Incarnation", IncarnationHeader)
	}
	if ExpectIncarnationHeader != "X-Gmux-Expect-Incarnation" {
		t.Errorf("ExpectIncarnationHeader = %q; the daemon sends X-Gmux-Expect-Incarnation", ExpectIncarnationHeader)
	}
}
