package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if got := fmt.Sprint(cfg.Agent.MaxSubagentsByDepth.Values); got != "[-1 8]" || cfg.Agent.MaxSubagentsByDepth.Disabled {
		t.Errorf("max_subagents_by_depth = %+v, want [-1 8]", cfg.Agent.MaxSubagentsByDepth)
	}
	if cfg.Tailscale.Enabled {
		t.Error("tailscale should be disabled by default")
	}
	if !cfg.Discovery.Devcontainers {
		t.Error("discovery.devcontainers should default to true")
	}
	if cfg.Sessions.RetentionDays != 30 || cfg.Sessions.RetentionMax != 200 || cfg.Sessions.ScrollbackCacheMB != 256 {
		t.Errorf("sessions defaults = %+v", cfg.Sessions)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
port = 9999

[agent]
max_subagents_by_depth = [64, 12]

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
	if got := fmt.Sprint(cfg.Agent.MaxSubagentsByDepth.Values); got != "[64 12]" {
		t.Errorf("max_subagents_by_depth = %v, want [64 12]", cfg.Agent.MaxSubagentsByDepth.Values)
	}
	if !cfg.Tailscale.Enabled {
		t.Error("tailscale should be enabled")
	}
	if len(cfg.Tailscale.Allow) != 2 {
		t.Fatalf("allow = %v, want 2 entries", cfg.Tailscale.Allow)
	}
}

func TestLoadValidatesMaxSubagentsByDepth(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        string
		wantOff     bool
		wantErr     bool
	}{
		{name: "default shape", value: "[-1, 8]", want: "[-1 8]"},
		{name: "finite root", value: "[64, 8, 0]", want: "[64 8 0]"},
		{name: "strict", value: "[0]", want: "[0]"},
		{name: "disabled", value: "false", want: "[]", wantOff: true},
		{name: "true", value: "true", wantErr: true},
		{name: "empty", value: "[]", wantErr: true},
		{name: "scalar", value: "8", wantErr: true},
		{name: "negative after first", value: "[8, -1]", wantErr: true},
		{name: "too large", value: "[-1, 1025]", wantErr: true},
		{name: "too deep", value: "[1,2,3,4,5,6,7,8,9]", wantErr: true},
		{name: "wrong entry type", value: `[-1, "eight"]`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, "[agent]\nmax_subagents_by_depth = "+tt.value+"\n")
			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = %+v, want error", cfg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(cfg.Agent.MaxSubagentsByDepth.Values); got != tt.want || cfg.Agent.MaxSubagentsByDepth.Disabled != tt.wantOff {
				t.Fatalf("got %+v, want values=%s disabled=%v", cfg.Agent.MaxSubagentsByDepth, tt.want, tt.wantOff)
			}
		})
	}
}

func TestLoadNtfy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `
[notifications.ntfy]
enabled = true
server_url = "https://ntfy.example.net"
topic = "gmux_Q7f9x2mP4vN8kL3s"
token = "secret-token"
priority = 4
tags = ["gmux", "white_check_mark"]
click_url = "https://gmux.example.net/"
timeout = "7s"
`)
	if err := os.Chmod(Path(), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Notifications.Ntfy
	if !got.Enabled || got.ServerURL != "https://ntfy.example.net" || got.Topic != "gmux_Q7f9x2mP4vN8kL3s" || got.Token != "secret-token" {
		t.Fatalf("ntfy identity/auth not loaded: %+v", got)
	}
	if got.Priority != 4 || time.Duration(got.Timeout) != 7*time.Second || strings.Join(got.Tags, ",") != "gmux,white_check_mark" || got.ClickURL != "https://gmux.example.net/" {
		t.Fatalf("ntfy options not loaded: %+v", got)
	}
}

func TestLoadNtfyDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Notifications.Ntfy
	if got.Enabled || got.ServerURL != "https://ntfy.sh" || got.Priority != 3 || time.Duration(got.Timeout) != 5*time.Second {
		t.Fatalf("ntfy defaults = %+v", got)
	}
}

func TestLoadNtfyValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"missing topic", `enabled = true`, "topic is required"},
		{"bad topic", `topic = "spaces are bad"`, "topic must contain"},
		{"server path", `server_url = "https://example.net/ntfy"`, "must not contain a path"},
		{"server userinfo", `server_url = "https://user:pass@example.net"`, "must not contain userinfo"},
		{"bad scheme", `server_url = "ftp://example.net"`, "must use HTTP or HTTPS"},
		{"mixed auth", "token = \"tok\"\nusername = \"user\"\npassword = \"pass\"", "cannot be combined"},
		{"half basic auth", `username = "user"`, "must be set together"},
		{"plaintext bearer", "server_url = \"http://example.net\"\ntoken = \"tok\"", "credentials require HTTPS"},
		{"bad priority", `priority = 6`, "priority must be between"},
		{"bad tag", `tags = ["has,comma"]`, "tags must contain only"},
		{"bad timeout", `timeout = "500ms"`, "timeout must be between"},
		{"bad click", `click_url = "javascript:alert(1)"`, "absolute HTTP(S)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, "[notifications.ntfy]\n"+tt.body+"\n")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadNtfyEnabledRequiresPrivateConfig(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Unix permission semantics")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, "[notifications.ntfy]\nenabled = true\ntopic = \"secret_topic\"\n")
			if err := os.Chmod(Path(), mode); err != nil {
				t.Fatal(err)
			}

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "permissions 0600") {
				t.Fatalf("Load() error = %v, want private-permissions error", err)
			}
		})
	}
}

func TestLoadNtfyDisabledDoesNotRequirePrivateConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, "[notifications.ntfy]\nenabled = false\ntopic = \"secret_topic\"\n")
	if _, err := Load(); err != nil {
		t.Fatalf("disabled ntfy config should not require mode 0600: %v", err)
	}
}
func TestLoadSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `[sessions]
retention_days = 7
retention_max = 42
scrollback_cache_mb = 512
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sessions.RetentionDays != 7 || cfg.Sessions.RetentionMax != 42 || cfg.Sessions.ScrollbackCacheMB != 512 {
		t.Errorf("sessions = %+v", cfg.Sessions)
	}
}

func TestLoadValidatesSessions(t *testing.T) {
	maxDays := effectiveIntMax(math.MaxInt64 / int64(24*time.Hour))
	maxMB := effectiveIntMax(int64(math.MaxInt64 >> 20))
	maxInt := int64(^uint(0) >> 1)
	zero := SessionsConfig{}

	tests := []struct {
		name         string
		body         string
		wantErr      bool
		wantSessions *SessionsConfig
	}{
		{"explicit zero", "retention_days = 0\nretention_max = 0\nscrollback_cache_mb = 0", false, &zero},
		{"negative retention days", "retention_days = -1", true, nil},
		{"negative retention max", "retention_max = -1", true, nil},
		{"negative scrollback cache", "scrollback_cache_mb = -1", true, nil},
		{"maximum retention days", fmt.Sprintf("retention_days = %d", maxDays), false, nil},
		{"retention days maximum plus one", fmt.Sprintf("retention_days = %d", maxDays+1), true, nil},
		{"maximum retention count", fmt.Sprintf("retention_max = %d", maxInt), false, nil},
		{"maximum scrollback cache", fmt.Sprintf("scrollback_cache_mb = %d", maxMB), false, nil},
		{"scrollback cache maximum plus one", fmt.Sprintf("scrollback_cache_mb = %d", maxMB+1), true, nil},
		{"TOML integer decode overflow", "retention_max = 9223372036854775808", true, nil},
	}
	if maxInt < math.MaxInt64 {
		tests = append(tests, struct {
			name         string
			body         string
			wantErr      bool
			wantSessions *SessionsConfig
		}{"retention count maximum plus one", fmt.Sprintf("retention_max = %d", maxInt+1), true, nil})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			writeConfig(t, dir, "[sessions]\n"+tt.body+"\n")

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded with sessions %+v, want error", cfg.Sessions)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if tt.wantSessions != nil && cfg.Sessions != *tt.wantSessions {
				t.Errorf("sessions = %+v, want %+v", cfg.Sessions, *tt.wantSessions)
			}
		})
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
	if cfg.Sessions.RetentionDays != 30 || cfg.Sessions.RetentionMax != 200 || cfg.Sessions.ScrollbackCacheMB != 256 {
		t.Errorf("sessions defaults = %+v", cfg.Sessions)
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

// A host.toml written against the unreleased top-level spelling must name its
// new home rather than silently reverting the host to default budgets.
func TestLoadRejectsTopLevelMaxSubagentsByDepth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, "max_subagents_by_depth = [4, 4]\n")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error, want a migration error naming [agent]")
	}
	if !strings.Contains(err.Error(), "[agent]") {
		t.Errorf("error = %q, want it to name the [agent] section", err)
	}
}
