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
// Sweep also enforces a retention policy on dead-but-not-dismissed
// sessions so the state dir doesn't grow without bound: corpses older
// than 30 days are aged out, and only the newest 200 dead sessions are
// kept (LRU by exit time). Both limits are configurable via the
// GMUX_SESSION_RETENTION_DAYS and GMUX_SESSION_RETENTION_MAX environment
// variables (0 disables a limit). See RetentionPolicy and Sweep.
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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

const (
	metaFile = "meta.json"
	dirMode  = 0o700
	fileMode = 0o600
)

// Retention defaults bound the disk footprint of dead-but-not-dismissed
// sessions. Each dead session keeps a meta.json plus up to ~2 MiB of
// rotated scrollback under $XDG_STATE_HOME/gmux/sessions/<id>/; without a
// cap these accumulate forever for users who never dismiss. The Sweep at
// startup applies the policy: corpses older than MaxAge are aged out, and
// once that's done only the newest MaxCount dead sessions are kept (LRU by
// effective timestamp). Live sessions are never on the chopping block here
// because Sweep only loads Alive=false records, and any still-running
// runner re-registers as alive immediately afterwards.
//
// Resumable sessions get no special exemption, and that is safe:
// store.Upsert derives Resumable = !Alive && len(Command) > 0, so it is
// a UI affordance offering to re-run a dead session's command, not a
// handle on a live OS process. A resumable corpse has no backing
// process to orphan; pruning a 30-day-old one only retires a stale
// "resume" button, and the command itself was never the artifact we
// promised to keep forever. Recently-dead resumables stay because they
// are the newest by timestamp and well within MaxAge.
const (
	DefaultMaxAge   = 30 * 24 * time.Hour
	DefaultMaxCount = 200
)

// Environment variables that override the retention defaults at startup.
// Both accept a non-negative integer; 0 disables that limit (unbounded).
const (
	envRetentionDays  = "GMUX_SESSION_RETENTION_DAYS"
	envRetentionCount = "GMUX_SESSION_RETENTION_MAX"
)

// RetentionPolicy bounds how many dead-session directories survive a
// Sweep. A zero value for either field disables that limit.
type RetentionPolicy struct {
	// MaxAge ages out dead sessions whose effective timestamp is older
	// than now-MaxAge. Zero means no age limit.
	MaxAge time.Duration
	// MaxCount keeps only the newest MaxCount dead sessions (by
	// effective timestamp) after age-out. Zero means no count limit.
	MaxCount int
}

// DefaultRetention returns the built-in policy, with the
// GMUX_SESSION_RETENTION_DAYS / GMUX_SESSION_RETENTION_MAX environment
// overrides applied when set to a valid non-negative integer.
func DefaultRetention() RetentionPolicy {
	p := RetentionPolicy{MaxAge: DefaultMaxAge, MaxCount: DefaultMaxCount}
	if v := os.Getenv(envRetentionDays); v != "" {
		if days, err := strconv.Atoi(v); err == nil && days >= 0 {
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
	// now is the clock used by Sweep's retention pass. Overridable in
	// tests via WithClock; defaults to time.Now.
	now func() time.Time
}

// Option configures a Store at construction.
type Option func(*Store)

// WithRetention sets the dead-session retention policy applied during
// Sweep. Without it, Sweep performs no age/count pruning.
func WithRetention(p RetentionPolicy) Option {
	return func(s *Store) { s.retention = p }
}

// WithClock overrides the clock used by Sweep's retention pass. Tests
// inject a fixed clock to make age-out deterministic.
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

// Dir is the base directory under which per-session subdirectories
// live. Exposed for tests and for diagnostic logging.
func (s *Store) Dir() string { return s.dir }

// SessionDir is the per-session subdirectory: <Dir()>/<id>/.
func (s *Store) SessionDir(id string) string {
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
//   - directories with no meta.json (orphan scrollback or empty
//     dirs) are logged and removed
//   - directories whose meta.json fails to parse are logged and
//     left in place (operator may want to inspect)
//   - non-directory entries under the base dir are ignored
//
// After loading, the retention policy (if any) prunes dead-session
// directories: corpses older than MaxAge are aged out, then only the
// newest MaxCount survive (LRU by effective timestamp). Pruned sessions
// are not returned. See RetentionPolicy.
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

// prune applies the retention policy to the freshly-loaded dead
// sessions, removing the on-disk directories of the losers and
// returning the survivors. Age-out runs first, then the count cap on
// what remains. A no-op when the policy disables both limits.
func (s *Store) prune(loaded []store.Session) []store.Session {
	if s.retention.MaxAge <= 0 && s.retention.MaxCount <= 0 {
		return loaded
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}

	survivors := loaded[:0:0]
	if s.retention.MaxAge > 0 {
		cutoff := now().Add(-s.retention.MaxAge)
		for _, sess := range loaded {
			t, ok := effectiveTime(sess)
			if ok && t.Before(cutoff) {
				s.evict(sess.ID, "age")
				continue
			}
			survivors = append(survivors, sess)
		}
	} else {
		survivors = append(survivors, loaded...)
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
	return survivors
}

// evict removes a pruned session's directory and logs the reason.
func (s *Store) evict(id, reason string) {
	log.Printf("sessionmeta: pruning dead session %s (retention: %s)", id, reason)
	if err := s.Remove(id); err != nil {
		log.Printf("sessionmeta: prune remove %s: %v", id, err)
	}
}
