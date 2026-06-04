// Package peerstore persists the set of manually-added peers (ADR 0007
// §5). With `[[peers]]` removed from host.toml, a peer the user connects
// to via the "Connect to host" flow is durable state, not config: it
// lives in peers.json in the state directory (0600 — it can carry a
// bearer token), distinct from the user-authored config file.
//
// Auto-discovered peers (tailscale, devcontainers) are NOT stored here;
// they remain ephemeral/runtime. This file is exactly "the hosts you
// explicitly connected to".
//
// Each record carries the remote's opaque node_id (ADR 0007 §4) so the
// add flow can recognize a host it already knows — reached via a
// different address or already auto-discovered — instead of adding a
// duplicate.
package peerstore

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// fileName is the peers file within the state directory.
const fileName = "peers.json"

// Record is one manually-added peer.
type Record struct {
	// Name is the viewer-facing identity used in routing (/@name/). It is
	// the remote's self-reported name, suffixed (`name-2`) only on a
	// genuine collision with a different node.
	Name string `json:"name"`
	// URL is the base address used to reach the peer.
	URL string `json:"url"`
	// Token is the bearer token, if the peer requires one. Empty for
	// peers authenticated via tailnet identity.
	Token string `json:"token,omitempty"`
	// NodeID is the remote's opaque node id (ADR 0007 §4), used to detect
	// "same host" regardless of address or connection method.
	NodeID string `json:"node_id,omitempty"`
}

// nonSlug matches runs of characters that aren't slug-safe.
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify lowercases and reduces a host's self-reported name to a
// URL-safe routing slug (the name appears in `/@name/`). Returns ""
// when nothing usable remains.
func Slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// ValidateURL reports whether u is an acceptable peer base URL.
func ValidateURL(u string) error {
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", u, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("url %q must use http or https", u)
	}
	if parsed.Host == "" {
		return fmt.Errorf("url %q has no host", u)
	}
	return nil
}

// Store is the in-memory + on-disk set of manual peers. Safe for
// concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	records []Record
}

// Open loads peers.json from stateDir, returning an empty store if the
// file is absent.
func Open(stateDir string) (*Store, error) {
	s := &Store{path: filepath.Join(stateDir, fileName)}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("peerstore: reading %s: %w", s.path, err)
	}
	// An empty file (e.g. a truncated write from an older build that
	// didn't persist atomically) is treated as an empty store rather
	// than a fatal parse error, so the daemon can still start.
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.records); err != nil {
		return nil, fmt.Errorf("peerstore: parsing %s: %w", s.path, err)
	}
	return s, nil
}

// List returns a copy of the current records.
func (s *Store) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// PeerConfigs converts the records to peering configs for the manager.
func (s *Store) PeerConfigs() []config.PeerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]config.PeerConfig, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, config.PeerConfig{Name: r.Name, URL: r.URL, Token: r.Token, Source: config.SourceManual})
	}
	return out
}

// AddOutcome reports what AddOrGet did to the store.
type AddOutcome int

const (
	// Added: a brand-new record was appended.
	Added AddOutcome = iota
	// Updated: an existing record matched and its URL/token (and any
	// newly-learned node_id) were refreshed in place.
	Updated
	// Unchanged: an existing record matched and nothing differed.
	Unchanged
)

