package projects

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateV1ToV2(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	// Use the real $HOME so CanonicalizePath works correctly.
	v1Data := fmt.Sprintf(`{
  "items": [
    {
      "slug": "gmux",
      "remote": "github.com/gmuxapp/gmux",
      "paths": ["%s/dev/gmux"],
      "sessions": ["hub-protocol", "gmux"]
    },
    {
      "slug": "tmp",
      "paths": ["/tmp"],
      "sessions": ["new"]
    },
    {
      "slug": "home",
      "paths": ["%s"]
    }
  ]
}`, homeDir, homeDir)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(v1Data), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := Load(dir, map[string]string{
		"hub-protocol": "sess-hub",
		"gmux":         "sess-gmux",
		"new":          "sess-new",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if state.Version != currentVersion {
		t.Errorf("version = %d, want %d", state.Version, currentVersion)
	}
	if len(state.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(state.Items))
	}

	// Item 0: gmux — remote + canonicalized path.
	gmux := state.Items[0]
	if gmux.Slug != "gmux" {
		t.Errorf("item 0 slug = %q", gmux.Slug)
	}
	if len(gmux.Match) != 2 {
		t.Fatalf("gmux: expected 2 match rules, got %d", len(gmux.Match))
	}
	if gmux.Match[0].Remote != "github.com/gmuxapp/gmux" {
		t.Errorf("gmux rule 0: remote = %q", gmux.Match[0].Remote)
	}
	if gmux.Match[1].Path != "~/dev/gmux" {
		t.Errorf("gmux rule 1: path = %q, want ~/dev/gmux", gmux.Match[1].Path)
	}
	if got, want := fmt.Sprint(gmux.Sessions), "[sess-hub sess-gmux]"; got != want {
		t.Errorf("gmux sessions = %v, want %s", gmux.Sessions, want)
	}

	// Item 1: tmp — path outside $HOME stays absolute.
	tmp := state.Items[1]
	if len(tmp.Match) != 1 || tmp.Match[0].Path != "/tmp" {
		t.Errorf("tmp match = %+v", tmp.Match)
	}

	// Item 2: home — $HOME itself becomes ~.
	homeItem := state.Items[2]
	if len(homeItem.Match) != 1 || homeItem.Match[0].Path != "~" {
		t.Errorf("home match = %+v, want [{path: ~}]", homeItem.Match)
	}
	if len(homeItem.Sessions) != 0 {
		t.Errorf("home sessions = %v", homeItem.Sessions)
	}
}

func TestMigrateV2ToV3(t *testing.T) {
	// v2 data is structurally compatible with v3: the only schema
	// difference is the addition of reference items and the removal
	// of MatchRule.hosts. v2 files load cleanly; the hosts field is
	// silently dropped because the struct no longer carries it.
	v2Data := `{
  "version": 2,
  "items": [
    {
      "slug": "gmux",
      "match": [
        {"remote": "github.com/gmuxapp/gmux"},
        {"path": "~/dev/gmux", "hosts": ["laptop"]}
      ],
      "sessions": ["hub-protocol"]
    }
  ]
}`

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(v2Data), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if state.Version != currentVersion {
		t.Errorf("version = %d, want %d", state.Version, currentVersion)
	}
	if len(state.Items) != 1 {
		t.Fatalf("items = %d", len(state.Items))
	}
	if state.Items[0].Match[0].Remote != "github.com/gmuxapp/gmux" {
		t.Errorf("remote = %q", state.Items[0].Match[0].Remote)
	}
	if state.Items[0].Match[1].Path != "~/dev/gmux" {
		t.Errorf("path = %q", state.Items[0].Match[1].Path)
	}
}

func TestV3References(t *testing.T) {
	// Mixed v3 file with an owned project and a reference to a peer's
	// project. Both shapes load as Item; IsReference distinguishes.
	v3Data := `{
  "version": 3,
  "items": [
    {"slug": "gmux", "match": [{"path": "~/dev/gmux"}], "sessions": ["s1"]},
    {"slug": "claude", "peer": "workstation"}
  ]
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(v3Data), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(state.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(state.Items))
	}
	if state.Items[0].IsReference() {
		t.Error("item 0 should be owned")
	}
	if !state.Items[1].IsReference() {
		t.Error("item 1 should be a reference")
	}
	if state.Items[1].Peer != "workstation" {
		t.Errorf("item 1 peer = %q, want workstation", state.Items[1].Peer)
	}
	// Validate accepts the mixed file.
	if err := state.Validate(); err != nil {
		t.Errorf("validate mixed v3: %v", err)
	}
}

func TestLoadBacksUpBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	bak := path + ".bak"

	// A pre-version (v0/v1) file triggers a real migration → backup.
	v1 := `{"items":[{"slug":"gmux","remote":"github.com/gmuxapp/gmux","paths":["/home/x/dev/gmux"]}]}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", bak, err)
	}
	if string(got) != v1 {
		t.Fatalf("backup should hold the verbatim pre-migration bytes, got %q", got)
	}

	// A file already at the current version is not backed up again.
	os.Remove(bak)
	current := `{"version":` + itoa(currentVersion) + `,"items":[]}`
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Fatal("a current-version file should not be backed up")
	}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func TestMigrateRoundtrip(t *testing.T) {
	// Save state, reload, verify identical.
	dir := t.TempDir()
	original := &State{
		Version: currentVersion,
		Items: []Item{
			{Slug: "test", Match: []MatchRule{{Path: "~/projects/test"}}, Sessions: []string{"s1"}},
			{Slug: "remote", Peer: "workstation"},
		},
	}
	if err := original.Save(dir); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify by re-marshaling both and comparing.
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(loaded)
	if string(a) != string(b) {
		t.Errorf("roundtrip mismatch:\n  original: %s\n  loaded:   %s", a, b)
	}
}

func TestLoadMigratesV3SessionSlugsToIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	v3 := `{
  "version": 3,
  "items": [{
    "slug": "gmux",
    "match": [{"path": "~/dev/gmux"}],
    "sessions": ["sess-existing", "old-slug", "missing-slug", "duplicate-slug", "01234567-89ab-cdef-0123-456789abcdef"]
  }]
}`
	if err := os.WriteFile(path, []byte(v3), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := Load(dir, map[string]string{
		"old-slug":       "sess-resolved",
		"duplicate-slug": "sess-existing",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "[sess-existing sess-resolved 01234567-89ab-cdef-0123-456789abcdef]"
	if got := fmt.Sprint(state.Items[0].Sessions); got != want {
		t.Errorf("sessions = %s, want %s", got, want)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("reading migration backup: %v", err)
	}
	if string(backup) != v3 {
		t.Errorf("backup = %q, want original v3 bytes", backup)
	}
}

func TestLoadV4DoesNotConvertSessionSlugs(t *testing.T) {
	dir := t.TempDir()
	v4 := `{"version":4,"items":[{"slug":"gmux","match":[{"path":"~/dev/gmux"}],"sessions":["typed-slug"]}]}`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(v4), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := Load(dir, map[string]string{"typed-slug": "sess-resolved"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := fmt.Sprint(state.Items[0].Sessions); got != "[typed-slug]" {
		t.Errorf("sessions = %s, want [typed-slug]", got)
	}
}

func TestMigrateV1EmptyItems(t *testing.T) {
	// v1 data with no items at all.
	v1Data := `{"items": []}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(v1Data), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Version != currentVersion {
		t.Errorf("version = %d, want %d", state.Version, currentVersion)
	}
	if len(state.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(state.Items))
	}
}
