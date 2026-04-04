// Package config loads gmuxd configuration from ~/.config/gmux/host.toml.
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
	// Port is the TCP port for the HTTP listener (default 8790).
	Port int `toml:"port"`

	Tailscale TailscaleConfig `toml:"tailscale"`
}

// TailscaleConfig controls the optional tailscale (tsnet) listener.
type TailscaleConfig struct {
	// Enabled starts a tsnet listener on the tailnet. Default false.
	Enabled bool `toml:"enabled"`

	// Hostname is the tailscale machine name (e.g. "gmux" -> gmux.tailnet.ts.net).
	// Default "gmux".
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

// validateListen checks that the listen address is safe to bind to.
// Accepts: loopback (127.0.0.1, ::1), RFC 1918 (10/8, 172.16/12, 192.168/16),
// link-local (169.254/16), CGNAT (100.64/10, used by Tailscale/WireGuard),
// Docker bridge (172.17/16 falls under 172.16/12), unspecified (0.0.0.0 / ::,
// for containers), and IPv6 ULA (fd00::/8).
// Rejects: public IPs (use Tailscale for internet-facing access).
func validateListen(addr string) error {
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("%q is not a valid IP address", addr)
	}

	// Allow loopback (default).
	if ip.IsLoopback() {
		return nil
	}

	// Allow 0.0.0.0 / :: (all interfaces) for container use.
	if ip.IsUnspecified() {
		return nil
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

// ListenAddr returns the effective TCP listen address (host:port).
// The bind address is controlled by the GMUXD_LISTEN env var
// (default "127.0.0.1"). The port comes from the config file.
func (cfg Config) ListenAddr() (string, error) {
	listen := "127.0.0.1"
	if env := os.Getenv("GMUXD_LISTEN"); env != "" {
		listen = env
		if err := validateListen(listen); err != nil {
			return "", err
		}
	}

	return net.JoinHostPort(listen, fmt.Sprintf("%d", cfg.Port)), nil
}

// Dir returns the gmux config directory (~/.config/gmux/).
func Dir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gmux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gmux")
}

// Path returns the path to the host config file.
func Path() string {
	return filepath.Join(Dir(), "host.toml")
}

