package tsauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsAllowed(t *testing.T) {
	l := &Listener{
		cfg: Config{
			Allow: []string{"alice@github", "bob@github"},
		},
	}

	tests := []struct {
		login string
		want  bool
	}{
		{"alice@github", true},    // exact match
		{"bob@github", true},      // exact match
		{"eve@github", false},     // no match
		{"Alice@GitHub", true},    // case-insensitive
		{"", false},               // empty
	}

	for _, tt := range tests {
		got := l.isAllowed(tt.login)
		if got != tt.want {
			t.Errorf("isAllowed(%q) = %v, want %v", tt.login, got, tt.want)
		}
	}
}

func TestIsAllowedEmptyList(t *testing.T) {
	l := &Listener{
		cfg: Config{Allow: nil},
	}

	if l.isAllowed("anyone@github") {
		t.Error("empty allow list should deny everyone")
	}
}

func TestResetStateIfHostnameChanged(t *testing.T) {
	dir := t.TempDir()
	tsnetDir := filepath.Join(dir, "tsnet")

	// First call: no existing state, creates sentinel.
	resetStateIfHostnameChanged(tsnetDir, "gmux")
	got, err := os.ReadFile(filepath.Join(tsnetDir, hostnameFile))
	if err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
	if string(got) != "gmux\n" {
		t.Errorf("sentinel = %q, want %q", got, "gmux\n")
	}

	// Simulate tsnet creating a state file.
	statePath := filepath.Join(tsnetDir, "tailscaled.state")
	if err := os.WriteFile(statePath, []byte("fake-state"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Same hostname: state preserved.
	resetStateIfHostnameChanged(tsnetDir, "gmux")
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file should still exist after same-hostname call: %v", err)
	}

	// Different hostname: state directory wiped and re-created.
	resetStateIfHostnameChanged(tsnetDir, "gmux-hs")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should be gone after hostname change, got err=%v", err)
	}
	got, err = os.ReadFile(filepath.Join(tsnetDir, hostnameFile))
	if err != nil {
		t.Fatalf("sentinel not re-created: %v", err)
	}
	if string(got) != "gmux-hs\n" {
		t.Errorf("sentinel = %q, want %q", got, "gmux-hs\n")
	}
}

func TestAddIfMissing(t *testing.T) {
	// Adds when not present.
	list := addIfMissing(nil, "alice@github")
	if len(list) != 1 || list[0] != "alice@github" {
		t.Errorf("got %v", list)
	}

	// Doesn't duplicate (exact case).
	list = addIfMissing([]string{"alice@github"}, "alice@github")
	if len(list) != 1 {
		t.Errorf("got %v, want no duplicate", list)
	}

	// Doesn't duplicate (case-insensitive).
	list = addIfMissing([]string{"Alice@GitHub"}, "alice@github")
	if len(list) != 1 {
		t.Errorf("got %v, want no duplicate (case-insensitive)", list)
	}

	// Adds different user.
	list = addIfMissing([]string{"alice@github"}, "bob@github")
	if len(list) != 2 {
		t.Errorf("got %v, want 2 entries", list)
	}
}
