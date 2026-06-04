package peerstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportLegacyDiscovery(t *testing.T) {
	dir := t.TempDir()
	cache := `{"devices":{
		"n1":{"fqdn":"gmux-laptop.angler-map.ts.net","peer_name":"laptop","is_gmux":true},
		"n2":{"fqdn":"phone.angler-map.ts.net","peer_name":"","is_gmux":false},
		"n3":{"fqdn":"gmux-server.angler-map.ts.net","peer_name":"server","is_gmux":true}
	}}`
	if err := os.WriteFile(filepath.Join(dir, legacyDiscoveryFile), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(dir)
	// Both gmux devices are referenced by a project; the non-gmux one
	// can't be (and is filtered by IsGmux anyway).
	referenced := map[string]bool{"laptop": true, "server": true}
	n, err := s.ImportLegacyDiscovery(dir, referenced)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("imported = %d, want 2 (referenced gmux devices)", n)
	}

	recs := s.List()
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Token != "" || r.NodeID != "" {
			t.Errorf("migrated peer %q should have no token/node_id, got %+v", r.Name, r)
		}
		if r.URL == "" || r.URL[:8] != "https://" {
			t.Errorf("migrated peer %q should have an https URL, got %q", r.Name, r.URL)
		}
	}

	// Cache is removed so the migration runs once.
	if _, err := os.Stat(filepath.Join(dir, legacyDiscoveryFile)); !os.IsNotExist(err) {
		t.Fatal("legacy cache should be removed after import")
	}

	// Re-running is a no-op (cache gone).
	if n, err := s.ImportLegacyDiscovery(dir, referenced); err != nil || n != 0 {
		t.Fatalf("second import = (%d, %v), want (0, nil)", n, err)
	}
}

// Only hosts a project references are carried forward; an unreferenced
// gmux device is skipped, but the cache is still removed (migration runs
// once regardless).
func TestImportLegacyDiscoverySkipsUnreferenced(t *testing.T) {
	dir := t.TempDir()
	cache := `{"devices":{
		"n1":{"fqdn":"gmux-laptop.ts.net","peer_name":"laptop","is_gmux":true},
		"n2":{"fqdn":"gmux-spare.ts.net","peer_name":"spare","is_gmux":true}
	}}`
	if err := os.WriteFile(filepath.Join(dir, legacyDiscoveryFile), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(dir)
	n, err := s.ImportLegacyDiscovery(dir, map[string]bool{"laptop": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("imported = %d, want 1 (only the referenced host)", n)
	}
	if recs := s.List(); len(recs) != 1 || recs[0].Name != "laptop" {
		t.Fatalf("want only 'laptop', got %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyDiscoveryFile)); !os.IsNotExist(err) {
		t.Fatal("cache should be removed even when some devices are skipped")
	}
}

func TestImportLegacyDiscoveryAbsent(t *testing.T) {
	s, _ := Open(t.TempDir())
	if n, err := s.ImportLegacyDiscovery(t.TempDir(), map[string]bool{"x": true}); err != nil || n != 0 {
		t.Fatalf("absent cache = (%d, %v), want (0, nil)", n, err)
	}
}

func TestOpenEmptyWhenAbsent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("want empty, got %v", s.List())
	}
}

func TestAddOrGetPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if _, _, err := s.AddOrGet(Record{Name: "laptop", URL: "https://gmux-laptop.ts.net", Token: "secret", NodeID: "node_a"}); err != nil {
		t.Fatal(err)
	}

	// File is 0600 (carries a token).
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", info.Mode().Perm())
	}

	// Reloads identically.
	s2, _ := Open(dir)
	recs := s2.List()
	if len(recs) != 1 || recs[0].Name != "laptop" || recs[0].Token != "secret" || recs[0].NodeID != "node_a" {
		t.Fatalf("reloaded = %+v", recs)
	}
}

func TestAddOrGetRejectsBadURL(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.AddOrGet(Record{Name: "x", URL: "ftp://nope", NodeID: "n"}); err == nil {
		t.Fatal("expected error for non-http url")
	}
}

