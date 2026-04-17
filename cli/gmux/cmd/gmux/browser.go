package main

import (
	"encoding/json"
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

	// Strategy: default browser if Chromium-based → app mode, else
	// any installed Chromium → app mode, else system default.
	if tryDefaultBrowserAppMode(url) {
		return
	}
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

// tryDefaultBrowserAppMode checks if the user's default browser is
// Chromium-based and launches it in --app mode.
func tryDefaultBrowserAppMode(url string) bool {
	switch runtime.GOOS {
	case "darwin":
		bundleID := defaultBrowserBundleID()
		if binary, ok := macOSChromiumBinary(bundleID); ok {
			return startDetached(exec.Command(binary, "--app="+url))
		}
	default:
		desktop := defaultDesktopBrowser()
		if isChromiumDesktop(desktop) {
			// The default browser is Chromium-based — xdg-open won't pass
			// --app, but the binary should be on PATH with a known name.
			return tryAnyChromiumAppMode(url)
		}
	}
	return false
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

// --- default browser detection ---

// defaultBrowserBundleID returns the macOS bundle ID of the default
// HTTPS handler (e.g. "com.google.chrome"). Returns "" if Safari is
// the implicit default or detection fails.
func defaultBrowserBundleID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	plistPath := filepath.Join(home,
		"Library", "Preferences", "com.apple.LaunchServices",
		"com.apple.launchservices.secure.plist")
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", plistPath).Output()
	if err != nil {
		return ""
	}
	var plist struct {
		LSHandlers []struct {
			URLScheme string `json:"LSHandlerURLScheme"`
			RoleAll   string `json:"LSHandlerRoleAll"`
		} `json:"LSHandlers"`
	}
	if err := json.Unmarshal(out, &plist); err != nil {
		return ""
	}
	for _, h := range plist.LSHandlers {
		if strings.EqualFold(h.URLScheme, "https") {
			return h.RoleAll
		}
	}
	return "" // Safari is implicit default
}

// macOSChromiumBinary maps a bundle ID to its binary path if it's a
// known Chromium-based browser.
func macOSChromiumBinary(bundleID string) (string, bool) {
	// Map bundle IDs → .app names for known Chromium-based browsers.
	appNames := map[string]string{
		"com.google.chrome":          "Google Chrome",
		"org.chromium.chromium":      "Chromium",
		"company.thebrowser.browser": "Arc",
		"com.brave.browser":          "Brave Browser",
		"com.microsoft.edgemac":      "Microsoft Edge",
	}
	appName, ok := appNames[strings.ToLower(bundleID)]
	if !ok {
		return "", false
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
		binary := filepath.Join(dir, appName+".app", "Contents", "MacOS", appName)
		if _, err := os.Stat(binary); err == nil {
			return binary, true
		}
	}
	return "", false
}

// defaultDesktopBrowser returns the .desktop file name of the default
// web browser on Linux (e.g. "google-chrome.desktop").
func defaultDesktopBrowser() string {
	out, err := exec.Command("xdg-settings", "get", "default-web-browser").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isChromiumDesktop returns true if the .desktop name looks Chromium-based.
func isChromiumDesktop(desktop string) bool {
	d := strings.ToLower(desktop)
	return strings.Contains(d, "chrome") || strings.Contains(d, "chromium")
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
