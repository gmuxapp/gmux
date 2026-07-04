package sessionmeta

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// --- helpers -------------------------------------------------------

// writeDead persists a dead session with the given id, ExitedAt and
// SessionFile so retention tests can stage corpses on disk.
func writeDead(t *testing.T, s *Store, id, exitedAt, sessionFile string) {
	t.Helper()
	sess := store.Session{ID: id, Kind: "shell", Alive: false, ExitedAt: exitedAt, SessionFile: sessionFile}
	if err := s.Write(sess); err != nil {
		t.Fatalf("Write %s: %v", id, err)
	}
}

// writeScrollback stages a scrollback file of n bytes for session id
// with the given mtime (used to rank eviction order).
func writeScrollback(t *testing.T, s *Store, id string, n int, mtime time.Time) {
	t.Helper()
	p := filepath.Join(s.SessionDir(id), scrollback.ActiveName)
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		t.Fatalf("write scrollback %s: %v", id, err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", id, err)
	}
}

func loadedIDs(sessions []store.Session) map[string]bool {
	m := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		m[s.ID] = true
	}
	return m
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// --- scrollback cache cap -----------------------------------------

// TestPruneScrollbackEvictsOldestKeepsMeta pins the headline behavior:
// over the byte cap, the oldest scrollback is deleted while meta.json
// survives and newer scrollback is kept.
func TestPruneScrollbackEvictsOldestKeepsMeta(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 150}),
		WithClock(func() time.Time { return now }))

	writeDead(t, s, "sess-old", now.Add(-3*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "sess-new", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-old", 100, now.Add(-3*time.Hour))
	writeScrollback(t, s, "sess-new", 100, now.Add(-1*time.Hour)) // total 200 > 150

	s.PruneScrollback(nil)

	if exists(t, filepath.Join(s.SessionDir("sess-old"), scrollback.ActiveName)) {
		t.Error("oldest scrollback should be evicted")
	}
	if !exists(t, filepath.Join(s.SessionDir("sess-new"), scrollback.ActiveName)) {
		t.Error("newer scrollback should be kept (200-100=100 <= 150)")
	}
	// Meta survives for both.
	for _, id := range []string{"sess-old", "sess-new"} {
		if !exists(t, filepath.Join(s.SessionDir(id), metaFile)) {
			t.Errorf("%s meta.json must survive scrollback prune", id)
		}
	}
}

// TestPruneScrollbackCountsAndRemovesRotated pins that the rotated
// scrollback.0 file counts toward usage and is removed alongside the
// active file when a session is evicted.
func TestPruneScrollbackCountsAndRemovesRotated(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 150}),
		WithClock(func() time.Time { return now }))

	writeDead(t, s, "sess-a", now.Format(time.RFC3339), "")
	// 100 active + 100 rotated = 200 > 150, so this session is evicted.
	writeScrollback(t, s, "sess-a", 100, now)
	prev := filepath.Join(s.SessionDir("sess-a"), scrollback.PreviousName)
	if err := os.WriteFile(prev, make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write rotated: %v", err)
	}
	if err := os.Chtimes(prev, now, now); err != nil {
		t.Fatalf("chtimes rotated: %v", err)
	}

	s.PruneScrollback(nil)

	if exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Error("active scrollback should be removed")
	}
	if exists(t, prev) {
		t.Error("rotated scrollback.0 should be removed too")
	}
	if !exists(t, filepath.Join(s.SessionDir("sess-a"), metaFile)) {
		t.Error("meta.json must survive")
	}
}

// TestPruneScrollbackUnderCapNoop pins that nothing is touched when the
// aggregate is within budget.
func TestPruneScrollbackUnderCapNoop(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 1000}),
		WithClock(func() time.Time { return now }))
	writeDead(t, s, "sess-a", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-a", 100, now)

	s.PruneScrollback(nil)

	if !exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Error("scrollback under cap must not be evicted")
	}
}

