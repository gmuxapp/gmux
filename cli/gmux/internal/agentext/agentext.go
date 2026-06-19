// Package agentext ships the gmux pi session extension (pi-ext.mjs) and the
// helper the runner uses to materialize it.
//
// Where the agent-shim (package agentshim) infers the active conversation
// from fs syscalls, this extension gets it authoritatively from pi's own
// session lifecycle events (start/switch/fork) and forwards the session file
// to the runner. It is loaded via `pi -e <path>`; see pi-ext.mjs for the
// design comment.
//
// The .mjs source is embedded and materialized to a stable, content-addressed
// path on disk (same scheme as agentshim) so a single gmux binary self-heals
// across upgrades and the file stays inspectable.
package agentext

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "embed"
)

//go:embed pi-ext.mjs
var extSource []byte

var (
	once    sync.Once
	path    string
	loadErr error
)

// Path materializes the embedded extension to a stable, content-addressed
// file under the user cache dir and returns its absolute path. Subsequent
// calls in the same process return the cached result.
func Path() (string, error) {
	once.Do(func() { path, loadErr = materialize() })
	return path, loadErr
}

func materialize() (string, error) {
	sum := sha256.Sum256(extSource)
	short := hex.EncodeToString(sum[:])[:12]

	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "gmux", "agent-ext")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agentext: mkdir %s: %w", dir, err)
	}
	p := filepath.Join(dir, fmt.Sprintf("pi-ext-%s.mjs", short))

	if data, err := os.ReadFile(p); err == nil && string(data) == string(extSource) {
		return p, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, extSource, 0o644); err != nil {
		return "", fmt.Errorf("agentext: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("agentext: rename %s: %w", p, err)
	}
	return p, nil
}
