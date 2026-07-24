package statetool

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func openStore(t *testing.T) (*centralstore.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := centralstore.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func seed(t *testing.T, s *centralstore.Store) {
	t.Helper()
	ctx := context.Background()
	cat, _, err := s.ReplaceProjectCatalog(ctx, []centralstore.ProjectEntrySpec{
		{Owned: &centralstore.OwnedProjectSpec{Slug: "proj", Rules: []centralstore.MatchRule{{Path: "/proj"}}}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"beta", "alpha"} {
		if _, _, err := s.InsertSession(ctx, centralstore.NewSession{
			ID: centralstore.SessionID(id), Adapter: "shell", Command: []string{"sh"},
			CWD: "/proj", Remotes: map[string]string{"origin": "https://user:secret@git.example/repo.git"},
			CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PlaceLocalSession(ctx, centralstore.SessionID(id), cat[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := s.UpsertManualPeer(ctx, centralstore.ManualPeerSpec{
		Name: "laptop", URL: "https://user:pass@laptop:7369", Token: "super-secret", NodeID: "node-1",
	}, 5); err != nil {
		t.Fatal(err)
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"https://host:7369":                   "https://host:7369",
		"https://user:pass@host:7369/x":       "https://REDACTED@host:7369/x",
		"https://user@host":                   "https://REDACTED@host",
		"not a url ::":                        "not a url ::",
		"ssh://git@github.com/o/r.git":        "ssh://REDACTED@github.com/o/r.git",
		"alice:hunter2@host.example/repo.git": "REDACTED@host.example/repo.git",
		"git@host.example:repo.git":           "git@host.example:repo.git",
		"https://host/x?access_token=visible": "https://host/x?access_token=visible",
	}
	for in, want := range cases {
		if got := RedactURLUserinfo(in); got != want {
			t.Errorf("RedactURLUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportIsRedactedAndDeterministic(t *testing.T) {
	s, _ := openStore(t)
	seed(t, s)
	ctx := context.Background()

	doc, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "pass") {
		t.Fatalf("export leaked a secret: %s", raw)
	}
	if len(doc.Peers) != 1 || !doc.Peers[0].TokenPresent || doc.Peers[0].URL != "https://REDACTED@laptop:7369" {
		t.Fatalf("peers = %+v", doc.Peers)
	}
	if len(doc.Sessions) != 2 || doc.Sessions[0].ID != "alpha" || doc.Sessions[1].ID != "beta" {
		t.Fatalf("sessions not ID-sorted: %+v", doc.Sessions)
	}
	if doc.Sessions[0].Remotes["origin"] != "https://REDACTED@git.example/repo.git" {
		t.Fatalf("remote not scrubbed: %+v", doc.Sessions[0].Remotes)
	}
	if len(doc.Projects) != 1 || doc.Projects[0].Slug != "proj" || len(doc.Placements) != 2 {
		t.Fatalf("projects/placements = %+v / %+v", doc.Projects, doc.Placements)
	}

	// Verify started_at_ms and exited_at_ms round-trip through export
	// when populated (pins the export shape against the schema).
	started := centralstore.UnixMillis(1000)
	exited := centralstore.UnixMillis(2000)
	if _, err := s.ApplyCommonFacts(ctx, "alpha", 1, centralstore.CommonFactsPatch{
		StartedAt: centralstore.NullablePatch[centralstore.UnixMillis]{Set: &started},
		ExitedAt:  centralstore.NullablePatch[centralstore.UnixMillis]{Set: &exited},
	}); err != nil {
		t.Fatal(err)
	}
	doc, err = Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	alpha := doc.Sessions[0] // sorted: alpha < beta
	if alpha.ID != "alpha" {
		t.Fatalf("expected alpha first, got %s", alpha.ID)
	}
	if alpha.StartedAtMs == nil || *alpha.StartedAtMs != 1000 {
		t.Fatalf("started_at_ms = %v, want 1000", alpha.StartedAtMs)
	}
	if alpha.ExitedAtMs == nil || *alpha.ExitedAtMs != 2000 {
		t.Fatalf("exited_at_ms = %v, want 2000", alpha.ExitedAtMs)
	}
	// Refresh raw for the determinism check below.
	raw, _ = json.Marshal(doc)

	// Deterministic: a second export marshals byte-identically.
	doc2, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := json.Marshal(doc2)
	if !bytes.Equal(raw, raw2) {
		t.Fatalf("export not deterministic:\n%s\n%s", raw, raw2)
	}
}

// -- HTTP handlers ----------------------------------------------------------

func serveState(t *testing.T, store *centralstore.Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Handler{Store: store}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decode(t *testing.T, resp *http.Response) envelope {
	t.Helper()
	defer resp.Body.Close()
	var e envelope
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRoutesAnswer503WithoutAStore(t *testing.T) {
	// Pre-switch behavior (design S3): routes registered, no DB open.
	srv := serveState(t, nil)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/state/check"},
		{"POST", "/v1/state/backup"},
		{"GET", "/v1/state/export"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(`{"path":"/tmp/x"}`))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status %d, want 503", tc.method, tc.path, resp.StatusCode)
		}
		e := decode(t, resp)
		if e.OK || e.Error.Code != "central_store_not_active" {
			t.Fatalf("%s %s: envelope %+v", tc.method, tc.path, e)
		}
	}
}

func TestCheckRoute(t *testing.T) {
	s, _ := openStore(t)
	seed(t, s)
	srv := serveState(t, s)
	resp, err := srv.Client().Get(srv.URL + "/v1/state/check")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var report CheckReport
	e := decode(t, resp)
	if err := json.Unmarshal(e.Data, &report); err != nil {
		t.Fatal(err)
	}
	if !e.OK || !report.OK || len(report.Findings) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestBackupRoute(t *testing.T) {
	s, _ := openStore(t)
	seed(t, s)
	srv := serveState(t, s)
	target := filepath.Join(t.TempDir(), "b.db")

	post := func(body string) *http.Response {
		resp, err := srv.Client().Post(srv.URL+"/v1/state/backup", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := post(`{"path":"relative.db"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("relative path: status %d, want 400", resp.StatusCode)
	}
	resp := post(`{"path":"` + target + `"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var result BackupResult
	if err := json.Unmarshal(decode(t, resp).Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Path != target || result.Note != BackupNote {
		t.Fatalf("result = %+v", result)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup stat = %v, %v", info, err)
	}
	// Existing target refused.
	if resp := post(`{"path":"` + target + `"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("existing target: status %d, want 409", resp.StatusCode)
	}
}

func TestExportRoute(t *testing.T) {
	s, _ := openStore(t)
	seed(t, s)
	srv := serveState(t, s)
	resp, err := srv.Client().Get(srv.URL + "/v1/state/export")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	e := decode(t, resp)
	if strings.Contains(string(e.Data), "super-secret") {
		t.Fatalf("export route leaked a token: %s", e.Data)
	}
	var doc ExportDoc
	if err := json.Unmarshal(e.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sessions) != 2 || len(doc.Peers) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
}

// -- Offline gate ------------------------------------------------------------

func TestOpenOfflineAllowsStaleUnlockedLockFile(t *testing.T) {
	s, dir := openStore(t)
	seed(t, s)
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := OpenOffline(context.Background(), dir, func() bool { return false })
	if err != nil {
		t.Fatalf("stale unlocked lock file must not imply ownership: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenOfflineRefusesWhenLockHeld(t *testing.T) {
	s, dir := openStore(t)
	seed(t, s)
	// Simulate a daemon holding the advisory lock.
	lock, err := os.OpenFile(filepath.Join(dir, LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if _, err := OpenOffline(context.Background(), dir, func() bool { return false }); !errors.Is(err, ErrDaemonOwnsDatabase) {
		t.Fatalf("expected ErrDaemonOwnsDatabase, got %v", err)
	}
}

func TestOpenOfflineRefusesWhenDaemonHealthy(t *testing.T) {
	s, dir := openStore(t)
	seed(t, s)
	if _, err := OpenOffline(context.Background(), dir, func() bool { return true }); !errors.Is(err, ErrDaemonOwnsDatabase) {
		t.Fatalf("expected ErrDaemonOwnsDatabase, got %v", err)
	}
	// The refusal must not leave the flock held.
	h, err := OpenOffline(context.Background(), dir, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenOfflineBeginImmediateFailureReleasesFlock(t *testing.T) {
	s, dir := openStore(t)
	seed(t, s)
	// Hold SQLite's writer reservation without holding gmuxd.lock, forcing
	// OpenOffline to fail specifically at BEGIN IMMEDIATE.
	db, err := sql.Open("sqlite", "file:"+centralstore.DatabasePath(dir)+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")

	if _, err := OpenOffline(context.Background(), dir, func() bool { return false }); !errors.Is(err, ErrDaemonOwnsDatabase) {
		t.Fatalf("expected BEGIN IMMEDIATE ownership refusal, got %v", err)
	}
	// The failure path must have released the advisory flock/resources.
	lock, err := os.OpenFile(filepath.Join(dir, LockFileName), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock leaked after BEGIN IMMEDIATE failure: %v", err)
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}

func TestOpenOfflineHoldsWriteTransaction(t *testing.T) {
	s, dir := openStore(t)
	seed(t, s)
	ctx := context.Background()
	h, err := OpenOffline(ctx, dir, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = h.Close()
		}
	}()

	// While the gate is held, no writer can commit: the store's write
	// (same file, separate connection) must fail on the busy timeout.
	writeCtx, cancel := context.WithTimeout(ctx, 8e9)
	defer cancel()
	if _, _, err := s.InsertSession(writeCtx, centralstore.NewSession{
		ID: "blocked", Adapter: "shell", CWD: "/", CreatedAt: 1,
	}); err == nil {
		t.Fatal("expected a write to fail while the offline gate holds BEGIN IMMEDIATE")
	}

	// Reads (offline check, backup) still work against a read-only handle.
	ro, err := centralstore.OpenReadOnly(ctx, h.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	findings, err := ro.CheckState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v", findings)
	}
	target := filepath.Join(t.TempDir(), "offline.db")
	if err := ro.BackupInto(ctx, target); err != nil {
		t.Fatal(err)
	}

	// After Close, writes proceed again.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, _, err := s.InsertSession(ctx, centralstore.NewSession{
		ID: "unblocked", Adapter: "shell", CWD: "/", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenOfflineMissingDatabase(t *testing.T) {
	if _, err := OpenOffline(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("expected an error for a missing database")
	}
}
