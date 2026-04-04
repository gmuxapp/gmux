package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8790 {
		t.Errorf("port = %d, want 8790", cfg.Port)
	}
	if cfg.Tailscale.Enabled {
		t.Error("tailscale should be disabled by default")
	}
	if cfg.Tailscale.Hostname != "gmux" {
		t.Errorf("hostname = %q, want %q", cfg.Tailscale.Hostname, "gmux")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 9999

[tailscale]
enabled = true
hostname = "mybox"
allow = ["alice@github", "bob@github"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9999 {
		t.Errorf("port = %d, want 9999", cfg.Port)
	}
	if !cfg.Tailscale.Enabled {
		t.Error("tailscale should be enabled")
	}
	if cfg.Tailscale.Hostname != "mybox" {
		t.Errorf("hostname = %q, want %q", cfg.Tailscale.Hostname, "mybox")
	}
	if len(cfg.Tailscale.Allow) != 2 {
		t.Fatalf("allow = %v, want 2 entries", cfg.Tailscale.Allow)
	}
}

func TestLoadFiltersEmptyAllowEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[tailscale]
enabled = true
allow = ["alice@github", "", "  ", "bob@github"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tailscale.Allow) != 2 {
		t.Fatalf("allow = %v, want 2 entries (empty strings filtered)", cfg.Tailscale.Allow)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 8790
[tailscale]
enabled = true
alow = ["user@github"]
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for unknown key 'alow'")
	}
	if !strings.Contains(err.Error(), "unknown keys") {
		t.Errorf("error = %q, want mention of unknown keys", err)
	}
}

func TestLoadRejectsInvalidPort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `port = 99999`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for out-of-range port")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %q, want mention of out of range", err)
	}
}

func TestLoadRejectsBadLoginFormat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[tailscale]
enabled = true
allow = ["not-a-login-name"]
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for bad login format")
	}
	if !strings.Contains(err.Error(), "doesn't look like a login name") {
		t.Errorf("error = %q", err)
	}
}

func TestLoadRejectsBadTOML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `{{invalid`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for bad TOML")
	}
}

// ── GMUXD_LISTEN validation (via ListenAddr) ──

func TestListenAddrEnvValidatesPrivateRanges(t *testing.T) {
	for _, addr := range []string{"10.0.0.1", "172.16.0.1", "192.168.0.1", "100.100.100.100", "0.0.0.0", "::", "fd12::1"} {
		t.Setenv("GMUXD_LISTEN", addr)
		_, err := defaults().ListenAddr()
		if err != nil {
			t.Errorf("address %q should be accepted: %v", addr, err)
		}
	}
}

func TestListenAddrEnvRejectsPublicIP(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "8.8.8.8")
	_, err := defaults().ListenAddr()
	if err == nil {
		t.Fatal("expected error for public IP")
	}
	if !strings.Contains(err.Error(), "public IP") {
		t.Errorf("error = %q", err)
	}
}

func TestListenAddrEnvRejectsInvalidIP(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "not-an-ip")
	_, err := defaults().ListenAddr()
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
}

func TestListenAddrEnvRejectsPublicIPv6(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "2001:db8::1")
	_, err := defaults().ListenAddr()
	if err == nil {
		t.Fatal("expected error for public IPv6")
	}
}

// ── ListenAddr ──

func TestListenAddrDefault(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "")
	addr, err := defaults().ListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:8790" {
		t.Errorf("addr = %q, want %q", addr, "127.0.0.1:8790")
	}
}

func TestListenAddrCustomPort(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "")
	cfg := defaults()
	cfg.Port = 9999

	addr, err := cfg.ListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:9999" {
		t.Errorf("addr = %q, want %q", addr, "127.0.0.1:9999")
	}
}

func TestListenAddrEnvOverride(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "10.0.0.99")
	addr, err := defaults().ListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.99:8790" {
		t.Errorf("addr = %q, want %q", addr, "10.0.0.99:8790")
	}
}

func TestListenAddrIPv6(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "fd12::1")
	addr, err := defaults().ListenAddr()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "[fd12::1]:8790" {
		t.Errorf("addr = %q, want %q", addr, "[fd12::1]:8790")
	}
}


func writeConfig(t *testing.T, xdgDir, content string) {
	t.Helper()
	cfgDir := filepath.Join(xdgDir, "gmux")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "host.toml"), []byte(content), 0o644)
}
