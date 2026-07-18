package main

// This file is a black-box test of the built production daemon. It is only
// enabled by tools/production-e2e.sh, inside its networkless container.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
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
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
	"nhooyr.io/websocket"
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
	t.Run("death_before_apply_crash_repair", func(t *testing.T) { scenarioDeathBarrier(t, bin) })
	t.Run("verify_failure_preserves_incumbent", func(t *testing.T) { scenarioVerifyFailure(t, bin) })
	t.Run("ownership_contention", func(t *testing.T) { scenarioContention(t, bin) })
	t.Run("restart_survival", func(t *testing.T) { scenarioRestartSurvival(t, bin) })
	t.Run("route_crash_consistency", func(t *testing.T) { scenarioRouteCrashConsistency(t, bin) })
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
	ln        net.Listener
	srv       *http.Server
	events    chan string
	delivered chan struct{}
	id, sock  string
	once      sync.Once
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
	r := &prodRunner{ln: ln, events: make(chan string, 16), delivered: make(chan struct{}, 16), id: id, sock: sock}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "adapter": "shell", "alive": true, "created_at": time.Unix(1, 0).UTC().Format(time.RFC3339), "pid": os.Getpid(), "runner_version": "e2e", "binary_hash": "e2e", "cwd": e.home, "command": []string{"/bin/sh"}, "remotes": map[string]string{"credential_fixture": "alice:remote-secret@example.invalid/repo.git"}, "status": map[string]any{"working": false}, "unread": unread, "terminal_cols": 93, "terminal_rows": 31})
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
				r.delivered <- struct{}{}
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
func (r *prodRunner) crashClose() {
	r.once.Do(func() { _ = r.srv.Close(); _ = r.ln.Close(); _ = os.Remove(r.sock) })
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
	request(t, e, http.MethodPost, path, body)
}
func request(t *testing.T, e *prodEnv, method, path, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := unixipc.Client(e.socket()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return b
}
func waitVerdict(t *testing.T, e *prodEnv, id string) string {
	t.Helper()
	return string(request(t, e, http.MethodPost, "/v1/sessions/"+id+"/wait?timeout=1", ""))
}

func scenarioUnreadRestart(t *testing.T, bin string) {
	e := newProdEnv(t)
	r := startProdRunner(t, e, "sess-e2e-unread", true)
	defer r.close()
	d := startDaemon(t, bin, e)
	r.exit(true)
	waitFor(t, "dead unread", func() bool { s := session(t, e, r.id); return s["alive"] == false && s["unread"] == true })
	selectDeadSession(t, e, r.id)
	waitFor(t, "presence read ack", func() bool { return session(t, e, r.id)["unread"] == false })
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
func selectDeadSession(t *testing.T, e *prodEnv, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://localhost/v1/presence", &websocket.DialOptions{HTTPClient: unixipc.Client(e.socket())})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"client-hello","device_type":"desktop"}`)); err != nil {
		t.Fatal(err)
	}
	msg := fmt.Sprintf(`{"type":"client-state","visibility":"visible","focused":true,"selected_session_id":%q,"last_interaction":1}`, id)
	if err = conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
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
	name, data := readSSE(t, sc)
	if name != "snapshot.sessions" {
		t.Fatalf("first SSE=%q", name)
	}
	var sf struct {
		Sessions []struct {
			ID     string `json:"id"`
			Unread bool   `json:"unread"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sf.Sessions {
		if s.ID == id {
			found = true
			if s.Unread != unread {
				t.Fatalf("SSE unread=%v want %v", s.Unread, unread)
			}
		}
	}
	if !found {
		t.Fatalf("first sessions frame omitted %s: %s", id, data)
	}
	name, data = readSSE(t, sc)
	if name != "snapshot.world" {
		t.Fatalf("second SSE=%q", name)
	}
	var world map[string]json.RawMessage
	if err := json.Unmarshal(data, &world); err != nil {
		t.Fatal(err)
	}
	if _, ok := world["projects"]; !ok {
		t.Fatalf("unmatched world frame: %s", data)
	}
}
func readSSE(t *testing.T, sc *bufio.Scanner) (string, []byte) {
	t.Helper()
	var name string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			return name, []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	t.Fatalf("SSE ended: %v", sc.Err())
	return "", nil
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
	ro, err := centralstore.OpenReadOnly(context.Background(), e.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	missing, _, err := ro.Session(context.Background(), centralstore.SessionID(b.id))
	if err != nil || missing.ExitedAt == nil {
		t.Fatalf("missing row not swept: %+v %v", missing, err)
	}
	survivor, _, err := ro.Session(context.Background(), centralstore.SessionID(a.id))
	if err != nil || survivor.ExitedAt != nil {
		t.Fatalf("survivor incorrectly swept: %+v %v", survivor, err)
	}
}
func scenarioDeathBarrier(t *testing.T, bin string) {
	e := newProdEnv(t)
	r := startProdRunner(t, e, "sess-barrier", true)
	d := startDaemon(t, bin, e)
	if s := session(t, e, r.id); s["terminal_cols"] != float64(93) || s["terminal_rows"] != float64(31) {
		t.Fatalf("initial dimensions=%v", s)
	}
	// Hold SQLite's external writer lock so the real observation pipeline
	// receives the exit but cannot durably apply it before the crash.
	lockDB, err := sql.Open("sqlite", centralstore.DatabasePath(e.stateDir()))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := lockDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE local_sessions SET row_version=row_version WHERE id=?", r.id); err != nil {
		t.Fatal(err)
	}
	r.exit(true)
	select {
	case <-r.delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("exit event was not delivered")
	}
	// Wait for the daemon to actually attempt the durable apply and fail
	// with a busy-timeout error. This proves gmuxd parsed the exit and
	// reached the write path (not just flushed bytes to the socket).
	waitFor(t, "daemon observation-failed log", func() bool {
		return strings.Contains(d.log.String(), "observation failed")
	})
	roBefore, err := centralstore.OpenReadOnly(context.Background(), e.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := roBefore.Session(context.Background(), centralstore.SessionID(r.id))
	_ = roBefore.Close()
	if err != nil || before.ExitedAt != nil {
		t.Fatalf("apply was not blocked: %+v %v", before, err)
	}
	d.kill()
	_ = tx.Rollback()
	_ = lockDB.Close()
	r.crashClose()
	d = startDaemon(t, bin, e)
	defer d.kill()
	waitFor(t, "startup repair", func() bool { return session(t, e, r.id)["alive"] == false })
	s := session(t, e, r.id)
	if s["unread"] != true || s["terminal_cols"] != float64(93) || s["terminal_rows"] != float64(31) {
		t.Fatalf("facts corrupted: %v", s)
	}
	ro, err := centralstore.OpenReadOnly(context.Background(), e.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	row, _, err := ro.Session(context.Background(), centralstore.SessionID(r.id))
	if err != nil || row.ExitedAt == nil || !row.Unread || row.TerminalCols == nil || *row.TerminalCols != 93 || row.TerminalRows == nil || *row.TerminalRows != 31 {
		t.Fatalf("repaired row=%+v err=%v", row, err)
	}
}

func scenarioVerifyFailure(t *testing.T, bin string) {
	e := newProdEnv(t)
	d := startDaemon(t, bin, e)
	defer d.kill()
	before, ok := unixipc.HealthIdentity(e.socket())
	if !ok {
		t.Fatal("incumbent unhealthy")
	}
	// A separate corrupt state DB shares the incumbent's private runtime/socket:
	// if Verify were bypassed this replacement would attempt takeover.
	badState := filepath.Join(e.root, "corrupt-state")
	badDir := filepath.Join(badState, "gmux")
	if err := os.MkdirAll(badDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(centralstore.DatabasePath(badDir), []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin, "run")
	child.Env = append(e.vars(), "XDG_STATE_HOME="+badState)
	var log bytes.Buffer
	child.Stderr = &log
	child.Stdout = &log
	done := make(chan error, 1)
	go func() { done <- child.Run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("corrupt replacement succeeded")
		}
	case <-time.After(8 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("verify failure was not bounded")
	}
	if !strings.Contains(strings.ToLower(log.String()), "verify") {
		t.Fatalf("replacement did not fail Verify: %s", log.String())
	}
	after, ok := unixipc.HealthIdentity(e.socket())
	if !ok || after.PID != before.PID {
		t.Fatalf("incumbent changed: before=%+v after=%+v log=%s", before, after, log.String())
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
	if !strings.Contains(out.String(), "acquiring gmuxd.lock") {
		t.Fatalf("loser did not fail on flock contention: %s", out.String())
	}
	after, ok := unixipc.HealthIdentity(e.socket())
	if !ok || after.PID != d.cmd.Process.Pid {
		t.Fatalf("incumbent changed after contention: %+v log=%s", after, out.String())
	}
	if d.cmd.ProcessState != nil {
		t.Fatal("incumbent exited")
	}
}
func scenarioRestartSurvival(t *testing.T, bin string) {
	e := newProdEnv(t)
	a := startProdRunner(t, e, "sess-place-a", false)
	defer a.close()
	b := startProdRunner(t, e, "sess-place-b", false)
	defer b.close()
	c := startProdRunner(t, e, "sess-dismiss", false)
	defer c.close()
	sweep := startProdRunner(t, e, "sess-sweep-status", false)
	defer sweep.close()
	d := startDaemon(t, bin, e)
	waitFor(t, "restart fixtures live", func() bool { return len(sessions(t, e)) == 4 })
	catalog := fmt.Sprintf(`{"version":4,"items":[{"slug":"persist","match":[{"path":%q}],"sessions":[%q,%q,%q,%q]}]}`, e.home, a.id, b.id, c.id, sweep.id)
	request(t, e, http.MethodPut, "/v1/projects", catalog)
	waitFor(t, "placements applied", func() bool { return session(t, e, b.id)["project_slug"] == "persist" })
	exportNow := func() statetool.ExportDoc {
		cmd := exec.Command(bin, "state", "export")
		cmd.Env = e.vars()
		raw, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var x statetool.ExportDoc
		if err = json.Unmarshal(raw, &x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	b.exit(true)
	c.exit(false)
	waitFor(t, "dead persistence fixtures", func() bool { return session(t, e, b.id)["alive"] == false && session(t, e, c.id)["alive"] == false })
	deadVerdict := waitVerdict(t, e, b.id)
	b.close()
	post(t, e, "/v1/sessions/"+c.id+"/dismiss", "")
	waitFor(t, "dismiss hidden", func() bool {
		for _, s := range sessions(t, e) {
			if s["id"] == c.id {
				return false
			}
		}
		return true
	})
	c.close()
	// Capture the complete placement sequence before restart.
	before := exportNow()
	beforePlacements := make(map[string]statetool.ExportPlacement)
	for _, p := range before.Placements {
		beforePlacements[p.LocalSessionID] = p
	}
	if _, ok := beforePlacements[a.id]; !ok {
		t.Fatal("survivor unplaced before restart")
	}
	if _, ok := beforePlacements[b.id]; !ok {
		t.Fatal("dead session unplaced before restart")
	}
	if _, ok := beforePlacements[sweep.id]; !ok {
		t.Fatal("sweep session unplaced before restart")
	}
	stopDaemon(t, d, e)
	sweep.close() // no exit event: startup sweep must preserve its reported status.
	d = startDaemon(t, bin, e)
	defer d.kill()
	waitFor(t, "exit-less sweep", func() bool { return session(t, e, sweep.id)["alive"] == false })
	if got := session(t, e, a.id); got["alive"] != true || got["project_slug"] != "persist" {
		t.Fatalf("survivor registration lost project: %v", got)
	}
	// Compare the entire surviving placement sequence: parents/scopes and dense
	// positions must be identical before/after (accounting for C's dismissal).
	after := exportNow()
	afterPlacements := make(map[string]statetool.ExportPlacement)
	for _, p := range after.Placements {
		afterPlacements[p.LocalSessionID] = p
	}
	// A, B, sweep must survive with identical placements.
	for _, id := range []string{a.id, b.id, sweep.id} {
		beforeP, bOk := beforePlacements[id]
		afterP, aOk := afterPlacements[id]
		if !bOk || !aOk || afterP != beforeP {
			t.Fatalf("placement churn for %s: before=%+v after=%+v bOk=%v aOk=%v", id, beforeP, afterP, bOk, aOk)
		}
	}
	// C (dismissed) must not appear in placements after restart.
	if _, ok := afterPlacements[c.id]; ok {
		t.Fatal("dismissed session placement survived restart")
	}
	// Verify dense positions are contiguous for surviving placements.
	survivingPositions := make(map[int64]string)
	for _, p := range after.Placements {
		if p.LocalSessionID != "" {
			survivingPositions[p.Position] = p.LocalSessionID
		}
	}
	expectedCount := 3 // A, B, sweep
	if len(survivingPositions) != expectedCount {
		t.Fatalf("expected %d surviving placements, got %d: %v", expectedCount, len(survivingPositions), survivingPositions)
	}
	if got := session(t, e, b.id); got["exit_code"] != float64(0) || got["status"] == nil || got["project_slug"] != "persist" {
		t.Fatalf("dead turn/exit/placement lost: %v", got)
	}
	for _, s := range sessions(t, e) {
		if s["id"] == c.id {
			t.Fatalf("dismissed session resurfaced: %v", s)
		}
	}
	if got := waitVerdict(t, e, b.id); got != deadVerdict {
		t.Fatalf("dead wait verdict changed across restart: before=%s after=%s", deadVerdict, got)
	}
	ro, err := centralstore.OpenReadOnly(context.Background(), e.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	hidden, _, _ := ro.Session(context.Background(), centralstore.SessionID(c.id))
	if hidden.DismissedAt == nil {
		t.Fatalf("dismissal not durable: %+v", hidden)
	}
	swept, _, _ := ro.Session(context.Background(), centralstore.SessionID(sweep.id))
	if swept.ExitedAt == nil || !swept.StatusReported || swept.Working {
		t.Fatalf("sweep corrupted status: %+v", swept)
	}
}

func scenarioRouteCrashConsistency(t *testing.T, bin string) {
	e := newProdEnv(t)
	runners := []*prodRunner{startProdRunner(t, e, "sess-atomic-a", false), startProdRunner(t, e, "sess-atomic-b", false), startProdRunner(t, e, "sess-atomic-c", false)}
	for _, r := range runners {
		defer r.close()
	}
	d := startDaemon(t, bin, e)
	old := fmt.Sprintf(`{"version":4,"items":[{"slug":"old","match":[{"path":%q}],"sessions":[%q,%q,%q]}]}`, e.home, runners[0].id, runners[1].id, runners[2].id)
	newState := fmt.Sprintf(`{"version":4,"items":[{"slug":"new","match":[{"path":%q}],"sessions":[%q,%q,%q]}]}`, e.home, runners[2].id, runners[1].id, runners[0].id)
	request(t, e, http.MethodPut, "/v1/projects", old)
	// Race a real whole-catalog/multi-placement route commit against SIGKILL.
	// Wait for the daemon's operational log marker that proves it passed
	// request validation/decomposition and is about to enter the coordinator
	// mutation (not a fixed sleep that could fire before handler dispatch).
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPut, "http://localhost/v1/projects", strings.NewReader(newState))
		req.Header.Set("Content-Type", "application/json")
		resp, err := unixipc.Client(e.socket()).Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "projects-replace-pending log", func() bool {
		return strings.Contains(d.log.String(), "projects-replace-pending")
	})
	d.kill()
	<-done
	d = startDaemon(t, bin, e)
	export := func() statetool.ExportDoc {
		cmd := exec.Command(bin, "state", "export")
		cmd.Env = e.vars()
		raw, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var x statetool.ExportDoc
		if err = json.Unmarshal(raw, &x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	x := export()
	if len(x.Projects) != 1 || len(x.Placements) != 3 {
		t.Fatalf("partial catalog transaction: %+v", x)
	}
	slug := x.Projects[0].Slug
	if slug != "old" && slug != "new" {
		t.Fatalf("neither old nor new catalog: %+v", x.Projects)
	}
	positions := map[int64]bool{}
	for _, p := range x.Placements {
		if p.ProjectSlug != slug {
			t.Fatalf("mixed project scopes: %+v", x.Placements)
		}
		positions[p.Position] = true
	}
	if len(positions) != 3 {
		t.Fatalf("partial reorder positions: %+v", x.Placements)
	}
	// Race the real manual-peer transaction. Its row must be absent or complete.
	// Wait for the daemon's operational log marker that proves the health probe
	// completed and the handler is about to enter UpsertManualPeer.
	peer := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"service": "gmuxd", "node_id": "atomic-node", "hostname": "atomic-peer"}})
			return
		}
		http.NotFound(w, r)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go peer.Serve(ln)
	defer peer.Close()
	secret := "atomic-token-secret"
	peerBody := fmt.Sprintf(`{"URL":%q,"Token":%q}`, "http://user:atomic-url-secret@"+ln.Addr().String(), secret)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		req, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/peers", strings.NewReader(peerBody))
		req.Header.Set("Content-Type", "application/json")
		resp, err := unixipc.Client(e.socket()).Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	waitFor(t, "peer-upsert-pending log", func() bool {
		return strings.Contains(d.log.String(), "peer-upsert-pending")
	})
	d.kill()
	<-peerDone
	d = startDaemon(t, bin, e)
	defer d.kill()
	x = export()
	if len(x.Peers) > 1 {
		t.Fatalf("partial peer rows: %+v", x.Peers)
	}
	if len(x.Peers) == 1 {
		p := x.Peers[0]
		if p.Name != "atomic-peer" || p.URL == "" || !p.TokenPresent || p.CreatedAtMs == 0 || p.UpdatedAtMs == 0 {
			t.Fatalf("incomplete peer row: %+v", p)
		}
	}
	raw, _ := json.Marshal(x)
	for _, s := range []string{secret, "atomic-url-secret"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatalf("secret leaked from export: %s", raw)
		}
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
		r := startProdRunner(t, e, fmt.Sprintf("sess-stress-%03d", i), true)
		d = startDaemon(t, bin, e)
		waitFor(t, "stress runner live", func() bool { return session(t, e, r.id)["alive"] == true })
		r.exit(true)
		waitFor(t, "stress runner dead", func() bool { return session(t, e, r.id)["alive"] == false })
		post(t, e, "/v1/sessions/"+r.id+"/read", "")
		waitFor(t, "stress read durable", func() bool { return session(t, e, r.id)["unread"] == false })
		r.close()
		if i%2 == 0 {
			d.kill()
		} else {
			stopDaemon(t, d, e)
		}
	}
	d = startDaemon(t, bin, e)
	defer d.kill()
	all := sessions(t, e)
	if len(all) != cycles {
		t.Fatalf("final rows=%d want %d", len(all), cycles)
	}
	for _, s := range all {
		if s["alive"] != false || s["unread"] != false || s["exit_code"] != float64(0) {
			t.Fatalf("bad final stress row: %v", s)
		}
	}
	backup := filepath.Join(e.root, "backup.sqlite")
	cmd := exec.Command(bin, "state", "backup", backup)
	cmd.Env = e.vars()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("backup: %v %s", err, b)
	}
	if info, err := os.Stat(backup); err != nil || info.Size() == 0 {
		t.Fatalf("backup missing/empty: info=%v err=%v", info, err)
	}
	db, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	var quick string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&quick); err != nil || quick != "ok" {
		t.Fatalf("backup quick_check=%q err=%v", quick, err)
	}
	db.Close()
	peer := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"service": "gmuxd", "node_id": "peer-node", "hostname": "secret-peer"}})
			return
		}
		http.NotFound(w, r)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go peer.Serve(ln)
	defer peer.Close()
	url := "http://urluser:urlpass@" + ln.Addr().String()
	post(t, e, "/v1/peers", fmt.Sprintf(`{"URL":%q,"Token":"peer-token-secret"}`, url))
	ex := exec.Command(bin, "state", "export")
	ex.Env = e.vars()
	raw, err := ex.CombinedOutput()
	if err != nil {
		t.Fatalf("export: %v %s", err, raw)
	}
	for _, secret := range []string{"peer-token-secret", "urlpass", "remote-secret"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("export leaked %q: %s", secret, raw)
		}
	}
	check := exec.Command(bin, "state", "check")
	check.Env = e.vars()
	if b, err := check.CombinedOutput(); err != nil {
		t.Fatalf("state check: %v %s", err, b)
	}
	if d.cmd.Process == nil {
		t.Fatal("daemon missing")
	}
	if err := syscall.Kill(d.cmd.Process.Pid, syscall.Signal(0)); err != nil {
		t.Fatalf("daemon unexpectedly absent: %v", err)
	}
	for _, r := range sessions(t, e) {
		if r["alive"] == true {
			t.Fatalf("runner leaked alive: %v", r)
		}
	}
}
