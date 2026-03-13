package adapter

import "os"

// Registry holds adapters in priority order and resolves which one
// handles a given command.
type Registry struct {
	adapters []Adapter
	fallback Adapter
}

// NewRegistry creates an empty registry. Callers must register adapters
// and set a fallback before use.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an adapter to the registry. Adapters are tried in
// registration order; first match wins.
func (r *Registry) Register(a Adapter) {
	r.adapters = append(r.adapters, a)
}

// SetFallback sets the catch-all adapter used when nothing matches.
func (r *Registry) SetFallback(a Adapter) {
	r.fallback = a
}

// Resolve picks the adapter for the given command.
//
// Priority:
//  1. GMUX_ADAPTER env var — explicit override, bypass matching
//  2. Walk registered adapters, first Match() wins
//  3. Fallback (generic)
func (r *Registry) Resolve(command []string) Adapter {
	// Tier 1: explicit override
	if name := os.Getenv("GMUX_ADAPTER"); name != "" {
		for _, a := range r.adapters {
			if a.Name() == name {
				return a
			}
		}
		// Unknown name — fall through gracefully
	}

	// Tier 2: auto-match
	for _, a := range r.adapters {
		if a.Match(command) {
			return a
		}
	}

	// Tier 3: fallback
	if r.fallback != nil {
		return r.fallback
	}

	// Should never happen if registry is properly initialized
	return nil
}