// Re-adding the same node_id matches the existing record (no duplicate)
// and refreshes its URL/token in place, keeping the display name — under
// one lock, so it's safe against concurrent connects (no check-then-act
// race).
func TestAddOrGetDedupsByNodeID(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	first, _, _ := s.AddOrGet(Record{Name: "laptop", URL: "http://a:8790", Token: "t1", NodeID: "node_x"})

	got, outcome, err := s.AddOrGet(Record{Name: "laptop-again", URL: "http://b:8790", Token: "t2", NodeID: "node_x"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Updated {
		t.Fatalf("re-adding same node_id with new creds should report Updated, got %v", outcome)
	}
	if got.Name != first.Name || got.URL != "http://b:8790" || got.Token != "t2" {
		t.Fatalf("upsert should keep the name and refresh url/token, got %+v", got)
	}
	if len(s.List()) != 1 {
		t.Fatalf("upsert must not append, got %d records", len(s.List()))
	}
}

// Re-adding identical creds is a no-op: outcome Unchanged, no rewrite.
func TestAddOrGetUnchanged(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.AddOrGet(Record{Name: "laptop", URL: "http://a:8790", Token: "t", NodeID: "node_x"})
	_, outcome, err := s.AddOrGet(Record{Name: "laptop", URL: "http://a:8790", Token: "t", NodeID: "node_x"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Unchanged {
		t.Fatalf("identical re-add should report Unchanged, got %v", outcome)
	}
}

// A host added without a node_id (e.g. migrated from autodiscovery) is
// matched by URL on the next connect, so supplying a token updates that
// record and stamps the now-known node_id instead of duplicating it.
func TestAddOrGetUpsertsByURLWhenNodeIDUnknown(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.AddOrGet(Record{Name: "old-tower", URL: "https://old-tower.ts.net"}) // no token, no node_id

	got, outcome, err := s.AddOrGet(Record{Name: "old-tower", URL: "https://old-tower.ts.net", Token: "secret", NodeID: "node_ot"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Updated {
		t.Fatalf("supplying a token for a URL-matched host should report Updated, got %v", outcome)
	}
	if got.Token != "secret" || got.NodeID != "node_ot" {
		t.Fatalf("upsert should set token and stamp node_id, got %+v", got)
	}
	if len(s.List()) != 1 {
		t.Fatalf("URL match must not append, got %d records", len(s.List()))
	}
}

// Distinct URLs with no node_id stay distinct hosts.
func TestAddOrGetEmptyNodeIDDistinctURLsNotDeduped(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.AddOrGet(Record{Name: "a", URL: "http://a:8790"})
	s.AddOrGet(Record{Name: "b", URL: "http://b:8790"})
	if len(s.List()) != 2 {
		t.Fatalf("distinct URLs must not dedup, got %d", len(s.List()))
	}
}

func TestAddOrGetSlugifiesAndSuffixes(t *testing.T) {
	s, _ := Open(t.TempDir())
	// Non-slug self-reported name is slugified rather than rejected.
	got, _, err := s.AddOrGet(Record{Name: "My-Laptop.local", URL: "http://a:8790", NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-laptop-local" {
		t.Fatalf("slugified name = %q, want my-laptop-local", got.Name)
	}
	// A different node reporting a name that slugifies to the same value
	// is suffixed.
	got2, _, _ := s.AddOrGet(Record{Name: "my-laptop-local", URL: "http://b:8790", NodeID: "n2"})
	if got2.Name != "my-laptop-local-2" {
		t.Fatalf("collision name = %q, want my-laptop-local-2", got2.Name)
	}
}

func TestAddOrGetRejectsUnslugifiableName(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, _, err := s.AddOrGet(Record{Name: "...", URL: "http://a:8790", NodeID: "n"}); err == nil {
		t.Fatal("expected error for a name with no usable slug characters")
	}
}

func TestSlugify(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"My-Laptop.local", "my-laptop-local"},
		{"gmux-server", "gmux-server"},
		{"  Spaces  ", "spaces"},
		{"...", ""},
		{"", ""},
	} {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A failed disk write during Remove must roll back the in-memory slice,
// so the store stays consistent with disk and a later retry can succeed
// (rather than leaving the peer gone-from-memory but still on disk).
func TestRemoveRollsBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.AddOrGet(Record{Name: "keep", URL: "http://a:8790", NodeID: "n"})

	// Make the state dir unwritable so save() can't create its tmp file
	// during the next Remove (a read-only file wouldn't block the
	// atomic rename, but a read-only dir blocks the tmp write).
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, ok, err := s.Remove("keep")
	if err == nil || ok {
		t.Fatalf("expected save failure (ok=%v err=%v)", ok, err)
	}
	if recs := s.List(); len(recs) != 1 || recs[0].Name != "keep" {
		t.Fatalf("record must be rolled back in memory, got %+v", recs)
	}
}

// A 0-byte peers.json (e.g. left by a non-atomic write in an older
// build) must not wedge startup: Open treats it as an empty store.
func TestOpenToleratesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open on empty file should succeed, got %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("want empty store, got %v", s.List())
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.AddOrGet(Record{Name: "gone", URL: "http://h:8790", NodeID: "n"})
	rec, ok, err := s.Remove("gone")
	if err != nil || !ok || rec.Name != "gone" {
		t.Fatalf("remove = %+v ok=%v err=%v", rec, ok, err)
	}
	if _, ok, _ := s.Remove("gone"); ok {
		t.Fatal("second remove should report not found")
	}
	s2, _ := Open(dir)
	if len(s2.List()) != 0 {
		t.Fatalf("want empty after remove, got %v", s2.List())
	}
}
