// Package sessionmeta persists per-session metadata to disk so dead
// sessions survive a gmuxd restart and so future scrollback artifacts
// (S3) live alongside their owning session's record.
//
// gmuxd writes a meta.json file under
// $XDG_STATE_HOME/gmux/sessions/<id>/ every time a locally-owned
// session lands as Alive=false in the store. That covers the
// obvious Alive=true→false transitions (SSE exit, socket-gone,
// kill-unreachable) plus the less-obvious case of fast-exiting
// commands whose runner finishes before gmuxd's queryMeta arrives:
// the runner's /meta then reports alive=false directly, so the
// session lands as dead via Register's fresh-upsert path without
// any transition.
//
// On startup, Sweep loads each meta.json back into the store as
// Alive=false so previously-seen sessions remain visible in the
// sidebar across restarts. On Dismiss / Resume merge / slug
// takeover, the per-session directory is removed.
//
// Retention model (two tiers — see RetentionPolicy and ADR 0016):
//
//  1. Scrollback is a cache. The big artifact in each session dir is
//     the runner-written scrollback (up to ~2 MiB). PruneScrollback
//     caps the *aggregate* scrollback bytes across all dead sessions,
//     evicting the oldest scrollback files first while KEEPING
//     meta.json — so the session stays in the sidebar and resumable,
//     just without terminal history. It runs at startup (once the
//     store knows which runners are alive) and, throttled, when a
//     session is dismissed; never on a timer.
//
//  2. meta.json mirrors conversation existence. A dead session whose
//     backing conversation file disappears (the conversations index
//     reports the removal) has its whole dir retired. Dead sessions
//     that never had a conversation file (shells) can't key off that
//     signal, so they fall back to an age/count cap on the whole dir.
//
// Peer-owned sessions are excluded: the hub re-receives them from
// the spoke on reconnect, so persisting on the hub would create
// duplicate ghosts. Write is a no-op for sess.Peer != "".
//
// The package writes with mode 0o600 inside a 0o700 parent so
// scrollback (added in S3) and any future per-session secrets stay
// readable only by the gmux user.
package sessionmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

const (
	metaFile = "meta.json"
	dirMode  = 0o700
	fileMode = 0o600

	// pruneStampFile marks when PruneScrollback last ran. Its mtime
	// drives the dismiss-time throttle (MaybePruneScrollback). Lives in
	// the base dir, not a session subdir, so it's never confused for a
	// session by Sweep (which only descends into directories).
	pruneStampFile = ".scrollback-prune-stamp"
)

// Retention defaults bound the disk footprint of dead-but-not-dismissed
// sessions. See ADR 0016 for the model and RetentionPolicy for the
// per-field semantics.
const (
	// DefaultMaxAge / DefaultMaxCount age/count out *conversation-less*
	// dead sessions (shells): they keep meta + scrollback under the
	// sessions dir but have no conversation file whose removal could
	// retire them, so they need their own lifecycle.
	DefaultMaxAge   = 30 * 24 * time.Hour
	DefaultMaxCount = 200

	// DefaultScrollbackCacheBytes caps the aggregate size of scrollback
	// files across all dead sessions (~128 sessions' worth at the 2 MiB
	// per-session ceiling). Oldest scrollback is evicted first; meta is
	// untouched.
	DefaultScrollbackCacheBytes int64 = 256 << 20
)

// Environment variables that override the retention defaults at startup.
// Each accepts a non-negative integer; 0 disables that limit (unbounded).
const (
	envRetentionDays     = "GMUX_SESSION_RETENTION_DAYS"
	envRetentionCount    = "GMUX_SESSION_RETENTION_MAX"
	envScrollbackCacheMB = "GMUX_SCROLLBACK_CACHE_MB"
)

// RetentionPolicy bounds the disk footprint of dead sessions. A zero
// value for any field disables that limit.
type RetentionPolicy struct {
	// MaxAge ages out conversation-less dead sessions (SessionFile == "")
	// whose effective timestamp is older than now-MaxAge. Sessions with a
	// conversation file are exempt: their lifecycle is the conversation's
	// (index-driven removal). Zero means no age limit.
	MaxAge time.Duration
	// MaxCount keeps only the newest MaxCount conversation-less dead
	// sessions (by effective timestamp) after age-out. Zero means no
	// count limit.
	MaxCount int
	// ScrollbackCacheBytes caps the total bytes of scrollback files
	// across all dead sessions. When exceeded, PruneScrollback deletes
	// scrollback (keeping meta.json) oldest-first until under the cap.
	// Zero means no cap.
	ScrollbackCacheBytes int64
}