// TestPruneScrollbackSkipsAlive pins the liveness guard: an alive
// session's scrollback is never evicted even when it's the oldest and
// the cap is exceeded.
func TestPruneScrollbackSkipsAlive(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 50}),
		WithClock(func() time.Time { return now }))

	writeDead(t, s, "sess-live", now.Add(-5*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "sess-dead", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-live", 100, now.Add(-5*time.Hour)) // oldest
	writeScrollback(t, s, "sess-dead", 100, now.Add(-1*time.Hour))

	s.PruneScrollback(map[string]bool{"sess-live": true})

	if !exists(t, filepath.Join(s.SessionDir("sess-live"), scrollback.ActiveName)) {
		t.Error("alive session scrollback must never be evicted")
	}
	// The dead one absorbs the eviction instead.
	if exists(t, filepath.Join(s.SessionDir("sess-dead"), scrollback.ActiveName)) {
		t.Error("dead session scrollback should be evicted to meet the cap")
	}
}

// TestPruneScrollbackZeroCapDisabled pins that a zero cap disables the
// pass entirely.
func TestPruneScrollbackZeroCapDisabled(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 0}),
		WithClock(func() time.Time { return now }))
	writeDead(t, s, "sess-a", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-a", 10_000, now)

	s.PruneScrollback(nil)

	if !exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Error("zero cap must disable scrollback eviction")
	}
}

// TestMaybePruneScrollbackThrottle pins the dismiss-time throttle: the
// first call runs, an immediate second call is skipped, and a call
// after the interval runs again.
func TestMaybePruneScrollbackThrottle(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := now
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 50}),
		WithClock(func() time.Time { return clock }))
	writeDead(t, s, "sess-a", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-a", 100, now)

	if !s.MaybePruneScrollback(nil, 12*time.Hour) {
		t.Fatal("first call should run (no stamp yet)")
	}
	clock = now.Add(time.Hour)
	if s.MaybePruneScrollback(nil, 12*time.Hour) {
		t.Error("call within interval should be throttled")
	}
	clock = now.Add(13 * time.Hour)
	if !s.MaybePruneScrollback(nil, 12*time.Hour) {
		t.Error("call past interval should run")
	}
}

// --- whole-dir age/count cap (conversation-less corpses) ----------

// TestSweepAgesOutConversationlessCorpse pins that a shell corpse older
// than MaxAge is removed entirely, while a conversation-backed dead
// session of the same age is exempt.
func TestSweepAgesOutConversationlessCorpse(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{MaxAge: 7 * 24 * time.Hour}),
		WithClock(func() time.Time { return now }))

	old := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	writeDead(t, s, "sess-shell", old, "")                        // no conversation: ages out
	writeDead(t, s, "sess-agent", old, "/home/u/.claude/x.jsonl") // conversation-backed: exempt

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	ids := loadedIDs(loaded)
	if ids["sess-shell"] {
		t.Error("conversation-less corpse should age out")
	}
	if !ids["sess-agent"] {
		t.Error("conversation-backed dead session must be exempt from age cap")
	}
	if exists(t, s.SessionDir("sess-shell")) {
		t.Error("aged-out corpse dir should be removed")
	}
	if !exists(t, s.SessionDir("sess-agent")) {
		t.Error("conversation-backed dir must survive")
	}
}

// TestSweepCountCapIgnoresConversationBacked pins that the count cap
// only ranks/evicts conversation-less corpses; conversation-backed
// sessions neither consume budget nor get evicted.
func TestSweepCountCapIgnoresConversationBacked(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{MaxCount: 1}),
		WithClock(func() time.Time { return now }))

	writeDead(t, s, "sess-shell-old", now.Add(-3*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "sess-shell-new", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "sess-agent", now.Add(-9*time.Hour).Format(time.RFC3339), "/x.jsonl")

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	ids := loadedIDs(loaded)
	if ids["sess-shell-old"] {
		t.Error("oldest corpse should be evicted by count cap")
	}
	if !ids["sess-shell-new"] {
		t.Error("newest corpse should be kept")
	}
	if !ids["sess-agent"] {
		t.Error("conversation-backed session must not count against or be evicted by the cap")
	}
}

// TestSweepNoPolicyKeepsEverything pins that the bare New (no retention)
// prunes nothing.
func TestSweepNoPolicyKeepsEverything(t *testing.T) {
	s := New(t.TempDir())
	writeDead(t, s, "sess-a", "2000-01-01T00:00:00Z", "")
	writeDead(t, s, "sess-b", "2000-01-02T00:00:00Z", "")

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(loaded) != 2 {
		t.Errorf("expected both kept without a policy, got %d", len(loaded))
	}
}

