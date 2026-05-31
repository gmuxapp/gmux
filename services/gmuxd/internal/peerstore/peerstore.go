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
		out = append(out, config.PeerConfig{Name: r.Name, URL: r.URL, Token: r.Token})
	}
	return out
}

// AddOrGet atomically connects a host: if a record with the same
// (non-empty) node_id already exists it is returned with existed=true
// (the same host reached again is not a duplicate); otherwise the
// record's name is slugified, de-collided, appended, and persisted.
//
// The node_id check and the append happen under one lock, so two
// concurrent connects to the same host can't both pass the dedup and
// create duplicates (no check-then-act race).
func (s *Store) AddOrGet(rec Record) (stored Record, existed bool, err error) {
	if err := ValidateURL(rec.URL); err != nil {
		return Record{}, false, err
	}
	name := Slugify(rec.Name)
	if name == "" {
		return Record{}, false, fmt.Errorf("host name %q has no usable slug characters", rec.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec.NodeID != "" {
		for _, r := range s.records {
			if r.NodeID == rec.NodeID {
				return r, true, nil
			}
		}
	}

	taken := make(map[string]bool, len(s.records))
	for _, r := range s.records {
		taken[r.Name] = true
	}
	rec.Name = uniqueName(name, taken)

	s.records = append(s.records, rec)
	if err := s.save(); err != nil {
		s.records = s.records[:len(s.records)-1]
		return Record{}, false, err
	}
	return rec, false, nil
}

// Remove deletes the record with the given name and persists. Returns
// the removed record and whether one was found.
func (s *Store) Remove(name string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.records {
		if r.Name == name {
			s.records = append(s.records[:i], s.records[i+1:]...)
			if err := s.save(); err != nil {
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
	if err := os.WriteFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("peerstore: writing %s: %w", s.path, err)
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
