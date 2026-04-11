package peering

import (
	"errors"
	"testing"
)

func TestCategorizeError(t *testing.T) {
	tests := []struct {
		err  string
		want string
	}{
		{"connect: auth failed (HTTP 401)", "authentication failed"},
		{"connect: dial tcp 127.0.0.1:8790: connect: connection refused", "connection refused"},
		{"connect: dial tcp: lookup bad.host: no such host", "host not found"},
		{"connect: context deadline exceeded", "connection timed out"},
		{"connect: dial tcp 10.0.0.1:443: i/o timeout", "connection timed out"},
		{"connect: tls: failed to verify certificate", "TLS certificate error"},
		{"connect: x509: certificate signed by unknown authority", "TLS certificate error"},
		{"no data received", "no data received"},
		{"stream ended", "connection lost"},
		{"read: unexpected EOF", "connection failed"},
	}
	for _, tt := range tests {
		got := categorizeError(errors.New(tt.err))
		if got != tt.want {
			t.Errorf("categorizeError(%q) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
