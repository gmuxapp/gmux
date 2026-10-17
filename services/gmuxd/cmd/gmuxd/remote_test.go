package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(content, "host-toml") {
		t.Errorf("new file should contain reference link:\n%s", content)
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
	// Should not prepend the header comment to an existing user file.
	if strings.HasPrefix(content, "# gmux") {
		t.Errorf("should not add header comment to existing file:\n%s", content)
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

func TestEnableTailscaleConfig_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "host.toml")
	initial := `# My gmux config
port = 9999

[tailscale]
# Keep this hostname!
hostname = "mybox"
enabled = false  # was disabled

[discovery]
# auto-discover containers
devcontainers = true
`
	os.WriteFile(cfgPath, []byte(initial), 0o644)

	if err := enableTailscaleConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(cfgPath)
	content := string(data)

	// Comments must survive.
	for _, want := range []string{
		"# My gmux config",
		"# Keep this hostname!",
		"# auto-discover containers",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("comment lost %q in:\n%s", want, content)
		}
	}

	// Values must survive.
	if !strings.Contains(content, "port = 9999") {
		t.Errorf("port lost:\n%s", content)
	}
	if !strings.Contains(content, `hostname = "mybox"`) {
		t.Errorf("hostname lost:\n%s", content)
	}

	// The enabled line must be replaced.
	if !strings.Contains(content, "enabled = true") {
		t.Errorf("enabled not set:\n%s", content)
	}
	if strings.Contains(content, "enabled = false") {
		t.Errorf("old enabled still present:\n%s", content)
	}
}

func TestEnableTailscaleConfig_AlreadyEnabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "host.toml")
	initial := "[tailscale]\nenabled = true\nhostname = \"mybox\"\n"
	os.WriteFile(cfgPath, []byte(initial), 0o644)

	if err := enableTailscaleConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(cfgPath)
	if string(data) != initial {
		t.Errorf("file was modified when already enabled:\n%s", data)
	}
}

// Verify that enableTailscaleConfig produces valid TOML that config.Load
// accepts with tailscale actually enabled. Tests the full round-trip.
func TestEnableTailscaleConfig_ProducesValidConfig(t *testing.T) {
	cases := []struct {
		name    string
		initial string
	}{
		{"empty file", ""},
		{"port only", "port = 9999\n"},
		{"section disabled", "[tailscale]\nenabled = false\nallow = [\"alice@github\"]\n"},
		{"section no enabled", "[tailscale]\nallow = [\"alice@github\"]\n"},
		{"no trailing newline", "[tailscale]\nallow = [\"alice@github\"]"},
		{"section header only no newline", "[tailscale]"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgDir := filepath.Join(dir, "gmux")
			os.MkdirAll(cfgDir, 0o755)
			cfgPath := filepath.Join(cfgDir, "host.toml")
			if tt.initial != "" {
				os.WriteFile(cfgPath, []byte(tt.initial), 0o644)
			}

			if err := enableTailscaleConfig(cfgPath); err != nil {
				t.Fatal(err)
			}

			t.Setenv("XDG_CONFIG_HOME", dir)
			cfg, err := config.Load()
			if err != nil {
				data, _ := os.ReadFile(cfgPath)
				t.Fatalf("config.Load failed: %v\nfile contents:\n%s", err, data)
			}
			if !cfg.Tailscale.Enabled {
				data, _ := os.ReadFile(cfgPath)
				t.Errorf("tailscale.enabled = false after enableTailscaleConfig\nfile contents:\n%s", data)
			}
		})
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
	if !strings.Contains(out, "gmux remote") {
		t.Errorf("should tell user to run gmux remote again:\n%s", out)
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
			HTTPS:     true,
			MagicDNS:  true,
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
	if strings.Contains(out, "login.tailscale.com/admin/dns") {
		t.Errorf("should not point at admin console when tailnet is fine:\n%s", out)
	}
}

func TestDisplayStatus_NotConnectedMissingHTTPS(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS: &tsHealth{
			Connected:    false,
			HTTPS:        false,
			BackendState: "Running",
		},
	}
	code := displayStatus(h, &stdout)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "HTTPS is not enabled") {
		t.Errorf("should name HTTPS as the likely cause:\n%s", out)
	}
	if !strings.Contains(out, "login.tailscale.com/admin/dns") {
		t.Errorf("should link to admin console:\n%s", out)
	}
}

