// Package authtoken manages the bearer token used to authenticate
// connections on the network listener.
//
// Auto-generated tokens are 32-byte random values hex-encoded (64
// characters). User-provided tokens (via GMUXD_TOKEN or file) must be
// at least 64 hex characters. The token is persisted to the state
// directory so it survives daemon restarts. The file is created with
// 0600 permissions.
//
// On startup, the GMUXD_TOKEN environment variable can seed the token
// file. If the file already exists, the env var must match or startup
// fails. The env var is unset immediately on read to limit leakage to
// child processes.
package authtoken

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// tokenBytes is the number of random bytes in an auto-generated token.
	tokenBytes = 32

	// minTokenLen is the minimum length for a user-provided token (hex chars).
	// 64 hex chars = 32 bytes = 256 bits. Matches auto-generated tokens.
	minTokenLen = 64

	// fileName is the name of the token file in the state directory.
	fileName = "auth-token"

	// envVar is the environment variable that can seed the token file.
	envVar = "GMUXD_TOKEN"
)

// LoadOrCreate resolves the auth token for this daemon instance.
//
// It reads GMUXD_TOKEN (unsetting it immediately to limit child process
// leakage) and the token file, then reconciles:
//
//   - env set, no file: validate env, write to disk, return it.
//   - env set, file matches: return file token.
//   - env set, file differs: return an error (refuse to start).
//   - env not set, file present: return file token.
//   - env not set, no file: generate a random token, write to disk.
//
// A corrupted or malformed token file is always a hard error.
func LoadOrCreate(stateDir string) (string, error) {
	path := filepath.Join(stateDir, fileName)

	// ── Read env var ──

	envToken, envErr := readEnv()

	// ── Read file ──

	fileToken, fileErr := readFile(path)

	// ── Reconcile ──

	switch {
	case envErr != nil:
		// Bad env var: fail regardless of file state.
		return "", envErr

	case envToken != "" && fileErr != nil && !os.IsNotExist(fileErr):
		// Env var set, file exists but is corrupted/unreadable.
		return "", fmt.Errorf("authtoken: %w", fileErr)

	case envToken != "" && fileToken != "":
		// Both present: must match.
		if !Equal(envToken, fileToken) {
			return "", fmt.Errorf(
				"authtoken: %s value does not match existing token in %s; "+
					"either remove the file or fix the environment variable",
				envVar, path,
			)
		}
		return fileToken, nil

	case envToken != "" && fileToken == "":
		// Env var set, no valid file: seed the file.
		if err := writeFile(path, envToken); err != nil {
			return "", err
		}
		return envToken, nil

	case fileErr != nil && !os.IsNotExist(fileErr):
		// No env var, file exists but is corrupted/unreadable.
		return "", fmt.Errorf("authtoken: %w", fileErr)

	case fileToken != "":
		// No env var, valid file: use it.
		return fileToken, nil

	default:
		// No env var, no file: generate.
		return generate(path)
	}
}

// Equal compares two tokens in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// readEnv reads and validates GMUXD_TOKEN, then unsets it.
// Returns ("", nil) if the variable is not set.
func readEnv() (string, error) {
	raw, ok := os.LookupEnv(envVar)
	os.Unsetenv(envVar)

	if !ok || raw == "" {
		return "", nil
	}

	token := strings.TrimSpace(raw)
	if err := validateFormat(token); err != nil {
		return "", fmt.Errorf("authtoken: %s: %w", envVar, err)
	}
	return token, nil
}

// readFile reads and validates the token file.
// Returns ("", os.ErrNotExist) if the file doesn't exist.
// Returns a descriptive error for corrupted or unreadable files.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", err
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	token := strings.TrimSpace(string(data))
	if err := validateFormat(token); err != nil {
		return "", fmt.Errorf("%s is corrupted (%w); remove the file and restart, or set %s", path, err, envVar)
	}
	return token, nil
}

func writeFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("authtoken: creating state dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("authtoken: writing %s: %w", path, err)
	}
	return nil
}

func generate(path string) (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("authtoken: generating random token: %w", err)
	}

	token := hex.EncodeToString(b)
	if err := writeFile(path, token); err != nil {
		return "", err
	}
	return token, nil
}

func validateFormat(token string) error {
	if len(token) < minTokenLen {
		return fmt.Errorf("token too short (%d chars, minimum %d)", len(token), minTokenLen)
	}
	if _, err := hex.DecodeString(token); err != nil {
		return fmt.Errorf("token is not valid hex: %w", err)
	}
	return nil
}
