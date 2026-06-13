package projects

import (
	"errors"
	"testing"
)

func TestManagerAddProject_Success(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	item, err := m.AddProject("apps", []MatchRule{{Path: "/mnt/user/apps"}})
	if err != nil {
		t.Fatalf("AddProject: unexpected error: %v", err)
	}
	if item.Slug != "apps" {
		t.Fatalf("slug = %q, want %q", item.Slug, "apps")
	}

	// The project must actually be persisted.
	s, err := m.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Items) != 1 || s.Items[0].Slug != "apps" {
		t.Fatalf("persisted items = %#v, want one 'apps'", s.Items)
	}
}

func TestManagerAddProject_UniqueSlug(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	if _, err := m.AddProject("apps", []MatchRule{{Path: "/mnt/user/apps"}}); err != nil {
		t.Fatalf("first AddProject: %v", err)
	}
	// Different path, same base slug → slug is deduplicated, add succeeds.
	item, err := m.AddProject("apps", []MatchRule{{Path: "/srv/apps"}})
	if err != nil {
		t.Fatalf("second AddProject: %v", err)
	}
	if item.Slug != "apps-2" {
		t.Fatalf("slug = %q, want %q", item.Slug, "apps-2")
	}
}

// The regression guard: adding a project whose path duplicates an
// existing one must be rejected with a *ValidationError AND must not
// persist anything. Previously the handler reported success on this
// aborted save, letting clients pin references to a phantom slug.
func TestManagerAddProject_DuplicatePathRejected(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	if _, err := m.AddProject("apps", []MatchRule{{Path: "/mnt/user/apps"}}); err != nil {
		t.Fatalf("seed AddProject: %v", err)
	}

	// Same path again (e.g. a remote-grouped discovered suggestion that
	// resolves to an already-owned path) → must be rejected.
	item, err := m.AddProject("apps", []MatchRule{
		{Remote: "github.com/acme/apps"},
		{Path: "/mnt/user/apps"},
	})
	if err == nil {
		t.Fatalf("AddProject succeeded, want rejection (item=%#v)", item)
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error type = %T (%v), want *ValidationError", err, err)
	}
	if item.Slug != "" {
		t.Fatalf("item should be zero on rejection, got %#v", item)
	}

	// Nothing must have been persisted beyond the seed project.
	s, err := m.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Items) != 1 || s.Items[0].Slug != "apps" {
		t.Fatalf("persisted items = %#v, want only the seed 'apps'", s.Items)
	}
}