// TestSweepUndatableCorpseSurvives pins the conservative rule: a corpse
// with no parseable timestamp is never aged out and sorts as newest.
func TestSweepUndatableCorpseSurvives(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{MaxAge: time.Hour, MaxCount: 1}),
		WithClock(func() time.Time { return now }))

	writeDead(t, s, "sess-dated", now.Add(-10*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "sess-undated", "", "")

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	ids := loadedIDs(loaded)
	if !ids["sess-undated"] {
		t.Error("undatable corpse must survive retention")
	}
	if ids["sess-dated"] {
		t.Error("dated stale corpse should have been aged out")
	}
}

// --- env overrides -------------------------------------------------

func TestDefaultRetentionEnvOverride(t *testing.T) {
	t.Setenv(envRetentionDays, "5")
	t.Setenv(envRetentionCount, "10")
	t.Setenv(envScrollbackCacheMB, "64")
	p := DefaultRetention()
	if p.MaxAge != 5*24*time.Hour {
		t.Errorf("MaxAge: got %v, want 5d", p.MaxAge)
	}
	if p.MaxCount != 10 {
		t.Errorf("MaxCount: got %d, want 10", p.MaxCount)
	}
	if p.ScrollbackCacheBytes != 64<<20 {
		t.Errorf("ScrollbackCacheBytes: got %d, want 64MiB", p.ScrollbackCacheBytes)
	}

	t.Setenv(envRetentionDays, "0")
	t.Setenv(envScrollbackCacheMB, "0")
	if got := DefaultRetention().MaxAge; got != 0 {
		t.Errorf("days=0 should disable age limit, got %v", got)
	}
	if got := DefaultRetention().ScrollbackCacheBytes; got != 0 {
		t.Errorf("cache=0 should disable scrollback cap, got %d", got)
	}

	// Overflowing day count falls back to the default rather than
	// wrapping to a negative duration.
	t.Setenv(envRetentionDays, "100000000000")
	if got := DefaultRetention().MaxAge; got != DefaultMaxAge {
		t.Errorf("overflowing days should fall back to %v, got %v", DefaultMaxAge, got)
	}
}

// TestPruneScrollbackFailedRemoveKeepsEvicting pins the fix for the
// phantom-reclaim bug: when an older session's scrollback can't be
// removed (unwritable dir), total must not be decremented by its
// scan-time size, so a newer removable session still gets evicted to
// meet the cap instead of being skipped on a fake reclaim.
func TestPruneScrollbackFailedRemoveKeepsEvicting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 150}),
		WithClock(func() time.Time { return now }))

	writeDead(t, s, "sess-stuck", now.Add(-3*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "sess-evictable", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-stuck", 100, now.Add(-3*time.Hour))     // oldest, remove will fail
	writeScrollback(t, s, "sess-evictable", 100, now.Add(-1*time.Hour)) // total 200 > 150

	// Make the oldest session's dir unwritable so os.Remove fails.
	stuck := s.SessionDir("sess-stuck")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	s.PruneScrollback(nil)

	if !exists(t, filepath.Join(stuck, scrollback.ActiveName)) {
		t.Error("stuck scrollback should remain (remove failed)")
	}
	if exists(t, filepath.Join(s.SessionDir("sess-evictable"), scrollback.ActiveName)) {
		t.Error("evictable scrollback must still be reclaimed despite the earlier failure")
	}
}

// TestEvictScrollbackSkipsFreshlyWritten pins the write-freshness guard
// against the prune-vs-resume race: a victim whose scrollback mtime
// advanced past its scan-time value (someone — e.g. a just-resumed
// runner — wrote it after the scan) is skipped entirely, freeing 0.
func TestEvictScrollbackSkipsFreshlyWritten(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 1}),
		WithClock(func() time.Time { return now }))
	writeDead(t, s, "sess-a", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-a", 100, now)

	// Scan-time snapshot says the file is older than it now is.
	stale := scrollbackUsage{id: "sess-a", bytes: 100, mtime: now.Add(-time.Hour)}
	if freed := s.evictScrollback(stale); freed != 0 {
		t.Errorf("freshly-written victim must be skipped, freed %d", freed)
	}
	if !exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Error("freshly-written scrollback must survive")
	}

	// With an accurate snapshot the same victim is evicted.
	current := scrollbackUsage{id: "sess-a", bytes: 100, mtime: now}
	if freed := s.evictScrollback(current); freed != 100 {
		t.Errorf("accurate-snapshot victim should free 100, freed %d", freed)
	}
	if exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Error("scrollback should be evicted")
	}
}

