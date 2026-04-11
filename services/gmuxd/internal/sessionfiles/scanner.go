// Package sessionfiles provides periodic maintenance for the session
// store: purging ephemeral dead sessions that were never attributed
// to a conversation file.
//
// File-backed conversation discovery is handled by the conversations
// package. This package only handles cleanup.
package sessionfiles

import (
	"log"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// Scanner provides periodic store maintenance.
type Scanner struct {
	store *store.Store

	// OnFirstScan is called once after the initial purge completes.
	// At that point the store has the full set of known sessions,
	// making it safe to clean up stale references elsewhere (e.g.
	// project session arrays).
	OnFirstScan func()
}

func New(s *store.Store) *Scanner {
	return &Scanner{store: s}
}

// Run performs an initial purge, fires OnFirstScan, then purges
// periodically until stop is closed.
func (sc *Scanner) Run(interval time.Duration, stop <-chan struct{}) {
	sc.PurgeStaleSessions(10 * time.Minute)
	if sc.OnFirstScan != nil {
		sc.OnFirstScan()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sc.PurgeStaleSessions(10 * time.Minute)
		}
	}
}

// PurgeStaleSessions removes dead sessions that have no slug and
// are older than maxAge. These are short-lived sessions that exited
// without ever being attributed to a conversation file.
func (sc *Scanner) PurgeStaleSessions(maxAge time.Duration) {
	now := time.Now().UTC()
	for _, s := range sc.store.List() {
		if s.Alive || s.Resumable || s.Slug != "" {
			continue
		}
		exited, err := time.Parse(time.RFC3339, s.ExitedAt)
		if err != nil {
			continue
		}
		if now.Sub(exited) > maxAge {
			log.Printf("sessionfiles: purging stale session %s (exited %s ago)", s.ID, now.Sub(exited).Round(time.Second))
			sc.store.Remove(s.ID)
		}
	}
}
