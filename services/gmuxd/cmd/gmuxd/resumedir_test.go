package main

import (
	"path/filepath"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// newProjectMgr writes a projects.json with a single owned project whose
// canonical dir is canonicalPath, then returns a loaded manager.
func newProjectMgr(t *testing.T, canonicalPath string) *projects.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr := projects.NewManager(dir)
	if _, err := mgr.AddProject("proj", []projects.MatchRule{{Path: canonicalPath}}); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	return mgr
}

func TestResolveResumeDir_CwdExists(t *testing.T) {
	cwd := t.TempDir()
	canon := t.TempDir()
	mgr := newProjectMgr(t, canon)
	sess := store.Session{Cwd: cwd, ProjectSlug: "proj"}

	dir, fellBack := resolveResumeDir(mgr, sess)
	if dir != cwd || fellBack {
		t.Errorf("got (%q, %v), want (%q, false)", dir, fellBack, cwd)
	}
}

func TestResolveResumeDir_CwdMissing_FallsToCanonical(t *testing.T) {
	canon := t.TempDir()
	mgr := newProjectMgr(t, canon)
	gone := filepath.Join(t.TempDir(), "deleted-grove")
	sess := store.Session{Cwd: gone, ProjectSlug: "proj"}

	dir, fellBack := resolveResumeDir(mgr, sess)
	if dir != canon || !fellBack {
		t.Errorf("got (%q, %v), want (%q, true)", dir, fellBack, canon)
	}
}

func TestResolveResumeDir_CanonicalAlsoMissing_FallsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	goneCanon := filepath.Join(t.TempDir(), "gone-canon")
	mgr := newProjectMgr(t, goneCanon)
	goneCwd := filepath.Join(t.TempDir(), "gone-cwd")
	sess := store.Session{Cwd: goneCwd, ProjectSlug: "proj"}

	dir, fellBack := resolveResumeDir(mgr, sess)
	if dir != home || !fellBack {
		t.Errorf("got (%q, %v), want (%q, true)", dir, fellBack, home)
	}
}

func TestResolveResumeDir_NothingExists(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "gone-home"))
	goneCanon := filepath.Join(t.TempDir(), "gone-canon")
	mgr := newProjectMgr(t, goneCanon)
	sess := store.Session{Cwd: filepath.Join(t.TempDir(), "gone-cwd"), ProjectSlug: "proj"}

	dir, fellBack := resolveResumeDir(mgr, sess)
	if dir != "" || fellBack {
		t.Errorf("got (%q, %v), want (\"\", false)", dir, fellBack)
	}
}

func TestRelaunchData(t *testing.T) {
	// Common path: cwd was used, no fallback fields leak into the payload.
	d := relaunchData("sess-1", 42, "/orig", "/orig", false)
	if d["pid"] != 42 || d["session_id"] != "sess-1" {
		t.Errorf("missing core fields: %v", d)
	}
	if _, ok := d["original_cwd"]; ok {
		t.Errorf("original_cwd should be absent without fallback: %v", d)
	}
	if _, ok := d["fallback_cwd"]; ok {
		t.Errorf("fallback_cwd should be absent without fallback: %v", d)
	}

	// Fallback path: original + fallback dirs surfaced for the toast layer.
	d = relaunchData("sess-1", 42, "/gone", "/canon", true)
	if d["original_cwd"] != "/gone" || d["fallback_cwd"] != "/canon" {
		t.Errorf("expected fallback fields, got %v", d)
	}
}

func TestResolveResumeDir_NoProject_CwdMissingFallsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mgr := projects.NewManager(t.TempDir()) // empty: no owning project
	sess := store.Session{Cwd: filepath.Join(t.TempDir(), "gone")}

	dir, fellBack := resolveResumeDir(mgr, sess)
	if dir != home || !fellBack {
		t.Errorf("got (%q, %v), want (%q, true)", dir, fellBack, home)
	}
}