// TestPruneScrollbackCoalesces pins the TryLock coalescing: a prune
// entering while another holds the lock must do nothing (no eviction,
// no stamp), and a later un-contended run must evict normally.
func TestPruneScrollbackCoalesces(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir(),
		WithRetention(RetentionPolicy{ScrollbackCacheBytes: 1}),
		WithClock(func() time.Time { return now }))
	writeDead(t, s, "sess-a", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "sess-a", 100, now)

	s.pruneMu.Lock()
	s.PruneScrollback(nil) // contended: must skip
	s.pruneMu.Unlock()

	if !exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Fatal("contended prune must not evict")
	}
	if exists(t, filepath.Join(s.Dir(), pruneStampFile)) {
		t.Error("contended prune must not advance the throttle stamp")
	}

	s.PruneScrollback(nil) // uncontended: evicts
	if exists(t, filepath.Join(s.SessionDir("sess-a"), scrollback.ActiveName)) {
		t.Error("uncontended prune should evict")
	}
}

// TestRemoveDeadBySessionFileDrivesWatchRemovals pins the seam between
// the store broadcast and the meta persister: retiring a dead session
// via RemoveDeadBySessionFile must, through the session-remove event
// consumed by WatchRemovals, delete the on-disk meta dir.
func TestRemoveDeadBySessionFileDrivesWatchRemovals(t *testing.T) {
	metaStore := New(t.TempDir())
	sess := store.Session{ID: "sess-dead1", Kind: "claude", Alive: false, SessionFile: "/c/conv.jsonl"}
	if err := metaStore.Write(sess); err != nil {
		t.Fatal(err)
	}

	sessions := store.New()
	sessions.Upsert(sess)
	events, cancel := sessions.Subscribe()
	var cancelOnce sync.Once
	stopWatch := func() { cancelOnce.Do(cancel) }
	defer stopWatch()
	done := make(chan struct{})
	go func() { metaStore.WatchRemovals(events); close(done) }()

	if ids := sessions.RemoveDeadBySessionFile("/c/conv.jsonl"); len(ids) != 1 {
		t.Fatalf("expected 1 retired session, got %v", ids)
	}

	deadline := time.After(2 * time.Second)
	for exists(t, metaStore.SessionDir("sess-dead1")) {
		select {
		case <-deadline:
			t.Fatal("meta dir not removed via WatchRemovals within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopWatch()
	<-done
}

// TestSweepSparesLiveRunnerScrollbackDir pins the fix for the
// restart-nukes-live-scrollback bug: a session dir containing only
// scrollback (no meta.json) belongs to a live, never-died runner —
// meta.json is written only on dead landings — or to a crash leftover
// that PruneScrollback's cap reclaims. Sweep must NOT RemoveAll it;
// doing so unlinks the runner's open scrollback file and sticky-breaks
// its next rotation, destroying the session's post-mortem replay on
// every daemon restart. Truly empty dirs are still swept.
func TestSweepSparesLiveRunnerScrollbackDir(t *testing.T) {
	s := New(t.TempDir())

	// Live runner's dir: scrollback only, no meta.json.
	liveDir := s.SessionDir("sess-live1")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sb := filepath.Join(liveDir, scrollback.ActiveName)
	if err := os.WriteFile(sb, []byte("live output"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Genuinely empty orphan dir: still swept.
	emptyDir := s.SessionDir("sess-empty1")
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("no meta.json anywhere; nothing should load, got %d", len(loaded))
	}
	if !exists(t, sb) {
		t.Error("live runner's scrollback must survive Sweep")
	}
	if exists(t, emptyDir) {
		t.Error("empty orphan dir should still be removed")
	}
}
