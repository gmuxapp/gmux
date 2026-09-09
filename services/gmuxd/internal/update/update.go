// Package update checks for new gmux releases in the background.
package update

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/buildversion"
)

const (
	repo          = "gmuxapp/gmux"
	checkInterval = 12 * time.Hour
)

// Checker periodically polls GitHub for the latest gmux release.
// It is safe to call Available from any goroutine.
type Checker struct {
	current string
	client  *http.Client

	mu     sync.RWMutex
	latest string // empty until first successful check
}

// New starts a background checker. current is the running version (e.g. "v0.4.6").
// If current is a development version the checker does nothing: no goroutine,
// no network.
func New(current string) *Checker {
	return newChecker(current, nil)
}

// newChecker is New with an injectable transport, so a test can observe
// whether the checker went to the network at all — the property New's dev-build
// branch exists for. A nil transport means http.DefaultTransport.
func newChecker(current string, transport http.RoundTripper) *Checker {
	c := &Checker{
		current: current,
		client:  &http.Client{Timeout: 10 * time.Second, Transport: transport},
	}
	if buildversion.IsDev(current) {
		return c
	}
	go c.loop()
	return c
}

// Available returns the latest version string if it is newer than current,
// or "" if we are up to date (or haven't checked yet).
func (c *Checker) Available() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

func (c *Checker) loop() {
	c.check()
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for range ticker.C {
		c.check()
	}
}

func (c *Checker) check() {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	resp, err := c.client.Get(url)
	if err != nil {
		return // silent — network may be unavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil || release.TagName == "" {
		return
	}

	if newer(release.TagName, c.current) {
		c.mu.Lock()
		c.latest = release.TagName
		c.mu.Unlock()
		log.Printf("update: %s available (current: %s)", release.TagName, c.current)
	} else {
		c.mu.Lock()
		c.latest = ""
		c.mu.Unlock()
	}
}

// newer returns true if a is a higher semver than b.
// Both may optionally have a "v" prefix.
func newer(a, b string) bool {
	pa := parseSemver(strings.TrimPrefix(a, "v"))
	pb := parseSemver(strings.TrimPrefix(b, "v"))
	if pa == nil || pb == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] > pb[i] {
			return true
		}
		if pa[i] < pb[i] {
			return false
		}
	}
	return false
}

// parseSemver parses "1.2.3" into [1, 2, 3]. Returns nil on failure.
func parseSemver(s string) []int {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	var out [3]int
	for i, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out[:]
}
