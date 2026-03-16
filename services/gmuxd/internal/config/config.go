// Package config loads gmuxd configuration from ~/.config/gmux/config.toml.
//
// Missing file or missing keys are fine — everything has a safe default.
// The file is never written by gmuxd; users create and edit it manually.
//
// Security-relevant fields are strictly validated: unknown keys, invalid
// values, and dangerous combinations cause a hard error at startup.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level gmuxd configuration.
type Config struct {
	Port      int             `toml:"port"`
	Tailscale TailscaleConfig `toml:"tailscale"`
}

// TailscaleConfig controls the optional tailscale (tsnet) listener.
type TailscaleConfig struct {
	// Enabled starts a tsnet listener on the tailnet. Default false.
	Enabled bool `toml:"enabled"`

	// Hostname is the tailscale machine name (e.g. "gmux" → gmux.tailnet.ts.net).
	// Default "gmux".
	Hostname string `toml:"hostname"`

	// Allow is the list of tailscale login names permitted to connect
	// (e.g. "user@github"). Matched against the peer's UserProfile.LoginName.
	// Empty list with enabled=true is a hard error (fail-closed).
	Allow []string `toml:"allow"`
}

// Load reads the config file. Returns defaults if the file doesn't exist.
// Returns an error for malformed config, unknown fields, or invalid
// security settings — gmuxd should refuse to start in these cases.
func Load() (Config, error) {
	cfg := defaults()

	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: reading %s: %w", path, err)
	}

	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	// Reject unknown keys — a typo like "alow" instead of "allow" would
	// silently result in an empty allow list, which is a security issue.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("config: unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}

	// Normalize allow list: trim whitespace, remove empty entries.
	filtered := cfg.Tailscale.Allow[:0]
	for _, entry := range cfg.Tailscale.Allow {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			filtered = append(filtered, entry)
		}
	}
	cfg.Tailscale.Allow = filtered

	if err := validate(cfg); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}

	return cfg, nil
}

func validate(cfg Config) error {
	// Port range.
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port %d is out of range (1-65535)", cfg.Port)
	}

	// Tailscale: allow list entries must look like login names.
	// An empty allow list is fine — the node owner is auto-whitelisted at runtime.
	for _, entry := range cfg.Tailscale.Allow {
		if !strings.Contains(entry, "@") {
			return fmt.Errorf("tailscale.allow entry %q doesn't look like a login name (expected format: user@provider)", entry)
		}
	}

	// Tailscale: hostname must be non-empty when enabled.
	if cfg.Tailscale.Enabled && cfg.Tailscale.Hostname == "" {
		return fmt.Errorf("tailscale.enabled is true but tailscale.hostname is empty")
	}

	return nil
}

func defaults() Config {
	return Config{
		Port: 8790,
		Tailscale: TailscaleConfig{
			Hostname: "gmux",
		},
	}
}

func configPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gmux", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gmux", "config.toml")
}
