package main

// This file is a black-box test of the built production daemon. It is only
// enabled by tools/production-e2e.sh, inside its networkless container.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

func TestProductionContainerE2E(t *testing.T) {
	if os.Getenv("GMUX_PRODUCTION_E2E") != "1" {
		t.Skip("container production E2E is opt-in")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("refusing production E2E outside Docker")
	}
	if os.Getenv("GMUX_E2E_CONTAINER_GUARD") != "isolated-v1" {
		t.Fatal("missing container isolation guard")
	}
	bin := os.Getenv("GMUXD_E2E_BINARY")
	if !filepath.IsAbs(bin) {
		t.Fatal("GMUXD_E2E_BINARY must be absolute")
	}
	t.Run("unread_restart_sse_sqlite", func(t *testing.T) { scenarioUnreadRestart(t, bin) })
	t.Run("daemon_down_runner_death", func(t *testing.T) { scenarioDaemonDown(t, bin) })
	t.Run("ownership_contention", func(t *testing.T) { scenarioContention(t, bin) })
	t.Run("backup_export_and_restart_stress", func(t *testing.T) { scenarioAdminStress(t, bin) })
}

type prodEnv struct {
	root, state, run, config, home string
	port                           int
}

func newProdEnv(t *testing.T) *prodEnv {
	t.Helper()
	root, err := os.MkdirTemp("/e2e", "p-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	e := &prodEnv{root: root, state: filepath.Join(root, "state"), run: filepath.Join(root, "run"), config: filepath.Join(root, "config"), home: filepath.Join(root, "home"), port: freePort(t)}
	for _, d := range []string{e.state, e.run, e.config, e.home} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(e.config, "gmux")
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("port = %d\n[discovery]\ndevcontainers = false\n[tailscale]\nenabled = false\n", e.port)
	if err := os.WriteFile(filepath.Join(cfg, "host.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return e
}
func (e *prodEnv) vars() []string {
	return append(os.Environ(), "HOME="+e.home, "XDG_STATE_HOME="+e.state, "XDG_CONFIG_HOME="+e.config, "XDG_RUNTIME_DIR="+e.run, "GMUX_SOCKET_DIR="+e.run)
}
func (e *prodEnv) socket() string   { return filepath.Join(e.state, "gmux", "gmuxd.sock") }
func (e *prodEnv) stateDir() string { return filepath.Join(e.state, "gmux") }

type daemonProc struct {
	cmd  *exec.Cmd
	done chan error
	log  *bytes.Buffer
}

func startDaemon(t *testing.T, bin string, e *prodEnv) *daemonProc {
	t.Helper()
	d := &daemonProc{done: make(chan error, 1), log: new(bytes.Buffer)}
	d.cmd = exec.Command(bin, "run")
	d.cmd.Env = e.vars()
	d.cmd.Stdout = d.log
	d.cmd.Stderr = d.log
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.done <- d.cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if unixipc.Healthy(e.socket()) {
			return d
		}
		select {
		case err := <-d.done:
			t.Fatalf("daemon exited before ready: %v\n%s", err, d.log.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.kill()
	t.Fatalf("daemon not ready: %s", d.log.String())
	return nil
}
func (d *daemonProc) kill() {
	if d != nil && d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
		select {
		case <-d.done:
		case <-time.After(5 * time.Second):
		}
	}
}
func stopDaemon(t *testing.T, d *daemonProc, e *prodEnv) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/shutdown", nil)
	resp, err := unixipc.Client(e.socket()).Do(req)
	if err != nil {
		d.kill()
		t.Fatalf("shutdown: %v", err)
	}
	resp.Body.Close()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		d.kill()
		t.Fatal("daemon did not join")
	}
}

// runner is the real HTTP-over-Unix runner transport, including a persistent
// SSE connection and controllable exit. Closing it models process death.
type prodRunner struct {
	ln       net.Listener
	srv      *http.Server
	events   chan string
	id, sock string
	once     sync.Once
}

func startProdRunner(t *testing.T, e *prodEnv, id string, unread bool) *prodRunner {
	t.Helper()
	dir := e.run
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, id+".sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	r := &prodRunner{ln: ln, events: make(chan string, 16), id: id, sock: sock}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "adapter": "shell", "alive": true, "created_at": time.Unix(1, 0).UTC().Format(time.RFC3339), "pid": os.Getpid(), "runner_version": "e2e", "binary_hash": "e2e", "cwd": e.home, "command": []string{"/bin/sh"}, "remotes": map[string]string{}, "status": map[string]any{"working": false}, "unread": unread, "terminal_cols": 93, "terminal_rows": 31})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for {
			select {
			case s := <-r.events:
				fmt.Fprint(w, s)
				w.(http.Flusher).Flush()
			case <-q.Context().Done():
				return
			}
		}
	})
	r.srv = &http.Server{Handler: mux}
	go r.srv.Serve(ln)
	return r
}
func (r *prodRunner) exit(unread bool) {
	r.events <- fmt.Sprintf("event: status\ndata: {\"working\":false,\"unread\":%v}\n\nevent: exit\ndata: {\"exit_code\":0}\n\n", unread)
}
func (r *prodRunner) close() {
	r.once.Do(func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = r.srv.Shutdown(ctx)
		_ = r.ln.Close()
		_ = os.Remove(r.sock)
	})
}

