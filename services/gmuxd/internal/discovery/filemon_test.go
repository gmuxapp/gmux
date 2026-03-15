package discovery

import "testing"

func TestSimilarityScoreExactMatch(t *testing.T) {
	score := similarityScore("hello world", "hello world")
	if score < 0.99 {
		t.Fatalf("expected ~1.0 for exact match, got %f", score)
	}
}

func TestSimilarityScorePartialMatch(t *testing.T) {
	// File tail is a substring of scrollback.
	score := similarityScore("fix the bug", "Let me fix the bug for you and also add tests")
	if score < 0.9 {
		t.Fatalf("expected high score for substring match, got %f", score)
	}
}

func TestSimilarityScoreNoMatch(t *testing.T) {
	score := similarityScore("aaaaa bbbbb ccccc", "xxxxx yyyyy zzzzz")
	if score > 0.2 {
		t.Fatalf("expected low score for no overlap, got %f", score)
	}
}

func TestSimilarityScoreEmpty(t *testing.T) {
	if similarityScore("", "hello") != 0 {
		t.Fatal("expected 0 for empty file tail")
	}
	if similarityScore("hello", "") != 0 {
		t.Fatal("expected 0 for empty scrollback")
	}
}

func TestLongestCommonSubstring(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"abcdef", "xbcdey", 4},     // "bcde"
		{"hello", "world", 1},        // "l" or "o"
		{"", "abc", 0},
		{"same", "same", 4},
		{"abc", "xyz", 0},
	}
	for _, tt := range tests {
		got := longestCommonSubstring(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("lcs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestTail(t *testing.T) {
	if tail("hello world", 5) != "world" {
		t.Fatal("expected 'world'")
	}
	if tail("hi", 10) != "hi" {
		t.Fatal("expected 'hi' when n > len")
	}
}
