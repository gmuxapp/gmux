package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

const remoteDocsURL = "https://gmux.app/remote-access/"

func runRemote(stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "gmuxd remote: %v\n", err)
		return 1
	}

	if !cfg.Tailscale.Enabled {
		return remoteSetup(cfg, stdin, stdout, stderr)
	}
	return remoteStatus(stdout, stderr)
}

// remoteSetup explains remote access, asks for confirmation, enables it,
// restarts the daemon, and polls until tailscale reaches a known state.
func remoteSetup(cfg config.Config, stdin io.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, "Remote access lets you connect to this machine's terminal sessions")
	fmt.Fprintln(stdout, "from anywhere using your browser. It works through Tailscale, which")
	fmt.Fprintln(stdout, "creates a private encrypted network between your devices.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "You'll need a Tailscale account (free for personal use).")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  Learn more: %s\n", remoteDocsURL)
	fmt.Fprintln(stdout)

	fmt.Fprintf(stdout, "Enable remote access? [y/N] ")
	reader := bufio.NewReader(stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return 0
	}
	fmt.Fprintln(stdout)

	// Enable tailscale in the config file.
	cfgPath := config.Path()
	if err := enableTailscaleConfig(cfgPath); err != nil {
		fmt.Fprintf(stderr, "gmuxd remote: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Enabled tailscale in %s\n", cfgPath)

	// Restart the daemon so it picks up the new config.
	fmt.Fprintln(stdout, "Restarting daemon...")
	if code := startBackground(stdout, stderr); code != 0 {
		return code
	}

	fmt.Fprintln(stdout)
	return remotePoll(stdout, stderr)
}

// enableTailscaleConfig ensures tailscale.enabled = true in the config file.
// Creates the file if it doesn't exist, or appends the section if missing.
//
// Uses the TOML library to parse the file and understand the current state,
// then makes the minimal edit needed. This preserves comments, formatting,
// and all other user content.
func enableTailscaleConfig(cfgPath string) error {
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot read %s: %w", cfgPath, err)
	}

	// Parse with the TOML library to understand the current state.
	var parsed struct {
		Tailscale struct {
			Enabled bool `toml:"enabled"`
		} `toml:"tailscale"`
	}
	md, parseErr := toml.Decode(string(data), &parsed)
	if parseErr != nil {
		return fmt.Errorf("cannot parse %s: %w", cfgPath, parseErr)
	}

	if parsed.Tailscale.Enabled {
		return nil // already enabled
	}

	content := string(data)
	// Normalize: ensure trailing newline so regex patterns reliably
	// match line endings (e.g. section header at end of file).
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	switch {
	case !md.IsDefined("tailscale"):
		// No [tailscale] section: append it.
		if content == "" {
			// New file: add a reference comment.
			content = "# gmux daemon configuration\n# Reference: https://gmux.app/reference/host-toml/\n\n"
		} else {
			// content already ends with \n from normalization above.
			content += "\n"
		}
		content += "[tailscale]\nenabled = true\n"

	case !md.IsDefined("tailscale", "enabled"):
		// Section exists but no enabled key: insert after the header.
		content = insertAfterSection(content, "tailscale", "enabled = true")

	default:
		// enabled = false: replace the value in place.
		content = replaceKeyInSection(content, "tailscale", "enabled", "enabled = true")
	}

	return os.WriteFile(cfgPath, []byte(content), 0o644)
}

// insertAfterSection inserts a line immediately after the [section] header.
func insertAfterSection(content, section, line string) string {
	re := regexp.MustCompile(`(?m)^\[` + regexp.QuoteMeta(section) + `\][ \t]*\r?\n`)
	loc := re.FindStringIndex(content)
	if loc == nil {
		return content // shouldn't happen, caller checked IsDefined
	}
	return content[:loc[1]] + line + "\n" + content[loc[1]:]
}

