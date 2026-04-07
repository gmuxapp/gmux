package projects

import (
	"encoding/json"
	"log"

	"github.com/gmuxapp/gmux/packages/paths"
)

// migrateState transforms a raw JSON document from any older schema version
// to the current version. Each step is a self-contained function that
// operates on the generic JSON representation (map[string]any), avoiding
// coupling to the current Go struct definitions.
//
// Version history:
//   - (no version field): original format. Items have "remote" (string)
//     and "paths" ([]string) as separate top-level fields.
//   - 2: unified match rules. Items have "match" ([]MatchRule) instead of
//     separate "remote" and "paths". Paths may use ~ for $HOME.
func migrateState(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	version := 0
	if v, ok := doc["version"].(float64); ok {
		version = int(v)
	}

	if version < 2 {
		migrateV1toV2(doc)
	}

	doc["version"] = float64(currentVersion)
	return json.Marshal(doc)
}

// migrateV1toV2 converts the original format (separate "remote" + "paths"
// fields per item) to unified "match" rules.
//
// Before:
//
//	{"slug": "gmux", "remote": "github.com/gmuxapp/gmux", "paths": ["/home/mg/dev/gmux"]}
//
// After:
//
//	{"slug": "gmux", "match": [{"remote": "github.com/gmuxapp/gmux"}, {"path": "~/dev/gmux"}]}
//
// The "remote" and "paths" fields are removed. Sessions are preserved as-is.
func migrateV1toV2(doc map[string]any) {
	items, ok := doc["items"].([]any)
	if !ok {
		return
	}

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		// Skip items that already have "match" (e.g. version field was
		// missing but the data was already in v2 format).
		if _, hasMatch := item["match"]; hasMatch {
			continue
		}

		var rules []any

		// Convert "remote" field to a match rule.
		if remote, ok := item["remote"].(string); ok && remote != "" {
			rules = append(rules, map[string]any{"remote": remote})
		}
		delete(item, "remote")

		// Convert each "paths" entry to a match rule, canonicalizing
		// absolute paths (e.g. /home/mg/dev/gmux → ~/dev/gmux).
		if rawPaths, ok := item["paths"].([]any); ok {
			for _, p := range rawPaths {
				if s, ok := p.(string); ok && s != "" {
					rules = append(rules, map[string]any{"path": paths.CanonicalizePath(s)})
				}
			}
		}
		delete(item, "paths")

		item["match"] = rules
	}

	log.Printf("projects: migrated v1 → v2 (%d items)", len(items))
}
