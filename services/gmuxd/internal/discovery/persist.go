package discovery

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// attributionFilePath returns the path for persisted attributions,
// colocated with the session sockets in the gmux-sessions directory.
func attributionFilePath() string {
	return filepath.Join(socketDir(), "attributions.json")
}

// persistedAttribution is the on-disk format for a single file attribution.
type persistedAttribution struct {
	SessionID string `json:"session_id"`
	Kind      string `json:"kind,omitempty"`
}

// loadAttributions reads persisted attributions from the default path.
func loadAttributions() map[string]string {
	return loadAttributionsFrom(attributionFilePath())
}

// loadAttributionsFrom reads persisted attributions from a specific path.
// Returns nil on any error (missing file, parse error, etc.).
func loadAttributionsFrom(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw map[string]persistedAttribution
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("filemon: failed to parse %s: %v", path, err)
		return nil
	}
	result := make(map[string]string, len(raw))
	for filePath, attr := range raw {
		result[filePath] = attr.SessionID
	}
	log.Printf("filemon: loaded %d persisted attribution(s) from %s", len(result), path)
	return result
}

// saveAttributions writes current attributions to the default path.
func saveAttributions(attributions map[string]string, sessions map[string]*monitoredSession) {
	saveAttributionsTo(attributionFilePath(), attributions, sessions)
}

// saveAttributionsTo writes current attributions to a specific path.
// Only persists attributions for sessions that are actively monitored,
// pruning stale entries from sessions that no longer exist.
// Writes atomically via rename to avoid partial reads.
func saveAttributionsTo(path string, attributions map[string]string, sessions map[string]*monitoredSession) {
	activeSessionIDs := make(map[string]bool, len(sessions))
	for id, ms := range sessions {
		if ms != nil {
			activeSessionIDs[id] = true
		}
	}

	raw := make(map[string]persistedAttribution, len(attributions))
	for filePath, sessionID := range attributions {
		if !activeSessionIDs[sessionID] {
			continue
		}
		attr := persistedAttribution{SessionID: sessionID}
		if ms := sessions[sessionID]; ms != nil {
			attr.Kind = ms.kind
		}
		raw[filePath] = attr
	}

	data, err := json.Marshal(raw)
	if err != nil {
		log.Printf("filemon: failed to marshal attributions: %v", err)
		return
	}

	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		log.Printf("filemon: mkdir %s: %v", filepath.Dir(tmp), err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("filemon: failed to write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("filemon: failed to rename %s → %s: %v", tmp, path, err)
		os.Remove(tmp)
	}
}
