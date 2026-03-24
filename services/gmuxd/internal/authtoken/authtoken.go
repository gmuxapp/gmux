// Package authtoken manages the bearer token used to authenticate
// connections on the network listener.
//
// The token is a 32-byte random value hex-encoded (64 characters).
// It is persisted to the state directory so it survives daemon restarts.
// The file is created with 0600 permissions.
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
	// tokenBytes is the number of random bytes in a token.
	tokenBytes = 32
	// fileName is the name of the token file in the state directory.
	fileName = "auth-token"
)

// LoadOrCreate reads the token from stateDir/auth-token.
// If the file does not exist, a new token is generated and written.
// Returns the token string and any error.
func LoadOrCreate(stateDir string) (string, error) {
	path := filepath.Join(stateDir, fileName)

	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if err := validateFormat(token); err != nil {
			// Corrupted file; regenerate.
			return generate(path)
		}
		return token, nil
	}

	if !os.IsNotExist(err) {
		return "", fmt.Errorf("authtoken: reading %s: %w", path, err)
	}

	return generate(path)
}

// Equal compares two tokens in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func generate(path string) (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("authtoken: generating random token: %w", err)
	}

	token := hex.EncodeToString(b)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("authtoken: creating state dir: %w", err)
	}

	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("authtoken: writing %s: %w", path, err)
	}

	return token, nil
}

func validateFormat(token string) error {
	if len(token) != tokenBytes*2 {
		return fmt.Errorf("bad length %d, want %d", len(token), tokenBytes*2)
	}
	if _, err := hex.DecodeString(token); err != nil {
		return fmt.Errorf("not valid hex: %w", err)
	}
	return nil
}
