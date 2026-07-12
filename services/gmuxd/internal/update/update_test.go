package update

import "testing"

func TestNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v0.5.0", "v0.4.6", true},
		{"v0.4.7", "v0.4.6", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.4.6", "v0.4.6", false},
		{"v0.4.5", "v0.4.6", false},
		{"v0.3.0", "v0.4.6", false},
		{"0.5.0", "0.4.6", true}, // no prefix
		{"dev", "v0.4.6", false}, // unparseable
		{"v0.4.6", "dev", false}, // unparseable
	}
	for _, tt := range tests {
		got := newer(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"1.2.3", true},
		{"0.0.0", true},
		{"1.2", false},
		{"abc", false},
		{"1.2.3-beta", false},
	}
	for _, tt := range tests {
		got := parseSemver(tt.in)
		if (got != nil) != tt.want {
			t.Errorf("parseSemver(%q): got nil=%v, want valid=%v", tt.in, got == nil, tt.want)
		}
	}
}
