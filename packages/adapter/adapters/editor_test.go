package adapters

import (
	"errors"
	"strings"
	"testing"
)

// lookPathAllowing returns a lookPath stub that succeeds only for the
// given names.
func lookPathAllowing(names ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

func noEnv(string) string { return "" }

func TestEditorMatch(t *testing.T) {
	e := NewEditor()
	match := [][]string{
		{"gmux", "__edit-child"},
		{"gmux", "__edit-child", "/tmp/notes.txt"},
		{"/usr/local/bin/gmux", "__edit-child", "/tmp/notes.txt"},
	}
	for _, cmd := range match {
		if !e.Match(cmd) {
			t.Errorf("Match(%v) = false, want true", cmd)
		}
	}
	noMatch := [][]string{
		{},
		{"gmux"},
		{"nano", "/tmp/notes.txt"},          // plain editor run stays on shell adapter
		{"gmux", "edit", "/tmp/notes.txt"},  // outer verb, not the sentinel
		{"other", "__edit-child", "/tmp/x"}, // not gmux
	}
	for _, cmd := range noMatch {
		if e.Match(cmd) {
			t.Errorf("Match(%v) = true, want false", cmd)
		}
	}
}

func TestEditorCommandTitle(t *testing.T) {
	e := NewEditor()
	if got := e.CommandTitle([]string{"gmux", "__edit-child", "/home/x/COMMIT_EDITMSG"}); got != "COMMIT_EDITMSG" {
		t.Errorf("title = %q, want COMMIT_EDITMSG", got)
	}
	if got := e.CommandTitle([]string{"gmux", "__edit-child"}); got != "editor" {
		t.Errorf("title = %q, want editor", got)
	}
}

func TestEditorOnRegister(t *testing.T) {
	e := NewEditor()
	info, err := e.OnRegister("id1", "/tmp", []string{"gmux", "__edit-child", "/tmp/My Notes.TXT"})
	if err != nil || info.Slug != "my-notes-txt" {
		t.Errorf("slug = %q, err = %v; want my-notes-txt", info.Slug, err)
	}
	info, err = e.OnRegister("id2", "/tmp", []string{"gmux", "__edit-child"})
	if err != nil || info.Slug != "editor" {
		t.Errorf("slug = %q, err = %v; want editor", info.Slug, err)
	}
}

func TestEditorRegisteredWithLauncher(t *testing.T) {
	if FindByAdapter("editor") == nil {
		t.Fatal("editor adapter not registered in All")
	}
	ls := NewEditor().Launchers()
	if len(ls) != 1 || ls[0].ID != "editor" || strings.Join(ls[0].Command, " ") != "gmux __edit-child" {
		t.Errorf("launchers = %+v", ls)
	}
}

func TestResolveFallbackEditor(t *testing.T) {
	t.Run("prefers nano", func(t *testing.T) {
		got, err := ResolveFallbackEditor(noEnv, lookPathAllowing("nano", "vim", "vi"))
		if err != nil || strings.Join(got, " ") != "nano" {
			t.Errorf("got %v, %v; want [nano]", got, err)
		}
	})

	t.Run("falls through PATH order", func(t *testing.T) {
		got, err := ResolveFallbackEditor(noEnv, lookPathAllowing("vi"))
		if err != nil || strings.Join(got, " ") != "vi" {
			t.Errorf("got %v, %v; want [vi]", got, err)
		}
	})

	t.Run("errors with hint when nothing found", func(t *testing.T) {
		_, err := ResolveFallbackEditor(noEnv, lookPathAllowing())
		if err == nil || !strings.Contains(err.Error(), "GMUX_EDIT_FALLBACK") {
			t.Errorf("want error mentioning GMUX_EDIT_FALLBACK, got %v", err)
		}
	})

	t.Run("GMUX_EDIT_FALLBACK wins and may carry flags", func(t *testing.T) {
		env := func(k string) string {
			if k == "GMUX_EDIT_FALLBACK" {
				return "vim -u NONE"
			}
			return ""
		}
		got, err := ResolveFallbackEditor(env, lookPathAllowing("vim", "nano"))
		if err != nil || strings.Join(got, " ") != "vim -u NONE" {
			t.Errorf("got %v, %v; want [vim -u NONE]", got, err)
		}
	})

	t.Run("GMUX_EDIT_FALLBACK missing from PATH errors", func(t *testing.T) {
		env := func(k string) string {
			if k == "GMUX_EDIT_FALLBACK" {
				return "myeditor"
			}
			return ""
		}
		_, err := ResolveFallbackEditor(env, lookPathAllowing("nano"))
		if err == nil || !strings.Contains(err.Error(), "myeditor") {
			t.Errorf("want error naming myeditor, got %v", err)
		}
	})

	// $EDITOR must never be consulted: `EDITOR="gmux edit"` is the
	// primary use case and reading it back would recurse.
	t.Run("ignores EDITOR", func(t *testing.T) {
		env := func(k string) string {
			if k == "EDITOR" {
				return "gmux edit"
			}
			return ""
		}
		got, err := ResolveFallbackEditor(env, lookPathAllowing("nano"))
		if err != nil || strings.Join(got, " ") != "nano" {
			t.Errorf("got %v, %v; want [nano]", got, err)
		}
	})
}
