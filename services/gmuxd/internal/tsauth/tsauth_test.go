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

func TestLoadOrSeedHostname_SeedsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	tsnetDir := filepath.Join(dir, "tsnet")

	name := loadOrSeedHostname(tsnetDir, "gmux-aquilo")
	if name != "gmux-aquilo" {
		t.Errorf("name = %q, want %q", name, "gmux-aquilo")
	}
	got, err := os.ReadFile(filepath.Join(tsnetDir, hostnameFile))
	if err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
	if string(got) != "gmux-aquilo\n" {
		t.Errorf("sentinel = %q, want %q", got, "gmux-aquilo\n")
	}
}

// The recorded name is kept verbatim and tsnet state is never wiped, even
// when the seed differs — tailscale owns the identity (ADR 0007).
func TestLoadOrSeedHostname_KeepsExistingNeverWipes(t *testing.T) {
	dir := t.TempDir()
	tsnetDir := filepath.Join(dir, "tsnet")
	if err := os.MkdirAll(tsnetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tsnetDir, hostnameFile), []byte("project-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tsnetDir, "tailscaled.state")
	if err := os.WriteFile(statePath, []byte("existing-keys"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A different seed must NOT rename or wipe.
	name := loadOrSeedHostname(tsnetDir, "gmux-aquilo")
	if name != "project-a" {
		t.Errorf("name = %q, want kept %q", name, "project-a")
	}
	data, err := os.ReadFile(statePath)
	if err != nil || string(data) != "existing-keys" {
		t.Fatalf("state file must survive untouched, got %q err=%v", data, err)
	}
}

func TestSeedName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Aquilo", "gmux-aquilo"},
		{"my.box", "gmux-my-box"},
		{"ca75413aec31", "gmux-ca75413aec31"},
		{"", "gmux"},
		{"---", "gmux"},
	}
	for _, tt := range tests {
		if got := seedName(tt.in); got != tt.want {
			t.Errorf("seedName(%q) = %q, want %q", tt.in, got, tt.want)
		}
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
