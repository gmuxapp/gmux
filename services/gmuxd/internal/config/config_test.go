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
	if !cfg.Discovery.Devcontainers {
		t.Error("discovery.devcontainers should default to true")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 9999

[tailscale]
enabled = true
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

// Removed ADR 0007 keys must not brick a daemon on upgrade: they are
// ignored with a deprecation warning, and the rest of the config still
// loads normally.
func TestLoadIgnoresRemovedTailscaleHostname(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 8123
[tailscale]
enabled = true
hostname = "project-a"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("deprecated tailscale.hostname should be ignored, got error: %v", err)
	}
	if cfg.Port != 8123 || !cfg.Tailscale.Enabled {
		t.Errorf("rest of config should still load, got %+v", cfg)
	}
}

// discovery.tailscale was removed in ADR 0008 (tailscale autodiscovery
// deleted). A host upgrading with the old key set must keep loading, not
// brick on an "unknown key" error.
func TestLoadIgnoresRemovedDiscoveryTailscale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 8125
[discovery]
tailscale = false
devcontainers = true
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("deprecated discovery.tailscale should be ignored, got error: %v", err)
	}
	if cfg.Port != 8125 || !cfg.Discovery.Devcontainers {
		t.Errorf("rest of config should still load, got %+v", cfg)
	}
}

func TestLoadIgnoresRemovedPeers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 8124
[[peers]]
name = "server"
url = "https://gmux-server.ts.net"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("deprecated [[peers]] should be ignored, got error: %v", err)
	}
	if cfg.Port != 8124 {
		t.Errorf("rest of config should still load, got port %d", cfg.Port)
	}
}

// A genuinely unknown key (e.g. a typo) must still fail loudly, even
// when it appears alongside a tolerated deprecated key.
func TestLoadRejectsRemovedKeyMixedWithTypo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[tailscale]
enabled = true
hostname = "project-a"
alow = ["user@github"]
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for unknown key 'alow' even alongside a deprecated key")
	}
	if !strings.Contains(err.Error(), "unknown keys") || strings.Contains(err.Error(), "hostname") {
		t.Errorf("error = %q, want unknown-keys error mentioning only the typo", err)
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

func TestLoadAcceptsDeviceTags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[tailscale]
enabled = true
allow = ["alice@github", "tag:gmux"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tailscale.Allow) != 2 {
		t.Fatalf("allow = %v, want 2 entries", cfg.Tailscale.Allow)
	}
}

func TestLoadRejectsMalformedTags(t *testing.T) {
	bad := []string{
		"tag:",           // empty name
		"tag:my tag",     // whitespace
		"tag:tag:double", // nested prefix
		"tag:GMux",       // uppercase
		"tag:1abc",       // must start with a letter
		"tag:-abc",       // must start with a letter
	}
	for _, entry := range bad {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, `
[tailscale]
enabled = true
allow = ["`+entry+`"]
`)

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for malformed tag %q", entry)
			}
			if !strings.Contains(err.Error(), "not a valid device tag") {
				t.Errorf("error = %q", err)
			}
		})
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


// ── [[peers]] ──

func TestLoadDiscoveryDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, ``)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Discovery.Devcontainers {
		t.Error("discovery.devcontainers should default to true")
	}
}

func TestLoadDiscoveryExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[discovery]
devcontainers = false
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discovery.Devcontainers {
		t.Error("discovery.devcontainers should be false when explicitly disabled")
	}
}

func writeConfig(t *testing.T, xdgDir, content string) {
	t.Helper()
	cfgDir := filepath.Join(xdgDir, "gmux")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "host.toml"), []byte(content), 0o644)
}
