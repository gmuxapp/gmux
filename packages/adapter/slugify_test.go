package adapter

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Fix the auth bug", "fix-the-auth-bug"},
		{"Hello, World!", "hello-world"},
		{"  leading/trailing  ", "leading-trailing"},
		{"UPPER CASE", "upper-case"},
		{"already-a-slug", "already-a-slug"},
		{"", ""},
		{"---", ""},
		{"a", "a"},
		// Truncation at 40 chars.
		{"this is a very long title that should be truncated to forty characters max", "this-is-a-very-long-title-that-should-be"},
		// No trailing hyphen after truncation.
		{"this is a very long title that should be-truncated", "this-is-a-very-long-title-that-should-be"},
		// UUID passthrough (still valid, just not pretty).
		{"019cf93a-c782-7942-ab76-010c81df6744", "019cf93a-c782-7942-ab76-010c81df6744"},
	}
	for _, tt := range tests {
		got := Slugify(tt.input)
		if got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsValidSlug(t *testing.T) {
	valid := []string{
		"a",
		"already-a-slug",
		"fix-the-auth-bug",
		"019cf93a-c782-7942-ab76-010c81df6744",
		strings.Repeat("a", maxSlugLen), // exactly at the cap
	}
	for _, s := range valid {
		if !IsValidSlug(s) {
			t.Errorf("IsValidSlug(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",
		"-leading",
		"trailing-",
		"double--hyphen",
		"UPPER",
		"has space",
		"foo/bar",
		"foo::bar",
		"foo\nbar",
		strings.Repeat("a", maxSlugLen+1), // one over the cap
	}
	for _, s := range invalid {
		if IsValidSlug(s) {
			t.Errorf("IsValidSlug(%q) = true, want false", s)
		}
	}

	// Slugify output always satisfies IsValidSlug, including for
	// over-length input that exercises the truncation path.
	long := strings.Repeat("word ", 30)
	if got := Slugify(long); !IsValidSlug(got) {
		t.Errorf("Slugify(long)=%q not a valid slug", got)
	}
}
