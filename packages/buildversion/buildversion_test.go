package buildversion

import "testing"

func TestIsDev(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"dev", true},
		{"dev+1a2b3c4d5e6f", true},
		{"dev+1a2b3c4d5e6f-dirty", true},
		{"v2.1.1", false},
		{"2.1.2-snapshot-abcdef", false},
		{"", false},
		{"development", false},
		{"devel", false},
	} {
		if got := IsDev(tc.in); got != tc.want {
			t.Errorf("IsDev(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSameBuildClass(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"dev", "dev", true},
		{"dev+aaaaaaaaaaaa", "dev+bbbbbbbbbbbb", true},   // rebuild, same class
		{"dev", "dev+aaaaaaaaaaaa-dirty", true},          // stamped vs unstamped
		{"v2.1.1", "v2.1.1", true},
		{"v2.1.1", "v2.1.0", false},                      // real upgrade
		{"dev+aaaaaaaaaaaa", "v2.1.1", false},            // dev vs release
		{"v2.1.1", "dev", false},
	} {
		if got := SameBuildClass(tc.a, tc.b); got != tc.want {
			t.Errorf("SameBuildClass(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
