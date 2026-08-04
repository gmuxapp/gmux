package centralstore

import (
	"context"
	"errors"
	"testing"
)

// The drive-mode axis (ADR 0033): persisted at registration, defaulted to
// terminal for older runners, refused when unknown, and immutable across
// re-registration (mode changes only through explicit conversion).
func TestRegisterRunnerDriveMode(t *testing.T) {
	ctx := context.Background()

	t.Run("acp mode persists", func(t *testing.T) {
		s := openTestStore(t)
		row, _, err := s.RegisterRunner(ctx, RunnerRegistration{
			ID: "a", Adapter: "claude", DriveMode: DriveModeACP, Alive: true, CreatedAt: 1, ObservedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if row.DriveMode != DriveModeACP {
			t.Fatalf("DriveMode = %q, want acp", row.DriveMode)
		}
		read, ok, err := s.Session(ctx, "a")
		if err != nil || !ok || read.DriveMode != DriveModeACP {
			t.Fatalf("read = %+v, %v, %v; want persisted acp mode", read, ok, err)
		}
	})

	t.Run("empty mode normalizes to terminal", func(t *testing.T) {
		s := openTestStore(t)
		row, _, err := s.RegisterRunner(ctx, RunnerRegistration{
			ID: "b", Adapter: "pi", Alive: true, CreatedAt: 1, ObservedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if row.DriveMode != DriveModeTerminal {
			t.Fatalf("DriveMode = %q, want terminal default", row.DriveMode)
		}
	})

	t.Run("unknown mode is refused, never defaulted", func(t *testing.T) {
		s := openTestStore(t)
		_, _, err := s.RegisterRunner(ctx, RunnerRegistration{
			ID: "c", Adapter: "pi", DriveMode: "telepathy", Alive: true, CreatedAt: 1, ObservedAt: 1,
		})
		if err == nil {
			t.Fatal("unknown drive mode accepted")
		}
	})

	t.Run("re-registration under a different mode is refused", func(t *testing.T) {
		s := openTestStore(t)
		if _, _, err := s.RegisterRunner(ctx, RunnerRegistration{
			ID: "d", Adapter: "claude", DriveMode: DriveModeTerminal, Alive: true, CreatedAt: 1, ObservedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.RegisterRunner(ctx, RunnerRegistration{
			ID: "d", Adapter: "claude", DriveMode: DriveModeACP, Alive: true, CreatedAt: 2, ObservedAt: 2,
		})
		if !errors.Is(err, ErrDriveModeMismatch) {
			t.Fatalf("err = %v, want ErrDriveModeMismatch", err)
		}
	})

	t.Run("insert validates and defaults like registration", func(t *testing.T) {
		s := openTestStore(t)
		row, _, err := s.InsertSession(ctx, NewSession{ID: "e", Adapter: "codex", DriveMode: DriveModeACP, CWD: "/", CreatedAt: 1})
		if err != nil || row.DriveMode != DriveModeACP {
			t.Fatalf("row=%+v err=%v, want acp", row, err)
		}
		if _, _, err := s.InsertSession(ctx, NewSession{ID: "f", Adapter: "codex", DriveMode: "nope", CWD: "/", CreatedAt: 1}); err == nil {
			t.Fatal("unknown drive mode accepted at insert")
		}
		plain, _, err := s.InsertSession(ctx, NewSession{ID: "g", Adapter: "shell", CWD: "/", CreatedAt: 1})
		if err != nil || plain.DriveMode != DriveModeTerminal {
			t.Fatalf("row=%+v err=%v, want terminal default", plain, err)
		}
	})
}
