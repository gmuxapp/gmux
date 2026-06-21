package filewatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "1.jsonl"))
	write(t, filepath.Join(root, "b", "c", "2.jsonl"))
	write(t, filepath.Join(root, "a", "skip.txt"))

	var got []string
	Snapshot(root, ".jsonl", func(p string) { got = append(got, filepath.Base(p)) })

	if len(got) != 2 {
		t.Fatalf("got %v, want 2 .jsonl files", got)
	}
}

// waitFor polls events until pred is satisfied or the deadline passes.
func waitFor(t *testing.T, events <-chan Event, pred func(Event) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if pred(e) {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for matching event")
		}
	}
}

func TestWatchCreateAndRemove(t *testing.T) {
	root := t.TempDir()
	events := make(chan Event, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, root, ".jsonl", func(e Event) { events <- e })

	time.Sleep(100 * time.Millisecond) // let the watch establish

	path := filepath.Join(root, "s.jsonl")
	write(t, path)
	waitFor(t, events, func(e Event) bool { return e.Path == path && !e.Removed })

	os.Remove(path)
	waitFor(t, events, func(e Event) bool { return e.Path == path && e.Removed })
}

// A file landing in a subdirectory created after the watch starts is caught up.
func TestWatchCatchesUpNewSubdir(t *testing.T) {
	root := t.TempDir()
	events := make(chan Event, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, root, ".jsonl", func(e Event) { events <- e })

	time.Sleep(100 * time.Millisecond)

	path := filepath.Join(root, "sub", "deep.jsonl")
	write(t, path) // mkdir sub + file in one shot

	waitFor(t, events, func(e Event) bool { return e.Path == path && !e.Removed })
}

func TestWatchStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, t.TempDir(), ".jsonl", func(Event) {}) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after ctx cancellation (goroutine leak)")
	}
}
