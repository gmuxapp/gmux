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
	if len(cfg.Tailscale.Allow) != 0 {
		t.Errorf("allow = %v, want empty", cfg.Tailscale.Allow)
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
	if cfg.Tailscale.Allow[0] != "alice@github" {
		t.Errorf("allow[0] = %q", cfg.Tailscale.Allow[0])
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
	if cfg.Tailscale.Allow[0] != "alice@github" || cfg.Tailscale.Allow[1] != "bob@github" {
		t.Errorf("allow = %v", cfg.Tailscale.Allow)
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

func TestLoadAllowsEnabledWithEmptyAllow(t *testing.T) {
	// Empty allow list is valid — the node owner is auto-whitelisted at runtime.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[tailscale]
enabled = true
hostname = "gmux"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("empty allow with enabled should be valid (owner auto-added at runtime): %v", err)
	}
	if !cfg.Tailscale.Enabled {
		t.Error("should be enabled")
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

func TestDisabledTailscaleAllowsNoAllowList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[tailscale]
enabled = false
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("disabled tailscale should not require allow list: %v", err)
	}
	if cfg.Tailscale.Enabled {
		t.Error("should be disabled")
	}
}

// ── Network listener tests ──

func TestNetworkListenPrivateIP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "192.168.1.100"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Listen != "192.168.1.100" {
		t.Errorf("listen = %q, want %q", cfg.Network.Listen, "192.168.1.100")
	}
}

func TestNetworkListenAllInterfaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "0.0.0.0"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Listen != "0.0.0.0" {
		t.Errorf("listen = %q, want %q", cfg.Network.Listen, "0.0.0.0")
	}
}

func TestNetworkListenRejectsPublicIP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "8.8.8.8"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for public IP")
	}
	if !strings.Contains(err.Error(), "public IP") {
		t.Errorf("error = %q, want mention of public IP", err)
	}
}

func TestNetworkListenRejectsLoopback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "127.0.0.1"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for loopback address")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %q, want mention of loopback", err)
	}
}

func TestNetworkListenRejectsInvalidIP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "not-an-ip"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
	if !strings.Contains(err.Error(), "not a valid IP") {
		t.Errorf("error = %q, want mention of invalid IP", err)
	}
}

func TestNetworkListenCGNAT(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "100.100.100.100"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("CGNAT address should be accepted: %v", err)
	}
	if cfg.Network.Listen != "100.100.100.100" {
		t.Errorf("listen = %q", cfg.Network.Listen)
	}
}

func TestNetworkListenRFC1918Ranges(t *testing.T) {
	for _, addr := range []string{"10.0.0.1", "172.16.0.1", "172.31.255.1", "192.168.0.1"} {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		writeConfig(t, dir, `
[network]
listen = "`+addr+`"
`)
		_, err := Load()
		if err != nil {
			t.Errorf("RFC 1918 address %q should be accepted: %v", addr, err)
		}
	}
}

func TestResolveNetworkListenDefaults(t *testing.T) {
	cfg := defaults()
	addr, err := cfg.ResolveNetworkListen()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "" {
		t.Errorf("expected empty, got %q", addr)
	}
}

func TestResolveNetworkListenAddsPort(t *testing.T) {
	cfg := defaults()
	cfg.Network.Listen = "10.0.0.5"

	addr, err := cfg.ResolveNetworkListen()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.5:8790" {
		t.Errorf("addr = %q, want %q", addr, "10.0.0.5:8790")
	}
}

func TestResolveNetworkListenPreservesPort(t *testing.T) {
	cfg := defaults()
	cfg.Network.Listen = "10.0.0.5:9999"

	addr, err := cfg.ResolveNetworkListen()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.5:9999" {
		t.Errorf("addr = %q, want %q", addr, "10.0.0.5:9999")
	}
}

func TestResolveNetworkListenEnvOverride(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "10.0.0.99")

	cfg := defaults()
	addr, err := cfg.ResolveNetworkListen()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.99:8790" {
		t.Errorf("addr = %q, want %q", addr, "10.0.0.99:8790")
	}
}

func TestResolveNetworkListenEnvOverridesConfig(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "10.0.0.99")

	cfg := defaults()
	cfg.Network.Listen = "192.168.1.1"

	addr, err := cfg.ResolveNetworkListen()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.99:8790" {
		t.Errorf("env should override config: addr = %q, want %q", addr, "10.0.0.99:8790")
	}
}

func TestResolveNetworkListenEnvValidation(t *testing.T) {
	t.Setenv("GMUXD_LISTEN", "8.8.8.8")

	cfg := defaults()
	_, err := cfg.ResolveNetworkListen()
	if err == nil {
		t.Fatal("expected error for public IP via env var")
	}
}

func TestNetworkListenIPv6Unspecified(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "::"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("IPv6 unspecified (::) should be accepted: %v", err)
	}
	if cfg.Network.Listen != "::" {
		t.Errorf("listen = %q, want %q", cfg.Network.Listen, "::")
	}
}

func TestNetworkListenIPv6ULA(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "fd12::1"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("IPv6 ULA address should be accepted: %v", err)
	}
	if cfg.Network.Listen != "fd12::1" {
		t.Errorf("listen = %q", cfg.Network.Listen)
	}
}

func TestNetworkListenIPv6Loopback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "::1"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for IPv6 loopback")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %q, want mention of loopback", err)
	}
}

func TestNetworkListenIPv6Public(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "2001:db8::1"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for public IPv6 address")
	}
	if !strings.Contains(err.Error(), "public IP") {
		t.Errorf("error = %q, want mention of public IP", err)
	}
}

func TestResolveNetworkListenIPv6AddsPort(t *testing.T) {
	cfg := defaults()
	cfg.Network.Listen = "fd12::1"

	addr, err := cfg.ResolveNetworkListen()
	if err != nil {
		t.Fatal(err)
	}
	// IPv6 addresses must be bracketed when combined with a port.
	if addr != "[fd12::1]:8790" {
		t.Errorf("addr = %q, want %q", addr, "[fd12::1]:8790")
	}
}

func TestNetworkListenWithPort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[network]
listen = "10.0.0.5:9999"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Listen != "10.0.0.5:9999" {
		t.Errorf("listen = %q", cfg.Network.Listen)
	}
}

func writeConfig(t *testing.T, xdgDir, content string) {
	t.Helper()
	cfgDir := filepath.Join(xdgDir, "gmux")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(content), 0o644)
}
