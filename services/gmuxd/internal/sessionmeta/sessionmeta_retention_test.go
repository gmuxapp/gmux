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
// ConversationRef so retention tests can stage corpses on disk.
func writeDead(t *testing.T, s *Store, id, exitedAt, conversationFile string) {
	t.Helper()
	sess := store.Session{ID: id, Adapter: "shell", Alive: false, ExitedAt: exitedAt, ConversationRef: conversationFile}
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

	writeDead(t, s, "10aobhpt", now.Add(-3*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "1vx41244", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeScrollback(t, s, "10aobhpt", 100, now.Add(-3*time.Hour))
	writeScrollback(t, s, "1vx41244", 100, now.Add(-1*time.Hour)) // total 200 > 150

	s.PruneScrollback(nil)

	if exists(t, filepath.Join(s.SessionDir("10aobhpt"), scrollback.ActiveName)) {
		t.Error("oldest scrollback should be evicted")
	}
	if !exists(t, filepath.Join(s.SessionDir("1vx41244"), scrollback.ActiveName)) {
		t.Error("newer scrollback should be kept (200-100=100 <= 150)")
	}
	// Meta survives for both.
	for _, id := range []string{"10aobhpt", "1vx41244"} {
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

	writeDead(t, s, "1mw5c5n9", now.Format(time.RFC3339), "")
	// 100 active + 100 rotated = 200 > 150, so this session is evicted.
	writeScrollback(t, s, "1mw5c5n9", 100, now)
	prev := filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.PreviousName)
	if err := os.WriteFile(prev, make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write rotated: %v", err)
	}
	if err := os.Chtimes(prev, now, now); err != nil {
		t.Fatalf("chtimes rotated: %v", err)
	}

	s.PruneScrollback(nil)

	if exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
		t.Error("active scrollback should be removed")
	}
	if exists(t, prev) {
		t.Error("rotated scrollback.0 should be removed too")
	}
	if !exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), metaFile)) {
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
	writeDead(t, s, "1mw5c5n9", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "1mw5c5n9", 100, now)

	s.PruneScrollback(nil)

	if !exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
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

	writeDead(t, s, "184lbyqm", now.Add(-5*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "1eha7rdu", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeScrollback(t, s, "184lbyqm", 100, now.Add(-5*time.Hour)) // oldest
	writeScrollback(t, s, "1eha7rdu", 100, now.Add(-1*time.Hour))

	s.PruneScrollback(map[string]bool{"184lbyqm": true})

	if !exists(t, filepath.Join(s.SessionDir("184lbyqm"), scrollback.ActiveName)) {
		t.Error("alive session scrollback must never be evicted")
	}
	// The dead one absorbs the eviction instead.
	if exists(t, filepath.Join(s.SessionDir("1eha7rdu"), scrollback.ActiveName)) {
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
	writeDead(t, s, "1mw5c5n9", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "1mw5c5n9", 10_000, now)

	s.PruneScrollback(nil)

	if !exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
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
	writeDead(t, s, "1mw5c5n9", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "1mw5c5n9", 100, now)

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
	writeDead(t, s, "1sf8bw26", old, "")                        // no conversation: ages out
	writeDead(t, s, "1dhy39b9", old, "/home/u/.claude/x.jsonl") // conversation-backed: exempt

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	ids := loadedIDs(loaded)
	if ids["1sf8bw26"] {
		t.Error("conversation-less corpse should age out")
	}
	if !ids["1dhy39b9"] {
		t.Error("conversation-backed dead session must be exempt from age cap")
	}
	if exists(t, s.SessionDir("1sf8bw26")) {
		t.Error("aged-out corpse dir should be removed")
	}
	if !exists(t, s.SessionDir("1dhy39b9")) {
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

	writeDead(t, s, "1f7q3vsi", now.Add(-3*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "1kbpp1us", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "1dhy39b9", now.Add(-9*time.Hour).Format(time.RFC3339), "/x.jsonl")

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	ids := loadedIDs(loaded)
	if ids["1f7q3vsi"] {
		t.Error("oldest corpse should be evicted by count cap")
	}
	if !ids["1kbpp1us"] {
		t.Error("newest corpse should be kept")
	}
	if !ids["1dhy39b9"] {
		t.Error("conversation-backed session must not count against or be evicted by the cap")
	}
}

// TestSweepNoPolicyKeepsEverything pins that the bare New (no retention)
// prunes nothing.
func TestSweepNoPolicyKeepsEverything(t *testing.T) {
	s := New(t.TempDir())
	writeDead(t, s, "1mw5c5n9", "2000-01-01T00:00:00Z", "")
	writeDead(t, s, "18wnzse2", "2000-01-02T00:00:00Z", "")

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

	writeDead(t, s, "1fjqsipi", now.Add(-10*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "1p9a38qc", "", "")

	loaded, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	ids := loadedIDs(loaded)
	if !ids["1p9a38qc"] {
		t.Error("undatable corpse must survive retention")
	}
	if ids["1fjqsipi"] {
		t.Error("dated stale corpse should have been aged out")
	}
}

func TestDefaultRetention(t *testing.T) {
	p := DefaultRetention()
	if p.MaxAge != DefaultMaxAge || p.MaxCount != DefaultMaxCount || p.ScrollbackCacheBytes != DefaultScrollbackCacheBytes {
		t.Fatalf("DefaultRetention() = %+v", p)
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

	writeDead(t, s, "1586ep4g", now.Add(-3*time.Hour).Format(time.RFC3339), "")
	writeDead(t, s, "1mi4w464", now.Add(-1*time.Hour).Format(time.RFC3339), "")
	writeScrollback(t, s, "1586ep4g", 100, now.Add(-3*time.Hour)) // oldest, remove will fail
	writeScrollback(t, s, "1mi4w464", 100, now.Add(-1*time.Hour)) // total 200 > 150

	// Make the oldest session's dir unwritable so os.Remove fails.
	stuck := s.SessionDir("1586ep4g")
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	s.PruneScrollback(nil)

	if !exists(t, filepath.Join(stuck, scrollback.ActiveName)) {
		t.Error("stuck scrollback should remain (remove failed)")
	}
	if exists(t, filepath.Join(s.SessionDir("1mi4w464"), scrollback.ActiveName)) {
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
	writeDead(t, s, "1mw5c5n9", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "1mw5c5n9", 100, now)

	// Scan-time snapshot says the file is older than it now is.
	stale := scrollbackUsage{id: "1mw5c5n9", bytes: 100, mtime: now.Add(-time.Hour)}
	if freed := s.evictScrollback(stale); freed != 0 {
		t.Errorf("freshly-written victim must be skipped, freed %d", freed)
	}
	if !exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
		t.Error("freshly-written scrollback must survive")
	}

	// With an accurate snapshot the same victim is evicted.
	current := scrollbackUsage{id: "1mw5c5n9", bytes: 100, mtime: now}
	if freed := s.evictScrollback(current); freed != 100 {
		t.Errorf("accurate-snapshot victim should free 100, freed %d", freed)
	}
	if exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
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
	writeDead(t, s, "1mw5c5n9", now.Format(time.RFC3339), "")
	writeScrollback(t, s, "1mw5c5n9", 100, now)

	s.pruneMu.Lock()
	s.PruneScrollback(nil) // contended: must skip
	s.pruneMu.Unlock()

	if !exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
		t.Fatal("contended prune must not evict")
	}
	if exists(t, filepath.Join(s.Dir(), pruneStampFile)) {
		t.Error("contended prune must not advance the throttle stamp")
	}

	s.PruneScrollback(nil) // uncontended: evicts
	if exists(t, filepath.Join(s.SessionDir("1mw5c5n9"), scrollback.ActiveName)) {
		t.Error("uncontended prune should evict")
	}
}

// TestRemoveDeadByConversationRefDrivesWatchRemovals pins the seam between
// the store broadcast and the meta persister: retiring a dead session
// via RemoveDeadByConversationRef must, through the session-remove event
// consumed by WatchRemovals, delete the on-disk meta dir.
func TestRemoveDeadByConversationRefDrivesWatchRemovals(t *testing.T) {
	metaStore := New(t.TempDir())
	sess := store.Session{ID: "1a5ua8uj", Adapter: "claude", Alive: false, ConversationRef: "/c/conv.jsonl"}
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

	if ids := sessions.RemoveDeadByConversationRef("claude", "/c/conv.jsonl"); len(ids) != 1 {
		t.Fatalf("expected 1 retired session, got %v", ids)
	}

	deadline := time.After(2 * time.Second)
	for exists(t, metaStore.SessionDir("1a5ua8uj")) {
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
	liveDir := s.SessionDir("18f5bhaf")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sb := filepath.Join(liveDir, scrollback.ActiveName)
	if err := os.WriteFile(sb, []byte("live output"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Genuinely empty orphan dir: still swept.
	emptyDir := s.SessionDir("157qugt2")
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
