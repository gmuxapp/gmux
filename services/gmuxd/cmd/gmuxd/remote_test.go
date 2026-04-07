package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

func TestEnableTailscaleConfig_NewFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "host.toml")

	if err := enableTailscaleConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "[tailscale]") {
		t.Errorf("missing [tailscale] section in:\n%s", content)
	}
	if !strings.Contains(content, "enabled = true") {
		t.Errorf("missing enabled = true in:\n%s", content)
	}
}

func TestEnableTailscaleConfig_ExistingFileNoSection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "host.toml")
	os.WriteFile(cfgPath, []byte("port = 9999\n"), 0o644)

	if err := enableTailscaleConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(cfgPath)
	content := string(data)
	if !strings.Contains(content, "port = 9999") {
		t.Errorf("original content lost:\n%s", content)
	}
	if !strings.Contains(content, "[tailscale]") {
		t.Errorf("missing [tailscale] section:\n%s", content)
	}
	if !strings.Contains(content, "enabled = true") {
		t.Errorf("missing enabled = true:\n%s", content)
	}
}

func TestEnableTailscaleConfig_ExistingSectionDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "host.toml")
	os.WriteFile(cfgPath, []byte("[tailscale]\nenabled = false\nhostname = \"mybox\"\n"), 0o644)

	if err := enableTailscaleConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(cfgPath)
	content := string(data)
	if !strings.Contains(content, "enabled = true") {
		t.Errorf("enabled not set to true:\n%s", content)
	}
	if strings.Contains(content, "enabled = false") {
		t.Errorf("old enabled = false still present:\n%s", content)
	}
	if !strings.Contains(content, "hostname = \"mybox\"") {
		t.Errorf("hostname lost:\n%s", content)
	}
}

func TestEnableTailscaleConfig_ExistingSectionNoEnabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "host.toml")
	os.WriteFile(cfgPath, []byte("[tailscale]\nhostname = \"mybox\"\n\n[discovery]\ntailscale = true\n"), 0o644)

	if err := enableTailscaleConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(cfgPath)
	content := string(data)
	if !strings.Contains(content, "enabled = true") {
		t.Errorf("enabled not added:\n%s", content)
	}
	if !strings.Contains(content, "hostname = \"mybox\"") {
		t.Errorf("hostname lost:\n%s", content)
	}
	if !strings.Contains(content, "[discovery]") {
		t.Errorf("discovery section lost:\n%s", content)
	}
}

func TestDisplayStatus_NeedsLogin(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS: &tsHealth{
			AuthURL: "https://login.tailscale.com/a/abc123",
		},
	}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "https://login.tailscale.com/a/abc123") {
		t.Errorf("missing auth URL in:\n%s", out)
	}
	if !strings.Contains(out, "gmuxd remote") {
		t.Errorf("should tell user to run gmuxd remote again:\n%s", out)
	}
	// Should NOT mention HTTPS or MagicDNS problems.
	if strings.Contains(out, "HTTPS") || strings.Contains(out, "MagicDNS") {
		t.Errorf("should not mention HTTPS/MagicDNS before login:\n%s", out)
	}
}

func TestDisplayStatus_Connected(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS: &tsHealth{
			FQDN:      "gmux.tailnet.ts.net",
			Connected: true,
			HTTPS:     true,
			MagicDNS:  true,
		},
	}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "https://gmux.tailnet.ts.net") {
		t.Errorf("missing FQDN in:\n%s", out)
	}
	if !strings.Contains(out, "Remote access is active") {
		t.Errorf("missing active message:\n%s", out)
	}
}

func TestDisplayStatus_ConnectedMissingHTTPS(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS: &tsHealth{
			FQDN:      "gmux.tailnet.ts.net",
			Connected: true,
			HTTPS:     false,
			MagicDNS:  true,
		},
	}
	code := displayStatus(h, &stdout)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "HTTPS is not enabled") {
		t.Errorf("should warn about HTTPS:\n%s", out)
	}
	if !strings.Contains(out, "login.tailscale.com/admin/dns") {
		t.Errorf("should link to admin console:\n%s", out)
	}
}

func TestDisplayStatus_NotConnected(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS: &tsHealth{
			Connected: false,
		},
	}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "still connecting") {
		t.Errorf("should say still connecting:\n%s", out)
	}
	// Should NOT mention HTTPS or MagicDNS problems.
	if strings.Contains(out, "HTTPS") || strings.Contains(out, "MagicDNS") {
		t.Errorf("should not mention HTTPS/MagicDNS when not connected:\n%s", out)
	}
}

func TestRemoteSetup_UserDeclinesNoConfigChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := remoteSetup(defaultConfig(), stdin, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	// Config file should not have been created.
	cfgPath := filepath.Join(dir, "gmux", "host.toml")
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("config file should not exist after declining, err=%v", err)
	}
}

func TestRemoteSetup_ShowsExplanation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	remoteSetup(defaultConfig(), stdin, &stdout, &stderr)

	out := stdout.String()
	if !strings.Contains(out, "Tailscale") {
		t.Errorf("should mention Tailscale:\n%s", out)
	}
	if !strings.Contains(out, remoteDocsURL) {
		t.Errorf("should link to docs:\n%s", out)
	}
	if !strings.Contains(out, "[y/N]") {
		t.Errorf("should show confirmation prompt:\n%s", out)
	}
}

func defaultConfig() config.Config {
	return config.Config{Port: 8790}
}
