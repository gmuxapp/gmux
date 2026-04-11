package sessionfiles

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

func TestPurgeStaleSessions(t *testing.T) {
	s := store.New()

	s.Upsert(store.Session{
		ID:       "stale",
		Alive:    false,
		ExitedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})
	s.Upsert(store.Session{
		ID:       "fresh",
		Alive:    false,
		ExitedAt: time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339),
	})
	s.Upsert(store.Session{
		ID:       "resumable",
		Alive:    false,
		Slug:     "some-key",
		ExitedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	})

	sc := New(s)
	sc.PurgeStaleSessions(1 * time.Hour)

	ids := map[string]bool{}
	for _, sess := range s.List() {
		ids[sess.ID] = true
	}

	if ids["stale"] {
		t.Error("stale session should have been purged")
	}
	if !ids["fresh"] {
		t.Error("fresh session should still be present")
	}
	if !ids["resumable"] {
		t.Error("resumable session should still be present")
	}
}
