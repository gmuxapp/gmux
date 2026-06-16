// Package agentshim ships the gmux agent shim (hook.mjs) and the helpers
// the runner uses to inject it into node/bun agent processes.
//
// The shim is a tiny JS preload that wraps the agent's fs write surface and
// posts session-file writes back to the runner's unix socket, giving gmux an
// authoritative answer to "which conversation file does this runner hold?"
// instead of guessing via terminal scrollback. See hook.mjs for the full
// design comment.
//
// The .mjs source is embedded so a single gmux binary can materialize it to a
// stable, readable path on disk (Node's --import / Bun's --preload need a real
// file path). The on-disk copy is content-addressed so it self-heals across
// upgrades and stays inspectable by users.
package agentshim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "embed"
)

//go:embed hook.mjs
var hookSource []byte

var (
	materializeOnce sync.Once
	materializePath string
	materializeErr  error
)

// Path materializes the embedded shim to a stable, content-addressed file
// under the user cache dir and returns its absolute path. Subsequent calls
// in the same process return the cached result. The filename includes a
// short content hash so a new shim version writes a new file rather than
// racing a running agent that still references the old path.
func Path() (string, error) {
	materializeOnce.Do(func() {
		materializePath, materializeErr = materialize()
	})
	return materializePath, materializeErr
}

func materialize() (string, error) {
	sum := sha256.Sum256(hookSource)
	short := hex.EncodeToString(sum[:])[:12]

	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "gmux", "agent-shim")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agentshim: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("hook-%s.mjs", short))

	// Idempotent: only write if missing or content drifted (e.g. a
	// truncated prior write). The hash in the name means a correct file
	// is always correct, so a plain Stat short-circuits the common case.
	if data, err := os.ReadFile(path); err == nil && string(data) == string(hookSource) {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, hookSource, 0o644); err != nil {
		return "", fmt.Errorf("agentshim: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("agentshim: rename %s: %w", path, err)
	}
	return path, nil
}

// PreloadEnv returns env with the shim wired in: GMUX_RUNNER_SOCK set to the
// runner's socket, and the runtime preload flag appended (append-safe) to
// both NODE_OPTIONS and BUN_OPTIONS. Both vars are set because the runner
// doesn't know whether the agent will run under node or bun; each runtime
// honours its own and ignores the other.
//
// Appending (rather than overwriting) preserves any flags the user already
// set upstream. The shim deletes these vars from its own process.env on
// startup so child processes the agent spawns don't inherit them.
func PreloadEnv(env []string, shimPath, sockPath string) []string {
	out := make([]string, 0, len(env)+1)
	seenNode, seenBun := false, false
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "NODE_OPTIONS="):
			out = append(out, appendFlag(e, "--import file://"+shimPath))
			seenNode = true
		case strings.HasPrefix(e, "BUN_OPTIONS="):
			out = append(out, appendFlag(e, "--preload "+shimPath))
			seenBun = true
		case strings.HasPrefix(e, "GMUX_RUNNER_SOCK="):
			// Drop any inherited value; we set our own below.
		default:
			out = append(out, e)
		}
	}
	if !seenNode {
		out = append(out, "NODE_OPTIONS=--import file://"+shimPath)
	}
	if !seenBun {
		out = append(out, "BUN_OPTIONS=--preload "+shimPath)
	}
	out = append(out, "GMUX_RUNNER_SOCK="+sockPath)
	return out
}

// appendFlag appends flag to a "KEY=value" env entry, preserving the key and
// any existing value.
func appendFlag(entry, flag string) string {
	eq := strings.IndexByte(entry, '=')
	key, val := entry[:eq], entry[eq+1:]
	val = strings.TrimSpace(val)
	if val == "" {
		return key + "=" + flag
	}
	return key + "=" + val + " " + flag
}