// DefaultRetention returns the built-in policy with the
// GMUX_SESSION_RETENTION_DAYS / GMUX_SESSION_RETENTION_MAX /
// GMUX_SCROLLBACK_CACHE_MB environment overrides applied when set to a
// valid non-negative integer.
func DefaultRetention() RetentionPolicy {
	p := RetentionPolicy{
		MaxAge:               DefaultMaxAge,
		MaxCount:             DefaultMaxCount,
		ScrollbackCacheBytes: DefaultScrollbackCacheBytes,
	}
	if v := os.Getenv(envRetentionDays); v != "" {
		// Cap at the largest day count that still fits in a time.Duration
		// (int64 ns) once multiplied by 24h, so a huge value can't
		// overflow to a negative duration that prune would silently treat
		// as "no age limit".
		const maxSafeDays = math.MaxInt64 / int64(24*time.Hour) // ~106 751
		if days, err := strconv.Atoi(v); err == nil && days >= 0 && int64(days) <= maxSafeDays {
			p.MaxAge = time.Duration(days) * 24 * time.Hour
		} else {
			log.Printf("sessionmeta: ignoring invalid %s=%q", envRetentionDays, v)
		}
	}
	if v := os.Getenv(envRetentionCount); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.MaxCount = n
		} else {
			log.Printf("sessionmeta: ignoring invalid %s=%q", envRetentionCount, v)
		}
	}
	if v := os.Getenv(envScrollbackCacheMB); v != "" {
		// Guard the MiB→bytes shift against int64 overflow the same way.
		const maxSafeMB = math.MaxInt64 >> 20 // MiB counts whose byte value fits in int64
		if mb, err := strconv.Atoi(v); err == nil && mb >= 0 && int64(mb) <= maxSafeMB {
			p.ScrollbackCacheBytes = int64(mb) << 20
		} else {
			log.Printf("sessionmeta: ignoring invalid %s=%q", envScrollbackCacheMB, v)
		}
	}
	return p
}

// Per-session directories may also contain scrollback files written
// by the runner (see packages/scrollback). sessionmeta does not
// inspect or own those files — it only manages meta.json — but it
// removes them as a side effect of Remove (entire dir tree).

// DefaultDir is the production state-dir for session metadata.
// $XDG_STATE_HOME/gmux/sessions, derived from paths.SessionsDir.
func DefaultDir() string {
	return paths.SessionsDir()
}

// Store binds a base directory to the file-IO operations. One Store
// is constructed at gmuxd startup with DefaultDir(); tests construct
// their own with a temp dir.
type Store struct {
	dir       string
	retention RetentionPolicy
	// now is the clock used by the retention passes. Overridable in
	// tests via WithClock; defaults to time.Now.
	now func() time.Time
	// pruneMu serializes PruneScrollback so the startup pass and a
	// dismiss-triggered pass don't double-walk the dir (and double-log
	// "no such file" on the same victim). A run that can't take it just
	// skips: the holder's walk already covers the same work.
	pruneMu sync.Mutex
}

// Option configures a Store at construction.
type Option func(*Store)

// WithRetention sets the dead-session retention policy. Without it,
// Sweep performs no age/count pruning and PruneScrollback is a no-op.
func WithRetention(p RetentionPolicy) Option {
	return func(s *Store) { s.retention = p }
}

