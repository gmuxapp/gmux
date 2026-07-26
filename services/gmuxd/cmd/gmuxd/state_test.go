package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

// isolateState points StateDir (and so SocketPath) at a fresh directory.
func isolateState(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serveFakeDaemon serves mux on the isolated daemon socket.
func serveFakeDaemon(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	ln, err := unixipc.Listen(paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func seedStateDB(t *testing.T, dir string) {
	t.Helper()
	store, err := centralstore.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertSession(context.Background(), centralstore.NewSession{
		ID: "sess", Adapter: "shell", Command: []string{"sh"}, CWD: "/", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStateUsageAndHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runState(nil, &out, &errOut); code != 2 {
		t.Fatalf("no args: code %d", code)
	}
	out.Reset()
	if code := runState([]string{"check", "--help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "Exit codes") {
		t.Fatalf("help: code/output wrong: %s", out.String())
	}
	if code := runState([]string{"bogus"}, &out, &errOut); code != 2 {
		t.Fatal("unknown subcommand must be a usage error")
	}
	if code := runState([]string{"backup"}, &out, &errOut); code != 2 {
		t.Fatal("backup without a path must be a usage error")
	}
}

func TestStateCheckOnline(t *testing.T) {
	dir := isolateState(t)
	store, err := centralstore.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mux := http.NewServeMux()
	(&statetool.Handler{Store: store}).Register(mux)
	serveFakeDaemon(t, mux)

	var out, errOut bytes.Buffer
	if code := runState([]string{"check"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "(online): ok") {
		t.Fatalf("output %q", out.String())
	}
}

func TestStateAgainstDaemonWithoutRoutes(t *testing.T) {
	// Pre-cutover daemon: healthy, but no /v1/state routes on its mux. The
	// error UX must read sensibly and never fall back to offline.
	isolateState(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	serveFakeDaemon(t, mux)

	for _, sub := range []string{"check", "export"} {
		var out, errOut bytes.Buffer
		if code := runState([]string{sub}, &out, &errOut); code != 3 {
			t.Fatalf("%s: code %d, stderr %s", sub, code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "does not serve state routes") {
			t.Fatalf("%s: stderr %q", sub, errOut.String())
		}
	}
}

func TestStateTransportTimeoutDoesNotFallBack(t *testing.T) {
	dir := isolateState(t)
	seedStateDB(t, dir) // old behavior would have succeeded offline
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state/check", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	serveFakeDaemon(t, mux)
	old := stateCheckTimeout
	stateCheckTimeout = 20 * time.Millisecond
	t.Cleanup(func() { stateCheckTimeout = old })

	var out, errOut bytes.Buffer
	if code := runState([]string{"check"}, &out, &errOut); code != 3 {
		t.Fatalf("code %d, output %q", code, out.String())
	}
	if !strings.Contains(errOut.String(), "daemon did not answer") || !strings.Contains(errOut.String(), "not falling back") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestStateAcceptedThenClosedDoesNotFallBack(t *testing.T) {
	dir := isolateState(t)
	seedStateDB(t, dir) // old behavior would have succeeded offline
	ln, err := unixipc.Listen(paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	var out, errOut bytes.Buffer
	if code := runState([]string{"check"}, &out, &errOut); code != 3 {
		t.Fatalf("code %d, output %q", code, out.String())
	}
	if !strings.Contains(errOut.String(), "not falling back") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestStateAgainstInertRoutes(t *testing.T) {
	// Post-S5 daemon shape with no store open: registered-but-inert routes
	// answer 503 central_store_not_active.
	isolateState(t)
	mux := http.NewServeMux()
	(&statetool.Handler{Store: nil}).Register(mux)
	serveFakeDaemon(t, mux)

	var out, errOut bytes.Buffer
	if code := runState([]string{"check"}, &out, &errOut); code != 3 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut.String(), "no central store active") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestStateCheckAndBackupOffline(t *testing.T) {
	dir := isolateState(t)
	seedStateDB(t, dir)

	var out, errOut bytes.Buffer
	if code := runState([]string{"check"}, &out, &errOut); code != 0 {
		t.Fatalf("offline check: code %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "(offline): ok") {
		t.Fatalf("output %q", out.String())
	}

	target := filepath.Join(t.TempDir(), "backup.db")
	out.Reset()
	if code := runState([]string{"backup", target}, &out, &errOut); code != 0 {
		t.Fatalf("offline backup: code %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), statetool.BackupNote) {
		t.Fatalf("backup output must warn about tokens: %q", out.String())
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup stat = %v, %v", info, err)
	}
}

func TestStateExportRequiresDaemon(t *testing.T) {
	dir := isolateState(t)
	seedStateDB(t, dir)
	var out, errOut bytes.Buffer
	if code := runState([]string{"export"}, &out, &errOut); code != 3 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut.String(), "requires a running daemon") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestStateOfflineRefusedWhenNoDatabase(t *testing.T) {
	isolateState(t)
	var out, errOut bytes.Buffer
	if code := runState([]string{"check"}, &out, &errOut); code != 3 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errOut.String(), "offline mode unavailable") {
		t.Fatalf("stderr %q", errOut.String())
	}
}
