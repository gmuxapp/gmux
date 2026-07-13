package main

import (
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// TestClearDeadSessionUnread pins the view-acknowledges rule for dead
// sessions (ADR 0023): viewing clears unread and the error dot but
// must not touch the persisted turn state (Status.Working) that
// post-mortem waits resolve by, and must not interfere with live
// sessions, whose unread is owned by the runner.
func TestClearDeadSessionUnread(t *testing.T) {
	exit3 := 3
	cases := []struct {
		name string
		in   store.Session
		want func(t *testing.T, s store.Session)
	}{
		{
			name: "dead unread one-shot: unread and error clear, Working survives",
			in: store.Session{
				ID: "s", Adapter: "shell", Alive: false, Unread: true,
				ExitCode: &exit3,
				Status:   &store.Status{Working: false, Error: true},
			},
			want: func(t *testing.T, s store.Session) {
				if s.Unread {
					t.Error("unread not cleared on view")
				}
				if s.Status == nil || s.Status.Error {
					t.Errorf("Status = %+v, want error cleared", s.Status)
				}
				if s.Status != nil && s.Status.Working {
					t.Error("Working mutated; turn state at death must survive the view")
				}
			},
		},
		{
			name: "live unread session: untouched (runner owns it)",
			in: store.Session{
				ID: "s", Adapter: "shell", Alive: true, Unread: true,
				Status: &store.Status{Working: false},
			},
			want: func(t *testing.T, s store.Session) {
				if !s.Unread {
					t.Error("live session's unread cleared daemon-side; the runner owns that transition")
				}
			},
		},
		{
			name: "dead already-read session: no-op",
			in: store.Session{
				ID: "s", Adapter: "shell", Alive: false, Unread: false,
				Status: &store.Status{Working: false},
			},
			want: func(t *testing.T, s store.Session) {
				if s.Unread {
					t.Error("unread flipped on")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := store.New()
			sessions.Upsert(tc.in)
			clearDeadSessionUnread(sessions, "s")
			got, ok := sessions.Get("s")
			if !ok {
				t.Fatal("session vanished")
			}
			tc.want(t, got)
		})
	}

	t.Run("unknown session: no panic", func(t *testing.T) {
		clearDeadSessionUnread(store.New(), "nope")
	})
}
