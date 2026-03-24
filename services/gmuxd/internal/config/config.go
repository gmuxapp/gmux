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
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level gmuxd configuration.
type Config struct {
	Port      int             `toml:"port"`
	Network   NetworkConfig   `toml:"network"`
	Tailscale TailscaleConfig `toml:"tailscale"`
}

// NetworkConfig controls the optional network listener.
// This binds gmuxd to an additional address beyond localhost, protected
// by a bearer token. Intended for use behind a VPN or in containers,
// not for direct exposure to untrusted networks.
type NetworkConfig struct {
	// Listen is the address to bind the network listener to
	// (e.g. "10.0.0.5", "0.0.0.0"). Empty means disabled.
	// The port from the top-level config is used unless overridden here
	// with "addr:port" syntax.
	Listen string `toml:"listen"`
}

// TailscaleConfig controls the optional tailscale (tsnet) listener.
type TailscaleConfig struct {
	// Enabled starts a tsnet listener on the tailnet. Default false.
	Enabled bool `toml:"enabled"`

	// Hostname is the tailscale machine name (e.g. "gmuxd" → gmuxd.tailnet.ts.net).
	// Default "gmuxd".
	Hostname string `toml:"hostname"`

	// Allow is the list of additional tailscale login names permitted to connect
	// (e.g. "user@github"). The node owner is always auto-whitelisted at runtime.
	// Entries are matched against the peer's UserProfile.LoginName.
	Allow []string `toml:"allow"`
}

// Load reads the config file. Returns defaults if the file doesn't exist.
// Returns an error for malformed config, unknown fields, or invalid
// security settings — gmuxd should refuse to start in these cases.
func Load() (Config, error) {
	cfg := defaults()

	path := Path()
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

	// Network listener validation.
	if cfg.Network.Listen != "" {
		if err := validateNetworkListen(cfg.Network.Listen); err != nil {
			return fmt.Errorf("network.listen: %w", err)
		}
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

// validateNetworkListen checks that the listen address is safe to bind to.
// Accepts: RFC 1918 (10/8, 172.16/12, 192.168/16), link-local (169.254/16),
// CGNAT (100.64/10, used by Tailscale/WireGuard), Docker bridge (172.17/16
// falls under 172.16/12), unspecified (0.0.0.0 / ::, for containers), and
// IPv6 ULA (fd00::/8).
// Rejects: loopback (use the default listener), public IPs (use Tailscale).
func validateNetworkListen(addr string) error {
	// Strip port if present (e.g. "10.0.0.5:9999").
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%q is not a valid IP address", addr)
	}

	// Allow 0.0.0.0 / :: (all interfaces) for container use.
	if ip.IsUnspecified() {
		return nil
	}

	// Reject loopback.
	if ip.IsLoopback() {
		return fmt.Errorf("%q is a loopback address (the default listener already binds to localhost)", addr)
	}

	// Allow private, link-local, and CGNAT ranges.
	if isPrivateOrCGNAT(ip) {
		return nil
	}

	return fmt.Errorf("%q is a public IP address; use Tailscale for internet-facing access, or bind to a private/VPN address", addr)
}

// isPrivateOrCGNAT returns true for RFC 1918, link-local, and CGNAT (100.64/10) addresses.
func isPrivateOrCGNAT(ip net.IP) bool {
	// net.IP.IsPrivate covers RFC 1918 + RFC 4193 (IPv6 ULA).
	if ip.IsPrivate() {
		return true
	}
	// Link-local (169.254.0.0/16 for IPv4, fe80::/10 for IPv6).
	if ip.IsLinkLocalUnicast() {
		return true
	}
	// CGNAT range 100.64.0.0/10 (used by Tailscale, some WireGuard setups).
	cgnat := net.IPNet{
		IP:   net.ParseIP("100.64.0.0"),
		Mask: net.CIDRMask(10, 32),
	}
	if cgnat.Contains(ip) {
		return true
	}
	return false
}

func defaults() Config {
	return Config{
		Port: 8790,
		Tailscale: TailscaleConfig{
			Hostname: "gmux",
		},
	}
}

// ResolveNetworkListen returns the effective network listen address,
// considering both the config file and the GMUXD_LISTEN env var.
// The env var takes precedence. Returns "" if network listening is disabled.
// The returned address always includes a port (defaults to cfg.Port).
func (cfg Config) ResolveNetworkListen() (string, error) {
	listen := cfg.Network.Listen
	if env := os.Getenv("GMUXD_LISTEN"); env != "" {
		listen = env
	}
	if listen == "" {
		return "", nil
	}

	// Validate the address (env var bypasses config validation).
	if err := validateNetworkListen(listen); err != nil {
		return "", err
	}

	// Ensure the address includes a port.
	if _, _, err := net.SplitHostPort(listen); err != nil {
		// No port specified; use the main config port.
		listen = net.JoinHostPort(listen, fmt.Sprintf("%d", cfg.Port))
	}

	return listen, nil
}

// Path returns the path to the config file.
func Path() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gmux", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gmux", "config.toml")
}
