package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// openBrowser opens the gmux UI. Prefers Chrome/Chromium in --app mode
// for a standalone window; falls back to the default browser.
func openBrowser(url string) {
	// Try any installed Chromium-based browser in --app mode for a
	// standalone window. Skip reading com.apple.launchservices.secure.plist
	// to detect the *default* browser — that plist read triggers the macOS
	// Sonoma "access data from other apps" privacy prompt every time.
	if tryAnyChromiumAppMode(url) {
		return
	}

	// Fallback: default browser (normal tab).
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

// tryAnyChromiumAppMode finds any installed Chromium-based browser and
// launches it with --app.
func tryAnyChromiumAppMode(url string) bool {
	switch runtime.GOOS {
	case "darwin":
		// macOS: Chrome.app doesn't put a binary on $PATH.
		// Check known .app bundle locations directly.
		home, _ := os.UserHomeDir()
		appDirs := []string{"/Applications", filepath.Join(home, "Applications")}
		for _, app := range []string{"Google Chrome", "Chromium"} {
			for _, dir := range appDirs {
				binary := filepath.Join(dir, app+".app", "Contents", "MacOS", app)
				if _, err := os.Stat(binary); err == nil {
					if startDetached(exec.Command(binary, "--app="+url)) {
						return true
					}
				}
			}
		}
	default:
		for _, name := range []string{"google-chrome-stable", "google-chrome", "chromium-browser", "chromium"} {
			if p, err := exec.LookPath(name); err == nil {
				if startDetached(exec.Command(p, "--app="+url)) {
					return true
				}
			}
		}
	}
	return false
}

// startDetached starts a command in a new session so it outlives gmux.
func startDetached(cmd *exec.Cmd) bool {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start() == nil
}

// upgradeHint returns the appropriate upgrade command based on how gmux was installed.
func upgradeHint() string {
	self, err := os.Executable()
	if err != nil {
		return "curl -sSfL https://gmux.app/install.sh | sh"
	}
	// Resolve symlinks to find the real binary location
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		real = self
	}
	// Check if we're inside a Homebrew prefix
	if strings.Contains(real, "/Cellar/") || strings.Contains(real, "/homebrew/") {
		return "brew upgrade gmuxapp/tap/gmux"
	}
	return "curl -sSfL https://gmux.app/install.sh | sh"
}

// maskTailscaleURL masks the tailnet name for privacy.
// "https://gmux.angler-map.ts.net" → "https://gmux.an******.ts.net"
func maskTailscaleURL(url string) string {
	// Find the tailnet part: between first dot after hostname and .ts.net
	tsNet := ".ts.net"
	idx := strings.Index(url, tsNet)
	if idx < 0 {
		return url
	}
	// Find the start of the tailnet name (after "https://gmux.")
	schemeEnd := strings.Index(url, "://")
	if schemeEnd < 0 {
		return url
	}
	hostStart := schemeEnd + 3
	// Find first dot after the hostname prefix
	dotIdx := strings.Index(url[hostStart:], ".")
	if dotIdx < 0 {
		return url
	}
	tailnetStart := hostStart + dotIdx + 1
	tailnetName := url[tailnetStart:idx]
	if len(tailnetName) <= 2 {
		return url
	}
	masked := tailnetName[:2] + "****"
	return url[:tailnetStart] + masked + url[idx:]
}