// AddOrGet upserts a host. It matches an existing record by node_id (the
// durable identity) or, when that misses, by URL — so supplying a token
// for a host added without one (e.g. one migrated from autodiscovery,
// whose node_id isn't known until it first authenticates) refreshes that
// record in place instead of creating a duplicate. On a match it updates
// the URL, token, and (if newly learned) node_id while keeping the
// existing display name; otherwise the name is slugified, de-collided,
// appended, and persisted.
//
// Matching and the append happen under one lock, so two concurrent
// connects to the same host can't both pass the dedup and create
// duplicates (no check-then-act race).
func (s *Store) AddOrGet(rec Record) (stored Record, outcome AddOutcome, err error) {
	if err := ValidateURL(rec.URL); err != nil {
		return Record{}, Added, err
	}
	name := Slugify(rec.Name)
	if name == "" {
		return Record{}, Added, fmt.Errorf("host name %q has no usable slug characters", rec.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if i := s.matchIndex(rec); i >= 0 {
		cur := s.records[i]
		// Empty Token/NodeID mean "unknown", not "clear it": only a non-empty
		// value updates the stored one. This keeps the legacy-discovery
		// import (token-less) from wiping the token of a host that was also
		// added manually, and there's no flow that deliberately blanks a
		// token. URL is always present (validated) so it always updates.
		changed := cur.URL != rec.URL ||
			(rec.Token != "" && cur.Token != rec.Token) ||
			(rec.NodeID != "" && cur.NodeID != rec.NodeID)
		if !changed {
			return cur, Unchanged, nil
		}
		s.records[i].URL = rec.URL
		if rec.Token != "" {
			s.records[i].Token = rec.Token
		}
		if rec.NodeID != "" {
			s.records[i].NodeID = rec.NodeID
		}
		updated := s.records[i]
		if err := s.save(); err != nil {
			s.records[i] = cur // roll back the in-memory change
			return Record{}, Added, err
		}
		return updated, Updated, nil
	}

	taken := make(map[string]bool, len(s.records))
	for _, r := range s.records {
		taken[r.Name] = true
	}
	rec.Name = uniqueName(name, taken)

	s.records = append(s.records, rec)
	if err := s.save(); err != nil {
		s.records = s.records[:len(s.records)-1]
		return Record{}, Added, err
	}
	return rec, Added, nil
}

// matchIndex returns the index of the record representing the same host
// as rec — same node_id when known, else same URL — or -1 if none.
// Caller holds s.mu.
func (s *Store) matchIndex(rec Record) int {
	if rec.NodeID != "" {
		for i, r := range s.records {
			if r.NodeID == rec.NodeID {
				return i
			}
		}
	}
	for i, r := range s.records {
		if sameURL(r.URL, rec.URL) {
			return i
		}
	}
	return -1
}

// sameURL reports whether two peer base URLs address the same host,
// ignoring a trailing slash and case.
func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// legacyDiscoveryFile is the pre-2.0 tailscale autodiscovery cache.
const legacyDiscoveryFile = "tailscale-discovery.json"

// legacyDiscoveryState / legacyDeviceEntry mirror the shape the removed
// tsdiscovery package (ADR 0008) wrote to tailscale-discovery.json. They
// exist only to migrate that cache's gmux entries into the peer store on
// the first upgrade start.
type legacyDiscoveryState struct {
	Devices map[string]*legacyDeviceEntry `json:"devices"`
}

type legacyDeviceEntry struct {
	FQDN     string `json:"fqdn"`
	PeerName string `json:"peer_name"`
	IsGmux   bool   `json:"is_gmux"`
}

// ImportLegacyDiscovery one-time-migrates the pre-2.0 autodiscovery
// cache into the peer store: a cached gmux device becomes a manual peer
// with no token (ADR 0008 removed autodiscovery, so the user
// re-authenticates each host by supplying its token) — but only if a
// project references it (its slugified name is in referenced). Those are
// the hosts whose references would otherwise orphan; other gmux boxes
// the tailnet once surfaced are left out rather than cluttering the
// roster with "auth needed" rows, and can be re-added on demand. The
// cache is removed afterward so the migration runs once regardless of
// how many hosts matched. Returns the number of hosts newly added; an
// absent cache is (0, nil).
func (s *Store) ImportLegacyDiscovery(stateDir string, referenced map[string]bool) (int, error) {
	path := filepath.Join(stateDir, legacyDiscoveryFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("peerstore: reading %s: %w", path, err)
	}

	var st legacyDiscoveryState
	if err := json.Unmarshal(data, &st); err != nil {
		// Unparseable cache: nothing to salvage. Drop it and move on.
		_ = os.Remove(path)
		return 0, nil
	}

	imported := 0
	for _, d := range st.Devices {
		if d == nil || !d.IsGmux || d.PeerName == "" || d.FQDN == "" {
			continue
		}
		// Only carry forward hosts a project references; skip gmux boxes
		// the tailnet surfaced but the user never pinned anything on.
		if !referenced[Slugify(d.PeerName)] {
			continue
		}
		// No token, no node_id: the host lands as "auth needed" until the
		// user supplies its token, which upserts this record by URL.
		_, outcome, err := s.AddOrGet(Record{Name: d.PeerName, URL: "https://" + d.FQDN})
		if err != nil {
			log.Printf("peerstore: skipping legacy peer %q: %v", d.PeerName, err)
			continue
		}
		if outcome == Added {
			imported++
		}
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("peerstore: could not remove legacy %s: %v", legacyDiscoveryFile, err)
	}
	return imported, nil
}

// Remove deletes the record with the given name and persists. Returns
// the removed record and whether one was found.
func (s *Store) Remove(name string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.records {
		if r.Name == name {
			// Build a fresh slice so the original backing array is left
			// intact for rollback if the disk write fails (mirrors
			// AddOrGet — keeps the in-memory store consistent with disk).
			prev := s.records
			next := make([]Record, 0, len(prev)-1)
			next = append(next, prev[:i]...)
			next = append(next, prev[i+1:]...)
			s.records = next
			if err := s.save(); err != nil {
				s.records = prev
				return Record{}, false, err
			}
			return r, true, nil
		}
	}
	return Record{}, false, nil
}

// save writes the records to disk (0600). Caller holds s.mu.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return fmt.Errorf("peerstore: encoding: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("peerstore: creating state dir: %w", err)
	}
	// Write-to-tmp-then-rename so a crash mid-write can never leave a
	// truncated/0-byte peers.json (rename is atomic; matches
	// projects.go and discovery/persist.go).
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("peerstore: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("peerstore: renaming %s: %w", s.path, err)
	}
	return nil
}

// uniqueName returns base if free, else base-2, base-3, … (ADR 0007 §7:
// a genuine collision between two distinct hosts is resolved viewer-side
// by suffixing).
func uniqueName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}
