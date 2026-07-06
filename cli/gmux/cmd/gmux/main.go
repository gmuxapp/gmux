package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"
)

// version is set at build time via -ldflags "-X main.version=..."
// Falls back to "dev" for local builds.
var version = "dev"

func main() {
	log.SetPrefix("gmux: ")
	log.SetFlags(0)

	cmd, err := parseCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		fmt.Fprintln(os.Stderr)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	switch cmd.mode {
	case modeHelp:
		printUsage(os.Stdout)
	case modeVersion:
		fmt.Println(version)
	case modeOpen:
		openUI()
	case modeRun:
		runSession(cmd.runArgs, !cmd.detach, runDirectives{
			ResumeID:    cmd.resumeID,
			InitialCols: cmd.initialCols,
			InitialRows: cmd.initialRows,
		})
	case modeList:
		os.Exit(cmdList(cmd.all, cmd.json))
	case modeKill:
		os.Exit(cmdKill(cmd.ref))
	case modeTail:
		os.Exit(cmdTail(cmd.ref, cmd.tailLines, cmd.raw))
	case modeAttach:
		os.Exit(cmdAttach(cmd.ref))
	case modeSend:
		os.Exit(cmdSend(cmd.ref, cmd.sendText, cmd.sendKeys))
	case modeSendKeys:
		os.Exit(cmdSendKeys(cmd.ref, cmd.keys, cmd.keysLiteral))
	case modeWait:
		os.Exit(cmdWait(cmd.ref, cmd.timeout))
	case modeEdit:
		runEdit(cmd.editFile)
	case modeEditChild:
		os.Exit(editChild(cmd.editFile))
	case modeDaemon:
		os.Exit(execGmuxd(cmd.daemonSub))
	case modeAuth:
		os.Exit(execGmuxd("auth"))
	case modeRemote:
		os.Exit(execGmuxd("remote"))
	case modeDumpEnv:
		os.Exit(dumpEnv())
	case modeCodexHook:
		os.Exit(codexHook(cmd.codexHookEvent))
	case modeClaudeHook:
		os.Exit(claudeHook())
	}
}

// execGmuxd bridges the `gmux daemon …`, `gmux auth`, and `gmux remote`
// verbs to the gmuxd binary, which still owns the implementation. A
// later slice moves the logic into gmux and slims gmuxd to a pure
// serve binary (ADR 0009). Streams stdio so interactive flows (remote
// setup y/N, auth QR) work transparently.
func execGmuxd(args ...string) int {
	bin := findGmuxdBin()
	if bin == "" {
		fmt.Fprintln(os.Stderr, "gmux: gmuxd not found (install it alongside gmux or add it to PATH)")
		return 1
	}
	c := exec.Command(bin, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	return 0
}

// openUI implements the `gmux open` invocation: ensure gmuxd is up,
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
