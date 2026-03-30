package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
)

const remoteDocsURL = "https://gmux.app/remote-access/"

func runRemote(stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "gmuxd remote: %v\n", err)
		return 1
	}

	var code int
	if !cfg.Tailscale.Enabled {
		code = remoteSetup(cfg, stdout, stderr)
	} else {
		code = remoteStatus(stdout, stderr)
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Docs: %s\n", remoteDocsURL)
	return code
}

// remoteSetup guides the user through enabling tailscale.
func remoteSetup(cfg config.Config, stdout, stderr io.Writer) int {
	cfgPath := config.Path()

	fmt.Fprintln(stdout, "Remote access is not configured.")
	fmt.Fprintln(stdout)

	// Check if config file exists.
	_, err := os.Stat(cfgPath)
	configExists := err == nil

	if configExists {
		fmt.Fprintf(stdout, "Add this to %s:\n\n", cfgPath)
		fmt.Fprintln(stdout, "  [tailscale]")
		fmt.Fprintln(stdout, "  enabled = true")
		fmt.Fprintln(stdout)
	} else {
		// Create the config file for them.
		dir := filepath.Dir(cfgPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(stderr, "gmuxd remote: cannot create %s: %v\n", dir, err)
			return 1
		}
		content := "[tailscale]\nenabled = true\n"
		if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
			fmt.Fprintf(stderr, "gmuxd remote: cannot write %s: %v\n", cfgPath, err)
			return 1
		}
		fmt.Fprintf(stdout, "Created %s with tailscale enabled.\n\n", cfgPath)
	}

	sock := paths.SocketPath()
	if configExists {
		fmt.Fprintln(stdout, "Then restart the daemon:")
		fmt.Fprintln(stdout, "  gmuxd start")
	} else if unixipc.Healthy(sock) {
		fmt.Fprintln(stdout, "Restart the daemon to connect to your tailnet:")
		fmt.Fprintln(stdout, "  gmuxd start")
	} else {
		fmt.Fprintln(stdout, "Start the daemon to connect to your tailnet:")
		fmt.Fprintln(stdout, "  gmuxd start")
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "On first start, gmuxd will print a Tailscale login URL.")
	fmt.Fprintln(stdout, "Visit it to register gmux as a device in your tailnet.")

	return 0
}

// remoteStatus shows the current tailscale connection state via IPC.
func remoteStatus(stdout, stderr io.Writer) int {
	sock := paths.SocketPath()
	client := unixipc.Client(sock)

	resp, err := client.Get("http://localhost/v1/health")
	if err != nil {
		fmt.Fprintln(stdout, "Remote access is enabled in config but the daemon is not running.")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Start it with:")
		fmt.Fprintln(stdout, "  gmuxd start")
		return 0
	}
	defer resp.Body.Close()

	var health struct {
		OK   bool `json:"ok"`
		Data struct {
			Listen       string `json:"listen"`
			TailscaleURL string `json:"tailscale_url"`
			Tailscale    *struct {
				FQDN     string `json:"fqdn"`
				MagicDNS bool   `json:"magic_dns"`
				HTTPS    bool   `json:"https"`
				AuthURL  string `json:"auth_url"`
			} `json:"tailscale"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		fmt.Fprintf(stderr, "gmuxd remote: unexpected health response\n")
		return 1
	}

	ts := health.Data.Tailscale

	fmt.Fprintf(stdout, "  local:  http://%s\n", health.Data.Listen)

	if ts == nil {
		if health.Data.TailscaleURL != "" {
			fmt.Fprintf(stdout, "  remote: %s\n", health.Data.TailscaleURL)
		}
		return 0
	}

	// Needs login.
	if ts.AuthURL != "" {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Tailscale needs login. Visit this URL to register the device:")
		fmt.Fprintf(stdout, "  %s\n", ts.AuthURL)
		return 0
	}

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
		return 1
	}

	if ts.FQDN == "" {
		fmt.Fprintln(stdout, "Tailscale is enabled but not yet connected.")
		fmt.Fprintln(stdout, "The device may still be registering. Restart with:")
		fmt.Fprintln(stdout, "  gmuxd start")
		return 0
	}

	fmt.Fprintln(stdout, "Remote access is active.")
	return 0
}
