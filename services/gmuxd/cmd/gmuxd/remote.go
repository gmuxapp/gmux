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
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
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
		fmt.Fprintln(stdout, "  gmux daemon start")
		return 0
	}

	return remotePoll(stdout, stderr)
}

// tailscaleHealth is the subset of the health response we care about.
type tailscaleHealth struct {
	Listen string
	TS     *tsHealth
	// Self is the daemon's own tailnet-reachability classification
	// (tailscale_self). It is the SINGLE truth `gmux remote` decides on: the
	// exact mapping `daemon status` prints, so the command and the status line
	// can never disagree. When present, the active/login decision uses it and
	// ignores tsHealth.Connected, which lies after a key expiry (the listener
	// keeps its FQDN, so `connected` stays true).
	Self *selfReach
	// login triggers an interactive login on the daemon's node and returns the
	// URL control publishes. Set by remotePoll from the daemon's login endpoint;
	// nil on an older daemon that has no endpoint, where an empty URL falls back
	// to the printed recovery step.
	login func() (string, error)
}

type tsHealth struct {
	FQDN         string `json:"fqdn"`
	MagicDNS     bool   `json:"magic_dns"`
	HTTPS        bool   `json:"https"`
	AuthURL      string `json:"auth_url"`
	BackendState string `json:"backend_state"`
	Connected    bool   `json:"connected"`
}

// selfReach mirrors tsauth.SelfReach on /v1/health. State is wire contract
// (tsauth.Reach* constants), which is what the display switches on.
type selfReach struct {
	State   string `json:"state"`
	Line    string `json:"line"`
	URL     string `json:"url"`
	AuthURL string `json:"auth_url"`
}

// needsLogin mirrors tsauth.SelfReach.NeedsLogin over the decoded wire shape.
func (s *selfReach) needsLogin() bool {
	switch s.State {
	case tsauth.ReachKeyExpired, tsauth.ReachNeedsLogin, tsauth.ReachStopped:
		return true
	}
	return false
}

const (
	healthURLPath      = "http://localhost/v1/health"
	remoteLoginURLPath = "http://localhost/v1/remote/login"
)

