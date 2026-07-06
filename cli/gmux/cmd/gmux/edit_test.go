package main

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

func TestResolveFallbackEditor(t *testing.T) {
	t.Run("prefers nano", func(t *testing.T) {
		got, err := resolveFallbackEditor(noEnv, lookPathAllowing("nano", "vim", "vi"))
		if err != nil || strings.Join(got, " ") != "nano" {
			t.Errorf("got %v, %v; want [nano]", got, err)
		}
	})

	t.Run("falls through PATH order", func(t *testing.T) {
		got, err := resolveFallbackEditor(noEnv, lookPathAllowing("vi"))
		if err != nil || strings.Join(got, " ") != "vi" {
			t.Errorf("got %v, %v; want [vi]", got, err)
		}
	})

	t.Run("errors with hint when nothing found", func(t *testing.T) {
		_, err := resolveFallbackEditor(noEnv, lookPathAllowing())
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
		got, err := resolveFallbackEditor(env, lookPathAllowing("vim", "nano"))
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
		_, err := resolveFallbackEditor(env, lookPathAllowing("nano"))
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
		got, err := resolveFallbackEditor(env, lookPathAllowing("nano"))
		if err != nil || strings.Join(got, " ") != "nano" {
			t.Errorf("got %v, %v; want [nano]", got, err)
		}
	})
}