func sessions(t *testing.T, e *prodEnv) []map[string]any {
	t.Helper()
	resp, err := unixipc.Client(e.socket()).Get("http://localhost/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Data struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"data"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env.Data.Sessions
}
func session(t *testing.T, e *prodEnv, id string) map[string]any {
	t.Helper()
	for _, s := range sessions(t, e) {
		if s["id"] == id {
			return s
		}
	}
	t.Fatalf("missing session %s", id)
	return nil
}
func waitFor(t *testing.T, why string, fn func() bool) {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout: " + why)
}
func post(t *testing.T, e *prodEnv, path string, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := unixipc.Client(e.socket()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: %d %s", path, resp.StatusCode, b)
	}
}

func scenarioUnreadRestart(t *testing.T, bin string) {
	e := newProdEnv(t)
	r := startProdRunner(t, e, "sess-e2e-unread", true)
	defer r.close()
	d := startDaemon(t, bin, e)
	r.exit(true)
	waitFor(t, "dead unread", func() bool { s := session(t, e, r.id); return s["alive"] == false && s["unread"] == true })
	post(t, e, "/v1/sessions/"+r.id+"/read", "")
	waitFor(t, "read ack", func() bool { return session(t, e, r.id)["unread"] == false })
	stopDaemon(t, d, e)
	r.close()
	d = startDaemon(t, bin, e)
	defer d.kill()
	if session(t, e, r.id)["unread"] != false {
		t.Fatal("unread resurrected")
	}
	assertInitialSSE(t, e, r.id, false)
	ro, err := centralstore.OpenReadOnly(context.Background(), e.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	row, ok, err := ro.Session(context.Background(), centralstore.SessionID(r.id))
	if err != nil || !ok || row.Unread {
		t.Fatalf("sqlite row=%+v ok=%v err=%v", row, ok, err)
	}
}
func assertInitialSSE(t *testing.T, e *prodEnv, id string, unread bool) {
	req, _ := http.NewRequest(http.MethodGet, "http://localhost/v1/events", nil)
	ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	resp, err := unixipc.Client(e.socket()).Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	names := []string{}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
		if len(names) == 2 {
			break
		}
	}
	if len(names) != 2 || names[0] != "snapshot.sessions" || names[1] != "snapshot.world" {
		t.Fatalf("initial SSE=%v", names)
	}
}
func scenarioDaemonDown(t *testing.T, bin string) {
	e := newProdEnv(t)
	a := startProdRunner(t, e, "sess-survivor", false)
	defer a.close()
	b := startProdRunner(t, e, "sess-missing", false)
	defer b.close()
	d := startDaemon(t, bin, e)
	waitFor(t, "both live", func() bool { return len(sessions(t, e)) == 2 })
	stopDaemon(t, d, e)
	b.close()
	d = startDaemon(t, bin, e)
	defer d.kill()
	waitFor(t, "convergence", func() bool { return session(t, e, b.id)["alive"] == false })
	if session(t, e, a.id)["alive"] != true {
		t.Fatal("surviving runner stamped dead")
	}
}
func scenarioContention(t *testing.T, bin string) {
	e := newProdEnv(t)
	d := startDaemon(t, bin, e)
	defer d.kill()
	// Give the contender a different daemon socket while symlinking only the
	// database and ownership lock to the incumbent's state.
	foreign := filepath.Join(e.root, "foreign-state")
	foreignDir := filepath.Join(foreign, "gmux")
	if err := os.MkdirAll(foreignDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.db", "gmuxd.lock"} {
		if err := os.Symlink(filepath.Join(e.stateDir(), name), filepath.Join(foreignDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	loser := exec.Command(bin, "run")
	loser.Env = append(e.vars(), "XDG_STATE_HOME="+foreign)
	var out bytes.Buffer
	loser.Stderr = &out
	loser.Stdout = &out
	if err := loser.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- loser.Wait() }()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		_ = loser.Process.Kill()
		t.Fatal("lock loser did not exit boundedly")
	}
	if !unixipc.Healthy(e.socket()) {
		t.Fatalf("incumbent lost contention: %s", out.String())
	}
	if d.cmd.ProcessState != nil {
		t.Fatal("incumbent exited")
	}
}
func scenarioAdminStress(t *testing.T, bin string) {
	e := newProdEnv(t)
	var d *daemonProc
	cycles := 5
	if os.Getenv("GMUX_E2E_PROFILE") == "extended" {
		cycles = 50
	}
	for i := 0; i < cycles; i++ {
		d = startDaemon(t, bin, e)
		if i%2 == 0 {
			d.kill()
		} else {
			stopDaemon(t, d, e)
		}
	}
	d = startDaemon(t, bin, e)
	defer d.kill()
	backup := filepath.Join(e.root, "backup.sqlite")
	cmd := exec.Command(bin, "state", "backup", backup)
	cmd.Env = e.vars()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("backup: %v %s", err, b)
	}
	if info, err := os.Stat(backup); err != nil || info.Size() == 0 {
		t.Fatalf("backup missing/empty: info=%v err=%v", info, err)
	}
	check := exec.Command(bin, "state", "check")
	check.Env = e.vars()
	if b, err := check.CombinedOutput(); err != nil {
		t.Fatalf("state check: %v %s", err, b)
	}
	if d.cmd.Process == nil {
		t.Fatal("daemon missing")
	}
	_ = syscall.Kill(d.cmd.Process.Pid, syscall.Signal(0))
	entries, _ := os.ReadDir("/proc")
	_ = entries
}
