package identity

import "testing"

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		fqdn     string
		osHost   string
		want     string
	}{
		{"tailscale fqdn wins over os hostname", "gmux-laptop.tailnet.ts.net", "ca75413aec31", "gmux-laptop"},
		{"fqdn without dot used as-is", "gmux-laptop", "host", "gmux-laptop"},
		{"no fqdn falls back to os hostname", "", "my-server", "my-server"},
		{"empty both", "", "", ""},
		{"leading-dot fqdn falls back (no usable label)", ".weird", "host", "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.fqdn, tt.osHost); got != tt.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", tt.fqdn, tt.osHost, got, tt.want)
			}
		})
	}
}