// WithClock overrides the clock used by the retention passes. Tests
// inject a fixed clock to make age-out and throttling deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New returns a Store rooted at dir. The directory is created lazily
// on first Write; no IO happens at construction.
func New(dir string, opts ...Option) *Store {
	s := &Store{dir: dir, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Dir is the base directory under which per-session subdirectories
// live. Exposed for tests and for diagnostic logging.
func (s *Store) Dir() string { return s.dir }

// SessionDir is the per-session subdirectory: <Dir()>/<id>/.
//
// Defense in depth: id becomes a path segment, and SessionDir feeds
// every disk operation in this package — including Remove's
// RemoveAll. A crafted id with separators or ".." must never let
// those escape s.dir, so an id that isn't a well-formed session ID is
// routed to a single reserved ".invalid" subdir that stays inside
// s.dir and can never collide with a real session. Well-formed IDs
// (which contain no separators) join unchanged, and Write rejects
// invalid IDs with an explicit error before reaching disk, so this
// fallback is purely a containment backstop.
func (s *Store) SessionDir(id string) string {
	if !paths.IsValidSessionID(id) {
		return filepath.Join(s.dir, ".invalid")
	}
	return filepath.Join(s.dir, id)
}

// persistedSession aliases store.Session to bypass the API
// MarshalJSON method, which strips internal fields like ShellTitle
// and AdapterTitle. The default reflection-based marshaller
// respects the json tags on the original struct — including the
// ones for those internal fields — so no manual mirroring is
// needed and adding a new field to store.Session automatically
// flows into persistence.
type persistedSession store.Session

// Write atomically persists s to <dir>/<s.ID>/meta.json. Skips
// peer-owned sessions: the hub doesn't authoritatively own those
// records and re-receives them from the spoke on reconnect.
//
// Atomic via temp file + rename. The temp file is created in the
// same directory so rename(2) is on the same filesystem.
func (s *Store) Write(sess store.Session) error {
	if sess.Peer != "" {
		return nil // not ours to persist
	}
	if sess.ID == "" {
		return errors.New("sessionmeta: cannot persist session with empty id")
	}
	// Defense in depth: the ID becomes a path segment under the
	// sessions dir, so reject anything that isn't a well-formed
	// session ID before it reaches MkdirAll/Rename. The registration
	// boundary validates too, but this guarantees the write path can
	// never escape the sessions dir regardless of caller.
	if !paths.IsValidSessionID(sess.ID) {
		return fmt.Errorf("sessionmeta: invalid session id %q", sess.ID)
	}

	sessDir := s.SessionDir(sess.ID)
	if err := os.MkdirAll(sessDir, dirMode); err != nil {
		return fmt.Errorf("sessionmeta: mkdir %s: %w", sessDir, err)
	}

	data, err := json.MarshalIndent(persistedSession(sess), "", "  ")
	if err != nil {
		return fmt.Errorf("sessionmeta: marshal %s: %w", sess.ID, err)
	}

	final := filepath.Join(sessDir, metaFile)
	tmp, err := os.CreateTemp(sessDir, metaFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("sessionmeta: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("sessionmeta: write tmp: %w", err)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		cleanupTmp()
		return fmt.Errorf("sessionmeta: chmod tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("sessionmeta: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		cleanupTmp()
		return fmt.Errorf("sessionmeta: rename %s → %s: %w", tmpPath, final, err)
	}
	return nil
}

// Read returns the persisted session for id, or (zero, error) if
// the directory or meta.json is missing or unparseable.
func (s *Store) Read(id string) (store.Session, error) {
	path := filepath.Join(s.SessionDir(id), metaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return store.Session{}, err
	}
	var ps persistedSession
	if err := json.Unmarshal(data, &ps); err != nil {
		return store.Session{}, fmt.Errorf("sessionmeta: parse %s: %w", path, err)
	}
	// Legacy-read shim: pre-v2 meta.json used "kind" and "session_file".
	// meta.json has no schema version, so fall back to the old keys when
	// the new ones are absent. Write emits only the new keys.
	// TODO(v2.1): drop this shim.
	if ps.Kind == "" || ps.SessionFile == "" {
		var legacy struct {
			Kind        string `json:"kind"`
			SessionFile string `json:"session_file"`
		}
		if err := json.Unmarshal(data, &legacy); err == nil {
			if ps.Kind == "" {
				ps.Kind = legacy.Kind
			}
			if ps.SessionFile == "" {
				ps.SessionFile = legacy.SessionFile
			}
		}
	}
	return store.Session(ps), nil
}

// Remove deletes the entire per-session directory. Idempotent: a
// missing directory is not an error. Used for dismiss and for the
// resume-merge cleanup, where the dead session's record is replaced
// by a fresh live registration.
func (s *Store) Remove(id string) error {
	if id == "" {
		return nil
	}
	sessDir := s.SessionDir(id)
	if err := os.RemoveAll(sessDir); err != nil {
		return fmt.Errorf("sessionmeta: remove %s: %w", sessDir, err)
	}
	return nil
}

// WatchRemovals consumes events until the channel closes, removing
// the on-disk record for every session-remove event. Wired at
// gmuxd startup against store.Subscribe so the persister cleans up
// after every store removal, not just the dismiss / resume-merge
// paths that have explicit Remove calls.
//
// Catches the slug-takeover case: when a fresh live session
// shadows a dead one with the same (kind, peer, slug), the store
// silently evicts the dead record and broadcasts session-remove.
// Without this loop those orphan meta.json files accumulate — the
// next Sweep would re-load them and the next slug collision would
// re-orphan, indefinitely.
//
// Errors from Remove are logged; failure to clean up one entry
// doesn't stop the loop. Returns when the events channel closes
// (i.e., when the store subscription is cancelled at shutdown).
func (s *Store) WatchRemovals(events <-chan store.Event) {
	for ev := range events {
		if ev.Type != "session-remove" {
			continue
		}
		if err := s.Remove(ev.ID); err != nil {
			log.Printf("sessionmeta: cleanup remove %s: %v", ev.ID, err)
		}
	}
}

// Sweep loads every persisted session from disk. Each is returned
// with Alive=false; callers (gmuxd at startup) Upsert them so the
// sidebar shows previously-seen sessions before any live runners
// register.
//
// Cleanup performed during the sweep:
//   - directories with no meta.json AND no scrollback (empty or junk
//     dirs) are logged and removed
//   - directories with scrollback but no meta.json are left alone:
//     they belong to live, never-died runners (meta.json is written
//     only on dead landings) whose open scrollback file must not be
//     unlinked; crash leftovers among them are reclaimed by
//     PruneScrollback's cap and swept once empty
//   - directories whose meta.json fails to parse are logged and
//     left in place (operator may want to inspect)
//   - non-directory entries under the base dir are ignored
//
// After loading, the whole-dir retention cap is applied to
// conversation-less dead sessions (SessionFile == ""): corpses older
// than MaxAge are aged out, then only the newest MaxCount survive.
// Sessions with a conversation file are exempt — they are retired only
// when their conversation file disappears (see the conversations index
// wiring in cmd/gmuxd). Pruned sessions are not returned. Scrollback
// space is reclaimed separately by PruneScrollback.
//
// Returns nil error when the base dir doesn't exist (clean install).
func (s *Store) Sweep() ([]store.Session, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionmeta: read %s: %w", s.dir, err)
	}

	var loaded []store.Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		sessDir := s.SessionDir(id)
		metaPath := filepath.Join(sessDir, metaFile)

		if _, err := os.Stat(metaPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// No meta.json. If the dir holds scrollback, it belongs to
				// a live, never-died runner — meta.json is written only on
				// dead landings, and the runner keeps its scrollback file
				// open in this dir from launch — so removing it here would
				// unlink the open file and sticky-break the runner's next
				// rotation, destroying the session's post-mortem replay on
				// every daemon restart. Leave it alone: if it is instead a
				// crash leftover, PruneScrollback's cap reclaims the bytes
				// and a later Sweep removes the then-empty dir.
				if hasScrollback(sessDir) {
					continue
				}
				log.Printf("sessionmeta: orphan dir %s (no %s); removing", sessDir, metaFile)
				_ = os.RemoveAll(sessDir)
				continue
			}
			log.Printf("sessionmeta: stat %s: %v; skipping", metaPath, err)
			continue
		}

		sess, err := s.Read(id)
		if err != nil {
			log.Printf("sessionmeta: read %s: %v; skipping", metaPath, err)
			continue
		}
		// Sweep loads dead sessions only; if a live runner is still
		// listening, discovery.Register will upsert it with Alive=true
		// shortly after.
		sess.Alive = false
		loaded = append(loaded, sess)
	}
	return s.prune(loaded), nil
}

// effectiveTime returns the timestamp used to rank a dead session for
// retention: ExitedAt (when it died) is preferred, falling back to
// LastActivityAt then CreatedAt. The bool is false when none parse, in
// which case the session is treated conservatively (never aged out,
// kept as if newest) so an undatable record is never evicted.
func effectiveTime(sess store.Session) (time.Time, bool) {
	for _, ts := range []string{sess.ExitedAt, sess.LastActivityAt, sess.CreatedAt} {
		if ts == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// prune applies the whole-dir age/count cap to conversation-less dead
// sessions and returns the survivors plus every session that has a
// conversation file (exempt). Removes the on-disk dir of each loser.
// A no-op when the policy disables both limits.
func (s *Store) prune(loaded []store.Session) []store.Session {
	if s.retention.MaxAge <= 0 && s.retention.MaxCount <= 0 {
		return loaded
	}

	// Partition: sessions with a conversation file are never whole-dir
	// pruned here; their lifecycle is index-driven.
	var exempt, corpses []store.Session
	for _, sess := range loaded {
		if sess.SessionFile != "" {
			exempt = append(exempt, sess)
		} else {
			corpses = append(corpses, sess)
		}
	}

	survivors := corpses[:0:0]
	if s.retention.MaxAge > 0 {
		cutoff := s.clock().Add(-s.retention.MaxAge)
		for _, sess := range corpses {
			t, ok := effectiveTime(sess)
			if ok && t.Before(cutoff) {
				s.evict(sess.ID, "age")
				continue
			}
			survivors = append(survivors, sess)
		}
	} else {
		survivors = append(survivors, corpses...)
	}

	if s.retention.MaxCount > 0 && len(survivors) > s.retention.MaxCount {
		// Sort newest-first; undatable records sort as newest so they
		// survive the count cap. Stable so equal timestamps keep their
		// readdir order.
		sort.SliceStable(survivors, func(i, j int) bool {
			ti, oki := effectiveTime(survivors[i])
			tj, okj := effectiveTime(survivors[j])
			if oki != okj {
				return !oki // undatable (newest) before datable
			}
			return ti.After(tj)
		})
		for _, sess := range survivors[s.retention.MaxCount:] {
			s.evict(sess.ID, "count")
		}
		survivors = survivors[:s.retention.MaxCount]
	}
	return append(survivors, exempt...)
}

// evict removes a pruned session's directory and logs the reason.
func (s *Store) evict(id, reason string) {
	log.Printf("sessionmeta: pruning dead session %s (retention: %s)", id, reason)
	if err := s.Remove(id); err != nil {
		log.Printf("sessionmeta: prune remove %s: %v", id, err)
	}
}

// scrollbackUsage is one session dir's scrollback footprint, used to
// rank eviction candidates oldest-first.
type scrollbackUsage struct {
	id    string
	bytes int64
	mtime time.Time // newest mtime across the dir's scrollback files
}

// scrollbackNames are the runner-written files PruneScrollback reclaims.
var scrollbackNames = []string{scrollback.ActiveName, scrollback.PreviousName}

// hasScrollback reports whether the session dir contains any
// runner-written scrollback file. Used by Sweep's orphan check to spare
// live runners' dirs (which have scrollback but no meta.json yet).
func hasScrollback(sessDir string) bool {
	for _, name := range scrollbackNames {
		if _, err := os.Stat(filepath.Join(sessDir, name)); err == nil {
			return true
		}
	}
	return false
}

// PruneScrollback enforces ScrollbackCacheBytes: it sums the scrollback
// bytes across every session dir and, if the total exceeds the cap,
// deletes scrollback files (keeping meta.json) oldest-first — by newest
// scrollback mtime — until the total is back under the cap.
//
// alive lists session IDs whose runner may still be writing scrollback;
// those dirs are skipped entirely (never delete a live runner's open
// file). The store is the authority on liveness, so callers build alive
// from it; at startup this must run only after discovery's first scan
// has registered live runners.
//
// Best-effort: stat/remove errors are logged and skipped. A zero cap or
// missing base dir is a no-op. Updates the prune stamp on completion.
func (s *Store) PruneScrollback(alive map[string]bool) {
	if s.retention.ScrollbackCacheBytes <= 0 {
		return
	}
	if !s.pruneMu.TryLock() {
		return // another prune is already in flight
	}
	defer s.pruneMu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("sessionmeta: scrollback prune read %s: %v", s.dir, err)
		}
		return
	}

	var usages []scrollbackUsage
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if alive[id] {
			continue // runner may be writing; off-limits
		}
		sessDir := s.SessionDir(id)
		var bytes int64
		var mtime time.Time
		for _, name := range scrollbackNames {
			fi, err := os.Stat(filepath.Join(sessDir, name))
			if err != nil {
				continue
			}
			bytes += fi.Size()
			if fi.ModTime().After(mtime) {
				mtime = fi.ModTime()
			}
		}
		if bytes == 0 {
			continue
		}
		usages = append(usages, scrollbackUsage{id: id, bytes: bytes, mtime: mtime})
		total += bytes
	}

	s.stampPrune()

	if total <= s.retention.ScrollbackCacheBytes {
		return
	}

	// Oldest scrollback first.
	sort.SliceStable(usages, func(i, j int) bool {
		return usages[i].mtime.Before(usages[j].mtime)
	})
	for _, u := range usages {
		if total <= s.retention.ScrollbackCacheBytes {
			break
		}
		// Decrement by what was actually freed, not the scan-time size:
		// if a remove failed (or the victim was skipped) those bytes are
		// still on disk, so we must keep evicting rather than stop early
		// on a phantom reclaim.
		total -= s.evictScrollback(u)
	}
}