// replaceKeyInSection replaces a key = value line within a TOML section.
// Matches the first line starting with the key name (ignoring leading
// whitespace) between the section header and the next section header.
func replaceKeyInSection(content, section, key, replacement string) string {
	headerRe := regexp.MustCompile(`(?m)^\[` + regexp.QuoteMeta(section) + `\][ \t]*\r?\n`)
	headerLoc := headerRe.FindStringIndex(content)
	if headerLoc == nil {
		return content
	}

	// Search for the key line between the header and the next section.
	rest := content[headerLoc[1]:]
	keyRe := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=.*$`)
	nextSection := regexp.MustCompile(`(?m)^\[`)

	// Limit search to before the next section header.
	searchEnd := len(rest)
	if loc := nextSection.FindStringIndex(rest); loc != nil {
		searchEnd = loc[0]
	}

	keyLoc := keyRe.FindStringIndex(rest[:searchEnd])
	if keyLoc == nil {
		return content
	}

	// Replace the matched line with the new value.
	absStart := headerLoc[1] + keyLoc[0]
	absEnd := headerLoc[1] + keyLoc[1]
	return content[:absStart] + replacement + content[absEnd:]
}

// remoteStatus checks on a running daemon with tailscale enabled.
// Polls until tailscale reaches a known state, then displays the result.
func remoteStatus(stdout, stderr io.Writer) int {
	sock := paths.SocketPath()
	if !unixipc.Healthy(sock) {
		fmt.Fprintln(stdout, "Remote access is enabled but the daemon is not running.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Start it with:")
		fmt.Fprintln(stdout, "  gmuxd start")
		return 0
	}

	return remotePoll(stdout, stderr)
}

// tailscaleHealth is the subset of the health response we care about.
type tailscaleHealth struct {
	Listen string
	TS     *tsHealth
}

type tsHealth struct {
	FQDN      string `json:"fqdn"`
	MagicDNS  bool   `json:"magic_dns"`
	HTTPS     bool   `json:"https"`
	AuthURL   string `json:"auth_url"`
	Connected bool   `json:"connected"`
}

// fetchTailscaleHealth fetches the tailscale status from the daemon.
func fetchTailscaleHealth(client *http.Client) (*tailscaleHealth, error) {
	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var health struct {
		OK   bool `json:"ok"`
		Data struct {
			Listen    string    `json:"listen"`
			Tailscale *tsHealth `json:"tailscale"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("unexpected response")
	}
	if !health.OK {
		return nil, fmt.Errorf("unhealthy response")
	}
	return &tailscaleHealth{
		Listen: health.Data.Listen,
		TS:     health.Data.Tailscale,
	}, nil
}

// remotePoll polls the daemon's health endpoint until tailscale reaches
// a known state (connected, needs login, or timeout). Then displays
// the appropriate information.
func remotePoll(stdout, stderr io.Writer) int {
	sock := paths.SocketPath()
	client := unixipc.Client(sock)

	fmt.Fprintf(stdout, "Connecting to Tailscale... ")

	// Poll until tailscale reaches a definitive state.
	// The daemon needs time to start tsnet, contact the control server,
	// and either get an auth URL or establish the connection.
	var result *tailscaleHealth
	deadline := time.Now().Add(30 * time.Second)
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	for time.Now().Before(deadline) {
		h, err := fetchTailscaleHealth(client)
		if err != nil {
			// Daemon might have just restarted; keep trying.
			<-tick.C
			continue
		}
		if h.TS == nil {
			// Tailscale object not yet present in response.
			<-tick.C
			continue
		}
		if h.TS.Connected || h.TS.AuthURL != "" {
			result = h
			break
		}
		// Still connecting, keep polling.
		<-tick.C
	}

	if result == nil {
		// Last-ditch fetch for whatever state we have.
		if h, err := fetchTailscaleHealth(client); err == nil {
			result = h
		}
	}

	fmt.Fprintln(stdout) // end the "Connecting..." line

	if result == nil || result.TS == nil {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stderr, "Could not reach the daemon. Check that it's running:")
		fmt.Fprintln(stderr, "  gmuxd start")
		return 1
	}

	return displayStatus(result, stdout)
}

// displayStatus renders the tailscale connection status.
func displayStatus(h *tailscaleHealth, stdout io.Writer) int {
	ts := h.TS

	// Needs login: show the auth URL and nothing else. The user must
	// complete login before we can know about HTTPS/MagicDNS.
	if ts.AuthURL != "" {
		fmt.Fprintln(stdout, "To complete setup, log in to Tailscale:")
		fmt.Fprintf(stdout, "  %s\n", ts.AuthURL)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "After logging in, run `gmuxd remote` again to check the connection.")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0
	}

	// Connected and fully operational.
	if ts.Connected {
		fmt.Fprintf(stdout, "  local:  http://%s\n", h.Listen)
		if ts.FQDN != "" {
			fmt.Fprintf(stdout, "  remote: https://%s\n", ts.FQDN)
		}
		fmt.Fprintln(stdout)

		problems := 0
		if !ts.HTTPS {
			fmt.Fprintln(stdout, "  ✗ HTTPS is not enabled in your tailnet")
			problems++
		}
		if !ts.MagicDNS {
			fmt.Fprintln(stdout, "  ✗ MagicDNS is not enabled in your tailnet")
			problems++
		}

		if problems > 0 {
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, "Enable these in your Tailscale admin console:")
			fmt.Fprintln(stdout, "  https://login.tailscale.com/admin/dns")
			fmt.Fprintln(stdout)
			fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
			return 1
		}

		fmt.Fprintln(stdout, "Remote access is active.")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0
	}

	// Not connected and no auth URL. Tailscale is in some intermediate state.
	fmt.Fprintln(stdout, "Tailscale is still connecting. This can take a minute on first setup.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Try again shortly:")
	fmt.Fprintln(stdout, "  gmuxd remote")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
	return 0
}
