// Package nodeid manages the stable, opaque per-node identifier gmuxd
// uses to answer "is this the same node?" across reconnects, address
// changes, and connection methods — independent of the node's mutable
// human-readable hostname. See ADR 0007.
//
// The id is generated once on first start and persisted to the state
// directory alongside the auth token, so it survives daemon restarts
// (and container recreation, when the state directory is on a persisted
// volume — the same requirement the auth token and the tailscale node
// key already impose).
//
// It is opaque: never shown in the UI and never part of a URL. It is
// not a secret — the auth token is the credential — but it is generated
// with crypto/rand so it cannot be cheaply forged to impersonate
// another node during peer dedup.
package nodeid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// idBytes is the number of random bytes in a generated id (256 bits).
	// Matches authtoken so both share one generate-and-persist shape;
	// far more than collision-resistance requires.
	idBytes = 32

	// prefix tags the id for easy recognition in logs and /v1/health,
	// mirroring the "sess-" session-id convention.
	prefix = "node_"

	// fileName is the name of the id file in the state directory.
	fileName = "node-id"
)

// LoadOrCreate returns this node's stable id, generating and persisting
// it on first call.
//
//   - file present and valid: return it.
//   - file absent: generate, persist (0600), return it.
//   - file present but corrupted: hard error. Silently regenerating
//     would change the node's identity and split it from peers that
//     already know it, so we refuse rather than guess.
func LoadOrCreate(stateDir string) (string, error) {
	path := filepath.Join(stateDir, fileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		id := strings.TrimSpace(string(data))
		if !valid(id) {
			return "", fmt.Errorf("nodeid: %s is corrupted; remove the file and restart", path)
		}
		return id, nil
	case os.IsNotExist(err):
		return generate(path)
	default:
		return "", fmt.Errorf("nodeid: reading %s: %w", path, err)
	}
}

// valid reports whether id is a well-formed node id ("node_" + 64 hex).
func valid(id string) bool {
	hexPart, ok := strings.CutPrefix(id, prefix)
	if !ok || len(hexPart) != idBytes*2 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

func generate(path string) (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("nodeid: generating random id: %w", err)
	}
	id := prefix + hex.EncodeToString(b)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("nodeid: creating state dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("nodeid: writing %s: %w", path, err)
	}
	return id, nil
}