// evictScrollback removes one session's scrollback files and returns
// the bytes actually freed.
//
// Write-freshness guard: the alive set is a snapshot, so a session
// resumed *during* the prune isn't in it — yet its fresh runner reopens
// (truncating) the same scrollback path (see cli/gmux run). Deleting
// that open file would leave the runner appending to an unlinked inode
// and break its next rotation (sticky failure). Any writer — including
// a runner the store doesn't know about — bumps the file mtime, so a
// victim whose scrollback mtime advanced past its scan-time value is
// skipped entirely. The remaining stat→remove window is microseconds.
func (s *Store) evictScrollback(u scrollbackUsage) (freed int64) {
	sessDir := s.SessionDir(u.id)
	for _, name := range scrollbackNames {
		if fi, err := os.Stat(filepath.Join(sessDir, name)); err == nil && fi.ModTime().After(u.mtime) {
			log.Printf("sessionmeta: scrollback prune skipping %s (written since scan)", u.id)
			return 0
		}
	}
	for _, name := range scrollbackNames {
		p := filepath.Join(sessDir, name)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if err := os.Remove(p); err != nil {
			log.Printf("sessionmeta: scrollback prune remove %s: %v", p, err)
			continue
		}
		freed += fi.Size()
	}
	if freed > 0 {
		log.Printf("sessionmeta: reclaimed %d bytes of scrollback from %s (cache cap)", freed, u.id)
	}
	return freed
}

// MaybePruneScrollback runs PruneScrollback only when at least
// minInterval has elapsed since the last run (tracked by the prune
// stamp's mtime). Returns whether it ran. Used on dismiss so a
// long-running daemon reclaims scrollback at a natural, user-initiated
// moment without any background timer. A missing stamp counts as "due".
func (s *Store) MaybePruneScrollback(alive map[string]bool, minInterval time.Duration) bool {
	if s.retention.ScrollbackCacheBytes <= 0 {
		return false
	}
	if fi, err := os.Stat(filepath.Join(s.dir, pruneStampFile)); err == nil {
		if s.clock().Sub(fi.ModTime()) < minInterval {
			return false
		}
	}
	s.PruneScrollback(alive)
	return true
}

// stampPrune records the current time as the last-prune marker. Best
// effort: a failure only means the throttle may fire again sooner.
func (s *Store) stampPrune() {
	path := filepath.Join(s.dir, pruneStampFile)
	now := s.clock()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	// Stamp doesn't exist yet (or base dir missing): create it.
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return
	}
	_ = f.Close()
	_ = os.Chtimes(path, now, now)
}
