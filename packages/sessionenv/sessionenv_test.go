package sessionenv

import (
	"slices"
	"testing"
)

func TestStrip(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"GMUX=1",
		"GMUX_SOCKET=/tmp/gmux-sessions/sess-abc.sock",
		"GMUX_SESSION_ID=sess-abc",
		"GMUX_ADAPTER=pi",
		"GMUX_RUNNER_VERSION=dev",
		"GMUX_RESUME_ID=sess-abc",
		"GMUX_HANDSHAKE_FD=3",
		"GMUX_SOCKET_DIR=/tmp/custom",
		"GMUXD_LISTEN=0.0.0.0",
		"GMUXD_TOKEN=secret",
		"HOME=/home/me",
	}
	want := []string{
		"PATH=/usr/bin",
		"GMUX_SOCKET_DIR=/tmp/custom",
		"GMUXD_LISTEN=0.0.0.0",
		"GMUXD_TOKEN=secret",
		"HOME=/home/me",
	}
	got := Strip(in)
	if !slices.Equal(got, want) {
		t.Errorf("Strip()\n got: %v\nwant: %v", got, want)
	}
}

// A bare GMUX with no '=' (malformed but possible) is still treated as
// the identity marker and stripped.
func TestStripBareKeyNoValue(t *testing.T) {
	got := Strip([]string{"GMUX", "GMUX_SOCKET", "KEEP=1"})
	want := []string{"KEEP=1"}
	if !slices.Equal(got, want) {
		t.Errorf("Strip()\n got: %v\nwant: %v", got, want)
	}
}

// GMUXD_* must never be mistaken for a GMUX_ session var: the prefix
// check keys on "GMUX_" and "GMUXD_..." does not match it.
func TestStripPreservesDaemonConfig(t *testing.T) {
	got := Strip([]string{"GMUXD_DEV_PROXY=http://x", "GMUX_SESSION_ID=s"})
	want := []string{"GMUXD_DEV_PROXY=http://x"}
	if !slices.Equal(got, want) {
		t.Errorf("Strip()\n got: %v\nwant: %v", got, want)
	}
}