// Before the backend reaches Running, HTTPS=false may just mean the
// tailnet state isn't known yet. Don't blame HTTPS settings then.
func TestDisplayStatus_StartingHTTPSUnknown(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS: &tsHealth{
			Connected:    false,
			HTTPS:        false,
			BackendState: "Starting",
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
	if strings.Contains(out, "login.tailscale.com/admin/dns") {
		t.Errorf("should not blame HTTPS while backend state is unknown:\n%s", out)
	}
}

func TestDisplayStatus_TailscaleNotStarted(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{Listen: "127.0.0.1:8790", TS: nil}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Tailscale hasn't started yet") {
		t.Errorf("should say tailscale hasn't started:\n%s", out)
	}
	if strings.Contains(out, "Could not reach the daemon") {
		t.Errorf("should not claim the daemon is unreachable:\n%s", out)
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

// The poll loop stops only on a settled state. `starting` and `unavailable` are
// transient — the latter a single failed status read, not a finished startup —
// so a one-off blip must not end `gmux remote` with an error while the node
// would report active on the next read.
func TestSelfReachTerminal(t *testing.T) {
	terminal := []string{"reachable", "key_expired", "needs_login", "https_disabled", "stopped", "disabled"}
	for _, s := range terminal {
		if !selfReachTerminal(s) {
			t.Errorf("%s: selfReachTerminal = false, want true", s)
		}
	}
	for _, s := range []string{"starting", "unavailable"} {
		if selfReachTerminal(s) {
			t.Errorf("%s: selfReachTerminal = true, want false (transient — keep polling)", s)
		}
	}
}

// A login URL published after the health-probe timeout (5s) must still be
// delivered: the login request uses a client whose timeout exceeds the daemon's
// login window. Reproduced at test scale — a daemon that answers the login POST
// after 1s is canceled by a 500ms client (the old behavior) but served by the
// login client, so the CLI prints the URL, not the auth-key fallback.
func TestRequestRemoteLoginSurvivesLateURL(t *testing.T) {
	const url = "https://login.tailscale.com/a/late"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1 * time.Second) // the daemon's login window is bounded; control is slow
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"data":{"auth_url":"` + url + `"}}`))
	}))
	defer srv.Close()

	short := &http.Client{Timeout: 500 * time.Millisecond}
	if _, err := requestRemoteLoginAt(srv.URL, short); err == nil {
		t.Fatal("a 500ms client should cancel a 1s login request (reproduces the pre-fix timeout)")
	}

	// The login client's timeout (remoteLoginTimeout) is the fix: a long
	// enough client delivers the late URL. Use a scaled client (2s > 1s) to
	// keep the test fast while exercising the same code path.
	long := &http.Client{Timeout: 2 * time.Second}
	got, err := requestRemoteLoginAt(srv.URL, long)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	if got != url {
		t.Fatalf("URL = %q, want %q", got, url)
	}
}

// The CLI's login client timeout must exceed the daemon's login window so a URL
// the daemon produces late is delivered, not canceled into the fallback. This
// pins the invariant Greptile flagged (unixipc.Client is 5s; daemon window is 12s).
func TestRemoteLoginTimeoutExceedsDaemonLoginWindow(t *testing.T) {
	if remoteLoginTimeout <= 12*time.Second {
		t.Errorf("remoteLoginTimeout = %s, want > the daemon's 12s login window", remoteLoginTimeout)
	}
}

// ── Self-reach-driven display (tailscale_self): the fix for #546's follow-up ─
//
// A node that was previously connected and whose key has since expired keeps
// its FQDN, so the raw `connected` flag stays true. These drive the fix:
// `gmux remote` must NOT print "Remote access is active" for an expired node,
// and must print a working login URL — the same mapping the status line uses.

// Expired key, previously had an FQDN, no AuthURL yet → the login is triggered
// and the URL it produces is printed. "Remote access is active" must not appear.
func TestDisplayStatus_KeyExpiredTriggersLogin(t *testing.T) {
	var stdout bytes.Buffer
	var triggered int
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS:     &tsHealth{FQDN: "gmux.tailnet.ts.net", Connected: true, HTTPS: true, MagicDNS: true}, // the lying leftover state
		Self:   &selfReach{State: "key_expired"},                                                     // no AuthURL published
		login: func() (string, error) {
			triggered++
			return "https://login.tailscale.com/a/fresh", nil
		},
	}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if triggered != 1 {
		t.Errorf("login trigger calls = %d, want 1", triggered)
	}
	out := stdout.String()
	if !strings.Contains(out, "https://login.tailscale.com/a/fresh") {
		t.Errorf("missing login URL in:\n%s", out)
	}
	if strings.Contains(out, "Remote access is active") {
		t.Errorf("must not claim active for an expired key:\n%s", out)
	}
}

// Expired key that already publishes a login URL → print it, do not trigger a
// second login.
func TestDisplayStatus_KeyExpiredWithAuthURL(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		Self:   &selfReach{State: "key_expired", AuthURL: "https://login.tailscale.com/a/abc123"},
		login: func() (string, error) {
			t.Fatal("login must not be triggered when a URL is already published")
			return "", nil
		},
	}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "https://login.tailscale.com/a/abc123") {
		t.Errorf("missing login URL in:\n%s", out)
	}
	if strings.Contains(out, "Remote access is active") {
		t.Errorf("must not claim active for an expired key:\n%s", out)
	}
}

// The healthy case must stay "active": reachable → the remote URL and the
// active line, exactly as before the fix.
func TestDisplayStatus_ReachableStaysActive(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		TS:     &tsHealth{FQDN: "gmux.tailnet.ts.net", Connected: true, HTTPS: true, MagicDNS: true},
		Self:   &selfReach{State: "reachable", URL: "https://gmux.tailnet.ts.net"},
	}
	code := displayStatus(h, &stdout)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Remote access is active") {
		t.Errorf("reachable must say active:\n%s", out)
	}
	if !strings.Contains(out, "https://gmux.tailnet.ts.net") {
		t.Errorf("missing remote URL:\n%s", out)
	}
	if strings.Contains(out, "login") {
		t.Errorf("reachable must not ask for login:\n%s", out)
	}
}

// When the daemon cannot produce a login URL at all (no tsnet node, login
// yields nothing), print the supported recovery step — TS_AUTHKEY — and NOT a
// login URL. This is the fallback the status line's "run 'gmux remote' to
// re-authenticate" resolves to, so the promise matches what is delivered.
func TestDisplayStatus_KeyExpiredNoURLFallback(t *testing.T) {
	var stdout bytes.Buffer
	h := &tailscaleHealth{
		Listen: "127.0.0.1:8790",
		Self:   &selfReach{State: "key_expired"},
		login:  func() (string, error) { return "", nil }, // daemon has no URL
	}
	code := displayStatus(h, &stdout)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "TS_AUTHKEY") {
		t.Errorf("must give the TS_AUTHKEY recovery step:\n%s", out)
	}
	if strings.Contains(out, "login.tailscale.com/a/") {
		t.Errorf("must not promise a login URL it does not have:\n%s", out)
	}
	if strings.Contains(out, "Remote access is active") {
		t.Errorf("must not claim active:\n%s", out)
	}
}

// The mock daemon: a node that was connected and whose key expired publishes no
// URL until a login is triggered; after the POST it reports key_expired with a
// fresh AuthURL. This proves requestRemoteLogin + fetchTailscaleHealthAt drive
// the flow end to end (the CLI half of the fix). A real key expiry cannot be
// produced locally, so the daemon is a mock LocalAPI behind a health endpoint.
func TestRemoteLoginFlowAgainstMockDaemon(t *testing.T) {
	const freshURL = "https://login.tailscale.com/a/triggered"
	var loginTriggered bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/remote/login":
			loginTriggered = true
			_, _ = w.Write([]byte(`{"ok":true,"data":{"auth_url":"` + freshURL + `"}}`))
		case r.URL.Path == "/v1/health":
			// Before the trigger: key_expired, no URL. After: URL present.
			self := `{"state":"key_expired","line":"NOT reachable"}`
			if loginTriggered {
				self = `{"state":"key_expired","line":"NOT reachable","auth_url":"` + freshURL + `"}`
			}
			_, _ = w.Write([]byte(`{"ok":true,"data":{"listen":"127.0.0.1:8790","tailscale":{"connected":true,"fqdn":"gmux.tailnet.ts.net"},"tailscale_self":` + self + `}}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Pre-trigger: the node reports key_expired with no URL, and Connected=true
	// (the lying leftover FQDN). The command must not treat it as active.
	h, err := fetchTailscaleHealthAt(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if h.Self == nil || h.Self.State != "key_expired" {
		t.Fatalf("pre-trigger self = %+v, want key_expired", h.Self)
	}
	if h.Self.AuthURL != "" {
		t.Fatalf("pre-trigger AuthURL = %q, want empty", h.Self.AuthURL)
	}

	// Trigger login on the daemon's node and read the URL it produces.
	url, err := requestRemoteLoginAt(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if url != freshURL {
		t.Fatalf("login URL = %q, want %q", url, freshURL)
	}

	// The end-to-end display, fed the post-trigger health, prints the URL and
	// not "active".
	after, err := fetchTailscaleHealthAt(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	after.login = func() (string, error) { return requestRemoteLoginAt(srv.URL, srv.Client()) }
	var stdout bytes.Buffer
	if code := displayStatus(after, &stdout); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), freshURL) || strings.Contains(stdout.String(), "Remote access is active") {
		t.Errorf("output =\n%s", stdout.String())
	}
}
