package centralstore

// Hot-path benchmarks for the daemon registration/placement write path.
// They pin the costs identified by the system resource profile:
//
//   - slug allocation used to run a full-table ListSessions hydration per
//     registration carrying a slug proposal (O(N) per call, O(N²) bulk);
//   - desiredScope used to scan every placement per placement per
//     rewritePlacements pass (O(P²) per insert/remove once children exist).

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func openBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(context.Background(), filepath.Join(b.TempDir(), "state"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	return s
}

// seedSluggedSessions inserts n sessions, each occupying a distinct slug on
// the "shell" adapter.
func seedSluggedSessions(b *testing.B, s *Store, n int) {
	b.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		v := NewSession{
			ID: SessionID(fmt.Sprintf("seed-%06d", i)), Adapter: "shell",
			Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{},
			CreatedAt: 1, SlugBase: fmt.Sprintf("seed-slug-%06d", i),
		}
		if _, _, err := s.InsertSession(ctx, v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRegisterRunnerSlugAllocation re-registers one session with a fresh
// slug proposal per iteration against a table of n slug-occupied rows. Table
// size stays fixed; only the allocation cost is exercised.
func BenchmarkRegisterRunnerSlugAllocation(b *testing.B) {
	for _, n := range []int{100, 2000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			ctx := context.Background()
			s := openBenchStore(b)
			seedSluggedSessions(b, s, n)
			reg := registrationB("bench-target", "shell", "/tmp", true, 10)
			if _, _, err := s.RegisterRunner(ctx, reg); err != nil {
				b.Fatal(err)
			}
			i := 0
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				i++
				slug := fmt.Sprintf("bench-slug-%09d", i)
				r := registrationB("bench-target", "shell", "/tmp", true, 10)
				r.Facts.Slug = &slug
				if _, _, err := s.RegisterRunner(ctx, r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRegisterRunnerSlugContended forces suffix probing: every iteration
// proposes an already-taken base so allocation must walk to a free "-k".
func BenchmarkRegisterRunnerSlugContended(b *testing.B) {
	ctx := context.Background()
	s := openBenchStore(b)
	// 50 sessions occupying taken, taken-2..taken-50.
	for i := 0; i < 50; i++ {
		slug := "taken"
		if i > 0 {
			slug = fmt.Sprintf("taken-%d", i+1)
		}
		v := NewSession{ID: SessionID(fmt.Sprintf("c-%03d", i)), Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{}, CreatedAt: 1, Slug: slug, SlugBase: slug}
		if _, _, err := s.InsertSession(ctx, v); err != nil {
			b.Fatal(err)
		}
	}
	if _, _, err := s.RegisterRunner(ctx, registrationB("bench-target", "shell", "/tmp", true, 10)); err != nil {
		b.Fatal(err)
	}
	i := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		i++
		// Alternate between two contended bases so replay short-circuit
		// (same base twice) never hides the probe.
		base := "taken"
		if i%2 == 0 {
			base = "taken-2"
		}
		r := registrationB("bench-target", "shell", "/tmp", true, 10)
		r.Facts.Slug = &base
		if _, _, err := s.RegisterRunner(ctx, r); err != nil {
			b.Fatal(err)
		}
	}
}

func registrationB(id, adapter, cwd string, alive bool, at UnixMillis) RunnerRegistration {
	cmd := []string{"sh"}
	remotes := map[string]string{}
	return RunnerRegistration{
		ID: SessionID(id), Adapter: adapter, Alive: alive, CreatedAt: at, ObservedAt: at,
		Facts: RunnerFacts{CWD: &cwd, Command: &cmd, Remotes: &remotes},
	}
}

// BenchmarkInsertSessionPlacementHeavy measures one InsertSession +
// RemoveSessionAtVersion pair (each renormalizes placements) against p
// placed sessions, half of which are children (parented placements are the
// path that made desiredScope O(P) per record).
func BenchmarkInsertSessionPlacementHeavy(b *testing.B) {
	for _, p := range []int{100, 2000} {
		b.Run(fmt.Sprintf("p=%d", p), func(b *testing.B) {
			ctx := context.Background()
			s := openBenchStore(b)
			if _, _, err := s.ReplaceProjectCatalog(ctx, []ProjectEntrySpec{{Owned: &OwnedProjectSpec{Slug: "p", Rules: []MatchRule{{Path: "/proj"}}}}}, 0); err != nil {
				b.Fatal(err)
			}
			// Roots then children: registration auto-places rows whose cwd
			// matches the project rule.
			for i := 0; i < p/2; i++ {
				root := registrationB(fmt.Sprintf("root-%05d", i), "shell", "/proj", true, UnixMillis(10+i))
				if _, _, err := s.RegisterRunner(ctx, root); err != nil {
					b.Fatal(err)
				}
				child := registrationB(fmt.Sprintf("child-%05d", i), "shell", "/proj", true, UnixMillis(10+i))
				pid := SessionID(fmt.Sprintf("root-%05d", i))
				child.ParentSessionID = &pid
				if _, _, err := s.RegisterRunner(ctx, child); err != nil {
					b.Fatal(err)
				}
			}
			all, err := placements(ctx, s.queries)
			if err != nil {
				b.Fatal(err)
			}
			if len(all) < p {
				b.Fatalf("expected >=%d placements, got %d", p, len(all))
			}
			i := 0
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				i++
				id := SessionID(fmt.Sprintf("bench-%09d", i))
				out, _, err := s.InsertSession(ctx, NewSession{ID: id, Adapter: "shell", Command: []string{"sh"}, CWD: "/proj", Remotes: map[string]string{}, CreatedAt: 1})
				if err != nil {
					b.Fatal(err)
				}
				if _, err := s.RemoveSessionAtVersion(ctx, id, out.Version); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
