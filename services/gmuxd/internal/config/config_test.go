package config

import (
	"fmt"
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


// ── [[peers]] ──

func TestLoadPeers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[[peers]]
name = "server"
url = "http://10.0.0.5:8790"
token = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

[[peers]]
name = "dev-box"
url = "http://172.17.0.2:8790"
token = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Peers) != 2 {
		t.Fatalf("peers = %d, want 2", len(cfg.Peers))
	}
	if cfg.Peers[0].Name != "server" {
		t.Errorf("peers[0].name = %q, want %q", cfg.Peers[0].Name, "server")
	}
	if cfg.Peers[1].Name != "dev-box" {
		t.Errorf("peers[1].name = %q, want %q", cfg.Peers[1].Name, "dev-box")
	}
}

func TestLoadPeersRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[[peers]]
name = "server"
url = "http://10.0.0.5:8790"
token = "abc"

[[peers]]
name = "server"
url = "http://10.0.0.6:8790"
token = "def"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for duplicate peer name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want mention of duplicate", err)
	}
}

func TestLoadPeersRejectsInvalidName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"", "name is required"},
		{"Server", "lowercase slug"},
		{"my_server", "lowercase slug"},
		{"my@server", "lowercase slug"},
		{"-leading", "lowercase slug"},
		{"trailing-", "lowercase slug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, fmt.Sprintf(`
[[peers]]
name = %q
url = "http://10.0.0.5:8790"
token = "abc"
`, tt.name))

			_, err := Load()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestLoadPeersRejectsMissingURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[[peers]]
name = "server"
token = "abc"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("error = %q, want mention of url", err)
	}
}

func TestLoadPeersAcceptsNoToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[[peers]]
name = "server"
url = "https://mybox.tailnet.ts.net"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("tokenless peer should be accepted (tailscale auth): %v", err)
	}
	if cfg.Peers[0].Token != "" {
		t.Errorf("token = %q, want empty", cfg.Peers[0].Token)
	}
}

func TestLoadPeersRejectsMultipleTokenSources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[[peers]]
name = "server"
url = "http://10.0.0.5:8790"
token = "inline"
token_file = "/path/to/token"
`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for multiple token sources")
	}
	if !strings.Contains(err.Error(), "only one") {
		t.Errorf("error = %q, want mention of 'only one'", err)
	}
}

func TestLoadPeersTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("my-secret-token\n"), 0o600)

	writeConfig(t, dir, fmt.Sprintf(`
[[peers]]
name = "server"
url = "http://10.0.0.5:8790"
token_file = %q
`, tokenFile))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].TokenFile != tokenFile {
		t.Errorf("token_file = %q, want %q", cfg.Peers[0].TokenFile, tokenFile)
	}
}

func TestLoadPeersTokenCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[[peers]]
name = "server"
url = "http://10.0.0.5:8790"
token_command = "echo my-secret"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].TokenCommand != "echo my-secret" {
		t.Errorf("token_command = %q, want %q", cfg.Peers[0].TokenCommand, "echo my-secret")
	}
}

func TestResolveTokens_Inline(t *testing.T) {
	cfg := Config{
		Peers: []PeerConfig{{Name: "s", URL: "http://x:8790", Token: "abc"}},
	}
	if err := cfg.ResolveTokens(); err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].Token != "abc" {
		t.Errorf("token = %q, want %q", cfg.Peers[0].Token, "abc")
	}
}

func TestResolveTokens_File(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("  file-secret  \n"), 0o600)

	cfg := Config{
		Peers: []PeerConfig{{Name: "s", URL: "http://x:8790", TokenFile: tokenFile}},
	}
	if err := cfg.ResolveTokens(); err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].Token != "file-secret" {
		t.Errorf("token = %q, want %q", cfg.Peers[0].Token, "file-secret")
	}
}

func TestResolveTokens_FileExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Write a token file at $HOME/.config/gmux/tokens/server
	dir := filepath.Join(home, ".config", "gmux", "tokens")
	os.MkdirAll(dir, 0o700)
	absPath := filepath.Join(dir, "server")
	os.WriteFile(absPath, []byte("home-secret\n"), 0o600)

	cfg := Config{
		Peers: []PeerConfig{{
			Name:      "s",
			URL:       "http://x:8790",
			TokenFile: "~/.config/gmux/tokens/server",
		}},
	}
	if err := cfg.ResolveTokens(); err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].Token != "home-secret" {
		t.Errorf("token = %q, want %q", cfg.Peers[0].Token, "home-secret")
	}
}

func TestResolveTokens_Command(t *testing.T) {
	cfg := Config{
		Peers: []PeerConfig{{Name: "s", URL: "http://x:8790", TokenCommand: "echo cmd-secret"}},
	}
	if err := cfg.ResolveTokens(); err != nil {
		t.Fatal(err)
	}
	if cfg.Peers[0].Token != "cmd-secret" {
		t.Errorf("token = %q, want %q", cfg.Peers[0].Token, "cmd-secret")
	}
}

func TestResolveTokens_EmptyFileErrors(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("  \n"), 0o600)

	cfg := Config{
		Peers: []PeerConfig{{Name: "s", URL: "http://x:8790", TokenFile: tokenFile}},
	}
	err := cfg.ResolveTokens()
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want mention of empty", err)
	}
}

func TestResolveTokens_MissingFileErrors(t *testing.T) {
	cfg := Config{
		Peers: []PeerConfig{{Name: "s", URL: "http://x:8790", TokenFile: "/nonexistent/path"}},
	}
	err := cfg.ResolveTokens()
	if err == nil {
		t.Fatal("expected error for missing token file")
	}
}

func TestResolveTokens_FailedCommandErrors(t *testing.T) {
	cfg := Config{
		Peers: []PeerConfig{{Name: "s", URL: "http://x:8790", TokenCommand: "false"}},
	}
	err := cfg.ResolveTokens()
	if err == nil {
		t.Fatal("expected error for failed command")
	}
}

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

func TestLoadPeersRejectsInvalidURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"not-a-url", "http or https"},
		{"ftp://server:8790", "http or https"},
		{"http://", "no host"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, fmt.Sprintf(`
[[peers]]
name = "server"
url = %q
token = "abc"
`, tt.url))

			_, err := Load()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestLoadPeersAcceptsHTTPAndHTTPS(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, fmt.Sprintf(`
[[peers]]
name = "server"
url = "%s://10.0.0.5:8790"
token = "abc"
`, scheme))

			_, err := Load()
			if err != nil {
				t.Fatalf("scheme %s should be accepted: %v", scheme, err)
			}
		})
	}
}

func TestLoadNoPeersIsValid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `port = 8790`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Peers) != 0 {
		t.Errorf("peers = %d, want 0", len(cfg.Peers))
	}
}

func writeConfig(t *testing.T, xdgDir, content string) {
	t.Helper()
	cfgDir := filepath.Join(xdgDir, "gmux")
	os.MkdirAll(cfgDir, 0o755)
	os.WriteFile(filepath.Join(cfgDir, "host.toml"), []byte(content), 0o644)
}