// fetchTailscaleHealthAt fetches the tailscale status from a daemon at baseURL
// ("http://localhost" over the unix socket; an httptest URL in tests).
func fetchTailscaleHealthAt(baseURL string, client *http.Client) (*tailscaleHealth, error) {
	resp, err := client.Get(baseURL + "/v1/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var health struct {
		OK   bool `json:"ok"`
		Data struct {
			Listen        string     `json:"listen"`
			Tailscale     *tsHealth  `json:"tailscale"`
			TailscaleSelf *selfReach `json:"tailscale_self"`
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
		Self:   health.Data.TailscaleSelf,
	}, nil
}

// fetchTailscaleHealth fetches the tailscale status from the local daemon.
func fetchTailscaleHealth(client *http.Client) (*tailscaleHealth, error) {
	return fetchTailscaleHealthAt("http://localhost", client) // healthURLPath
}

// requestRemoteLogin asks the daemon to start an interactive login on its own
// tsnet node and returns the login URL it produces. An expired key on a
// previously-connected node publishes no URL on its own, so the login must be
// triggered; this is the daemon-side StartLoginInteractive path (see
// tsauth.RequestLogin). Returns "" (no error) when the daemon has no tsnet
// node, in which case the caller prints the supported fallback.
func requestRemoteLogin(client *http.Client) (string, error) {
	return requestRemoteLoginAt("http://localhost", client) // remoteLoginURLPath
}

// remoteLoginTimeout bounds the CLI's login-trigger request. It must exceed the
// daemon's full login window (tsauth.RequestLogin polls for a fresh AuthURL for
// loginAuthURLTimeout+2s = 12s); the default unixipc.Client 5s timeout would
// cancel a request the daemon can still complete, dropping to the auth-key
// fallback instead of printing the URL. Health probes keep the short timeout.
const remoteLoginTimeout = 15 * time.Second

// requestRemoteLoginAt is requestRemoteLogin against a daemon at baseURL (an
// httptest URL in tests).
func requestRemoteLoginAt(baseURL string, client *http.Client) (string, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/remote/login", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		OK   bool `json:"ok"`
		Data struct {
			AuthURL string `json:"auth_url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("unexpected response")
	}
	if !out.OK {
		return "", fmt.Errorf("login request failed")
	}
	return out.Data.AuthURL, nil
}

// remotePoll polls the daemon's health endpoint until tailscale reaches
// a known state (connected, needs login, or timeout). Then displays
// the appropriate information.
func remotePoll(stdout, stderr io.Writer) int {
	sock := paths.SocketPath()
	client := unixipc.Client(sock)

	fmt.Fprintf(stdout, "Connecting to Tailscale... ")

	// Poll until the daemon reports a definitive self-reach state. A key expiry
	// is definitive (state key_expired) even though the listener keeps its FQDN,
	// so the poll stops and the login flow runs. `starting` and `unavailable` are
	// transient (the latter is a single failed status read, not a finished
	// startup) so polling continues; a not-yet-present self blob also keeps
	// polling. On an older daemon with no tailscale_self, fall back to the
	// pre-classification condition.
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
		if h.Self != nil {
			result = h
			// Poll stops on a definitive state. `starting` is transient; so is
			// `unavailable` — a single failed LocalAPI read is not a finished
			// startup — so keep polling and let the 30s window absorb a blip.
			// The last state is retained, so a deadline that ends on `unavailable`
			// still reports it.
			if selfReachTerminal(h.Self.State) {
				break
			}
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

	if result == nil {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stderr, "Could not reach the daemon. Check that it's running:")
		fmt.Fprintln(stderr, "  gmux daemon start")
		return 1
	}

	// Wire the daemon's login trigger so displayStatus can produce a URL for a
	// needs-login node that does not already publish one (an expired key on a
	// previously-connected node). Use a client whose timeout exceeds the
	// daemon's login window, so a URL the daemon produces late is delivered
	// rather than canceled into the auth-key fallback.
	loginClient := unixipc.Client(sock)
	loginClient.Timeout = remoteLoginTimeout
	result.login = func() (string, error) { return requestRemoteLogin(loginClient) }

	return displayStatus(result, stdout)
}

// selfReachTerminal reports a self-reach state `gmux remote` stops polling on:
// a settled answer it can act on. `starting` is transient (node coming up) and
// `unavailable` is a single failed status read, not a finished startup — a blip
// the 30s polling window should absorb, not a terminal verdict. Everything else
// (reachable, key_expired, needs_login, https_disabled, stopped, disabled) is
// definitive.
func selfReachTerminal(state string) bool {
	switch state {
	case tsauth.ReachStarting, tsauth.ReachUnavailable:
		return false
	}
	return true
}

// displayStatus renders the tailscale connection status. The daemon's own
// self-reach classification (tailscale_self) is the single truth `gmux remote`
// acts on — the same mapping `daemon status` prints — so the command's active/
// login decision and the status line can never diverge. A raw `connected` flag
// lies after a key expiry (the listener keeps its FQDN), so it is not trusted
// when a self-reach state is available. An older daemon with no tailscale_self
// falls back to the pre-classification rendering.
func displayStatus(h *tailscaleHealth, stdout io.Writer) int {
	if h.Self != nil {
		return displaySelfReach(h, stdout)
	}
	return displayLegacyDiag(h, stdout)
}

// displaySelfReach renders the connection status from the daemon's
// self-reach classification. The active/login decision is driven by the state
// (tsauth.Reach*), reused from the mapping that produced the status line.
func displaySelfReach(h *tailscaleHealth, stdout io.Writer) int {
	switch h.Self.State {
	case tsauth.ReachReachable:
		fmt.Fprintf(stdout, "  local:  http://%s\n", h.Listen)
		if h.Self.URL != "" {
			fmt.Fprintf(stdout, "  remote: %s\n", h.Self.URL)
		}
		fmt.Fprintln(stdout)
		if h.TS != nil && !h.TS.MagicDNS {
			fmt.Fprintln(stdout, "  ✗ MagicDNS is not enabled in your tailnet")
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, "Enable it in your Tailscale admin console:")
			fmt.Fprintln(stdout, "  https://login.tailscale.com/admin/dns")
			fmt.Fprintln(stdout)
			fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
			return 1
		}
		fmt.Fprintln(stdout, "Remote access is active.")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0

	case tsauth.ReachNoHTTPS:
		fmt.Fprintln(stdout, "Tailscale is logged in but the connection isn't coming up.")
		fmt.Fprintln(stdout, "The cause: HTTPS is not enabled in your tailnet, which gmuxd")
		fmt.Fprintln(stdout, "needs to serve remote access.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Enable HTTPS (and MagicDNS) in your Tailscale admin console:")
		fmt.Fprintln(stdout, "  https://login.tailscale.com/admin/dns")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Then run `gmux remote` again.")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 1

	case tsauth.ReachStarting:
		fmt.Fprintln(stdout, "Tailscale is still connecting. This can take a minute on first setup.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Try again shortly:")
		fmt.Fprintln(stdout, "  gmux remote")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0

	case tsauth.ReachUnavailable:
		fmt.Fprintln(stdout, h.Self.Line)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Check the daemon and try again:")
		fmt.Fprintln(stdout, "  gmux daemon status")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 1

	default:
		// key_expired / needs_login / stopped: re-authenticate. Use the URL the
		// node already publishes, else trigger a login on it and use the URL
		// that produces. Only when no URL can be produced is the supported
		// fallback printed — never a URL we do not have.
		if h.Self.needsLogin() {
			return displayLoginRequired(h, stdout)
		}
		// A state `gmux remote` was not built to render (a newer daemon): show
		// the line the daemon produced verbatim, plus the docs pointer.
		fmt.Fprintln(stdout, h.Self.Line)
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0
	}
}

// displayLoginRequired prints the re-authentication step for a node that needs
// login. It prefers a URL the node already publishes; an expired key on a
// previously-connected node publishes none, so it triggers a login on the
// daemon's node (the StartLoginInteractive path) and uses the URL control
// returns. When no URL can be produced, it prints the tsnet-supported recovery
// step (restart with TS_AUTHKEY) — the status line promises only "run 'gmux
// remote' to re-authenticate", which is exactly this output, not a literal URL.
func displayLoginRequired(h *tailscaleHealth, stdout io.Writer) int {
	authURL := h.Self.AuthURL
	if authURL == "" && h.login != nil {
		if url, err := h.login(); err == nil {
			authURL = url
		}
	}
	if authURL != "" {
		label := "To complete setup, log in to Tailscale:"
		if h.Self.State == tsauth.ReachKeyExpired {
			label = "To re-authenticate this node, log in to Tailscale:"
		}
		fmt.Fprintln(stdout, label)
		fmt.Fprintf(stdout, "  %s\n", authURL)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "After logging in, run `gmux remote` again to check the connection.")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0
	}

	fmt.Fprintln(stdout, "Tailscale did not offer a login URL for this node, and")
	fmt.Fprintln(stdout, "triggering a login produced none.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Re-authenticate the node with a Tailscale auth key by restarting")
	fmt.Fprintln(stdout, "the daemon with TS_AUTHKEY set (this is tsnet's own recovery path):")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "  TS_AUTHKEY=<your-key> gmux daemon restart")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
	return 1
}

// displayLegacyDiag renders the connection status from the raw tailscale
// diagnostics blob, for daemons that predate the tailscale_self field.
func displayLegacyDiag(h *tailscaleHealth, stdout io.Writer) int {
	ts := h.TS

	// Daemon is reachable but hasn't started Tailscale yet.
	if ts == nil {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "The daemon is running but Tailscale hasn't started yet.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Try again shortly:")
		fmt.Fprintln(stdout, "  gmuxd remote")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 0
	}

	// Needs login: show the auth URL and nothing else. The user must
	// complete login before we can know about HTTPS/MagicDNS.
	if ts.AuthURL != "" {
		fmt.Fprintln(stdout, "To complete setup, log in to Tailscale:")
		fmt.Fprintf(stdout, "  %s\n", ts.AuthURL)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "After logging in, run `gmux remote` again to check the connection.")
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

	// Not connected and no auth URL. Tailscale is in some intermediate
	// state. If the backend is running (logged in, netmap synced) but
	// the tailnet has no HTTPS certificates enabled, the listener can
	// never come up: tsnet needs a cert to serve. Surface that directly
	// instead of telling the user to retry forever. Only trust the
	// HTTPS flag when the backend reports Running; before that, false
	// may just mean the state isn't known yet.
	if ts.BackendState == "Running" && !ts.HTTPS {
		fmt.Fprintln(stdout, "Tailscale is logged in but the connection isn't coming up.")
		fmt.Fprintln(stdout, "The cause: HTTPS is not enabled in your tailnet, which gmuxd")
		fmt.Fprintln(stdout, "needs to serve remote access.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Enable HTTPS (and MagicDNS) in your Tailscale admin console:")
		fmt.Fprintln(stdout, "  https://login.tailscale.com/admin/dns")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Then run `gmuxd remote` again.")
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
		return 1
	}

	fmt.Fprintln(stdout, "Tailscale is still connecting. This can take a minute on first setup.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Try again shortly:")
	fmt.Fprintln(stdout, "  gmux remote")
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
	return 0
}
