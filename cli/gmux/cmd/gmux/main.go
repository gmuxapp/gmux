package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

// version is set at build time via -ldflags "-X main.version=..."
// Falls back to "dev" for local builds.
var version = "dev"

func main() {
	log.SetPrefix("gmux: ")
	log.SetFlags(0)

	m, f, rest, err := parseCLI(os.Args[1:])
	if err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			fmt.Fprintln(os.Stderr)
		}
		printUsage(os.Stderr)
		os.Exit(2)
	}

	switch m {
	case modeHelp:
		printUsage(os.Stdout)
		return
	case modeUI:
		openUI()
		return
	case modeRun:
		runSession(rest, !f.noAttach)
		return
	case modeList:
		os.Exit(cmdList())
	case modeKill:
		os.Exit(cmdKill(rest[0]))
	case modeTail:
		os.Exit(cmdTail(rest[0], f.tail))
	case modeAttach:
		os.Exit(cmdAttach(rest[0]))
	case modeSend:
		var text *string
		if len(rest) == 2 {
			text = &rest[1]
		}
		os.Exit(cmdSend(rest[0], text, f.noSubmit))
	}
}

// openUI implements the bare `gmux` invocation: ensure gmuxd is up,
// learn its TCP listen address and auth token from /v1/health, and
// hand those to the local browser.
func openUI() {
	ensureGmuxd()

	// Wait for gmuxd to be reachable before opening browser.
	client := gmuxdClient()
	baseURL := gmuxdBaseURL()
	var healthBody []byte
	ready := false
	for range 15 {
		if resp, err := client.Get(baseURL + "/v1/health"); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				healthBody = body
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		log.Fatalf("gmuxd is not running (check %s/gmuxd.log for errors)", os.TempDir())
	}

	// Parse health response for TCP address and auth token.
	listenAddr := parseHealthField(healthBody, "listen")
	token := parseHealthField(healthBody, "auth_token")

	browserURL := "http://" + listenAddr
	if token != "" {
		browserURL = fmt.Sprintf("http://%s/auth/login?token=%s", listenAddr, token)
	}

	// Print access URLs.
	fmt.Fprintf(os.Stderr, "  local:  http://%s\n", listenAddr)
	if tsURL := parseTailscaleURL(healthBody); tsURL != "" {
		fmt.Fprintf(os.Stderr, "  remote: %s\n", maskTailscaleURL(tsURL))
	}
	if updateVer := parseUpdateAvailable(healthBody); updateVer != "" {
		fmt.Fprintf(os.Stderr, "  update: %s available — %s\n", updateVer, upgradeHint())
	}

	openBrowser(browserURL)
}
