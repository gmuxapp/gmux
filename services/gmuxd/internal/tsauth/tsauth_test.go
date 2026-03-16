package tsauth

import "testing"

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
