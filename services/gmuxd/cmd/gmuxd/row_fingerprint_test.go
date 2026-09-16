package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// The reflective fingerprint must agree with the JSON oracle on "changed or
// not" for every single-field mutation of a realistic row, and must not
// collide across the corpus.
func TestFingerprintAgreesWithJSONOracle(t *testing.T) {
	base := wire.Session{
		ID: "abc", Title: "t", Cwd: "/x", Command: []string{"pi", "--flag"}, Alive: true, SemanticAgent: true,
		Status: &wire.Status{Active: true}, DescendantCounts: &wire.DescendantCounts{Total: 3, Alive: 1},
		Remotes: map[string]string{"origin": "github.com/a/b"}, ProjectIndex: 4, CreatedAt: "2026-01-01T00:00:00Z",
	}
	mutations := []func(*wire.Session){
		func(s *wire.Session) { s.Title = "u" },
		func(s *wire.Session) { s.Alive = false },
		func(s *wire.Session) { s.Status = nil },
		func(s *wire.Session) { s.Status = &wire.Status{Active: false} },
		func(s *wire.Session) { s.DescendantCounts.Unread = 1 },
		func(s *wire.Session) { s.DescendantCounts = nil },
		func(s *wire.Session) { s.Command = append([]string{}, "pi") },
		func(s *wire.Session) { s.Command = nil },
		func(s *wire.Session) { s.Remotes = map[string]string{"origin": "github.com/a/c"} },
		func(s *wire.Session) { s.Remotes = nil },
		func(s *wire.Session) { s.ProjectIndex = 5 },
		func(s *wire.Session) { s.Unread = true },
		func(s *wire.Session) { s.ParentSessionID = "p" },
		func(s *wire.Session) { s.Peer = "hs" },
	}
	if fingerprintSession(base) != fingerprintSession(base) {
		t.Fatal("not deterministic")
	}
	for i, m := range mutations {
		s := base
		if s.DescendantCounts != nil {
			c := *s.DescendantCounts
			s.DescendantCounts = &c
		}
		m(&s)
		jsonChanged := hashSessionJSON(s) != hashSessionJSON(base)
		fpChanged := fingerprintSession(s) != fingerprintSession(base)
		if jsonChanged != fpChanged {
			t.Fatalf("mutation %d: json changed=%v fingerprint changed=%v", i, jsonChanged, fpChanged)
		}
	}
	// Framing: concatenation cannot alias.
	a := wire.Session{Title: "ab", Subtitle: "c"}
	b := wire.Session{Title: "a", Subtitle: "bc"}
	if fingerprintSession(a) == fingerprintSession(b) {
		t.Fatal("string framing collision")
	}
	// Whole corpus, when available: no collisions, and random mutations agree.
	path := os.Getenv("GMUX_SPIKE_CORPUS")
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skip(err)
	}
	var rows []wire.Session
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]string{}
	for _, r := range rows {
		h := fingerprintSession(r)
		if prev, dup := seen[h]; dup && prev != r.ID {
			t.Fatalf("collision %s vs %s", prev, r.ID)
		}
		seen[h] = r.ID
	}
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		r := rows[rnd.Intn(len(rows))]
		s := r
		switch rnd.Intn(4) {
		case 0:
			s.Title += "!"
		case 1:
			s.Alive = !s.Alive
		case 2:
			s.LastOutputAt = "2030-01-01T00:00:00Z"
		default:
			s.Unread = !s.Unread
		}
		if (hashSessionJSON(s) != hashSessionJSON(r)) != (fingerprintSession(s) != fingerprintSession(r)) {
			t.Fatalf("corpus row %s: oracle disagreement", r.ID)
		}
	}
}
