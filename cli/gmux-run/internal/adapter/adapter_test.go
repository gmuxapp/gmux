package adapter

import (
	"os"
	"testing"
)

// --- Registry tests ---

type testAdapter struct {
	name    string
	matches bool
}

func (a *testAdapter) Name() string                                    { return a.name }
func (a *testAdapter) Match(_ []string) bool                           { return a.matches }
func (a *testAdapter) Prepare(ctx PrepareContext) ([]string, []string)  { return ctx.Command, nil }
func (a *testAdapter) Monitor(_ []byte) *Status                        { return nil }

func TestRegistryFallback(t *testing.T) {
	r := NewRegistry()
	r.SetFallback(&testAdapter{name: "generic", matches: true})
	a := r.Resolve([]string{"unknown"})
	if a.Name() != "generic" {
		t.Fatalf("expected 'generic' fallback, got %q", a.Name())
	}
}

func TestRegistryFirstMatch(t *testing.T) {
	r := NewRegistry()
	r.SetFallback(&testAdapter{name: "generic", matches: true})
	r.Register(&testAdapter{name: "pi", matches: true})
	r.Register(&testAdapter{name: "opencode", matches: true})
	a := r.Resolve([]string{"pi"})
	if a.Name() != "pi" {
		t.Fatalf("expected 'pi' (first match), got %q", a.Name())
	}
}

func TestRegistrySkipNonMatch(t *testing.T) {
	r := NewRegistry()
	r.SetFallback(&testAdapter{name: "generic", matches: true})
	r.Register(&testAdapter{name: "pi", matches: false})
	r.Register(&testAdapter{name: "pytest", matches: true})
	a := r.Resolve([]string{"pytest"})
	if a.Name() != "pytest" {
		t.Fatalf("expected 'pytest', got %q", a.Name())
	}
}

func TestRegistryEnvOverride(t *testing.T) {
	r := NewRegistry()
	r.SetFallback(&testAdapter{name: "generic", matches: true})
	r.Register(&testAdapter{name: "pi", matches: false})
	r.Register(&testAdapter{name: "pytest", matches: true})

	os.Setenv("GMUX_ADAPTER", "pi")
	defer os.Unsetenv("GMUX_ADAPTER")

	a := r.Resolve([]string{"anything"})
	if a.Name() != "pi" {
		t.Fatalf("expected 'pi' from env override, got %q", a.Name())
	}
}

func TestRegistryEnvOverrideUnknown(t *testing.T) {
	r := NewRegistry()
	r.SetFallback(&testAdapter{name: "generic", matches: true})
	r.Register(&testAdapter{name: "pi", matches: false})

	os.Setenv("GMUX_ADAPTER", "nonexistent")
	defer os.Unsetenv("GMUX_ADAPTER")

	// Should fall through to matching, then fallback
	a := r.Resolve([]string{"anything"})
	if a.Name() != "generic" {
		t.Fatalf("expected 'generic' fallback for unknown override, got %q", a.Name())
	}
}
