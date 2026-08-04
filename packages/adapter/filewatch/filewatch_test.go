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

// writeUntilObserved polls by writing path until the watcher reports it. This
// avoids depending on a fixed delay for the Watch goroutine to install watches.
func writeUntilObserved(t *testing.T, events <-chan Event, path string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case e := <-events:
			if e.Path == path && !e.Removed {
				return
			}
		case <-poll.C:
			write(t, path)
		case <-deadline.C:
			t.Fatalf("timed out waiting for event for %s", path)
		}
	}
}

func TestWatchRewatchesRecreatedSubdir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	events := make(chan Event, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, root, ".jsonl", func(e Event) { events <- e })

	writeUntilObserved(t, events, filepath.Join(dir, "before.jsonl"))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUntilObserved(t, events, filepath.Join(dir, "after.jsonl"))
}

func TestWatchRewatchesRecreatedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}

	events := make(chan Event, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, root, ".jsonl", func(e Event) { events <- e })

	writeUntilObserved(t, events, filepath.Join(root, "before.jsonl"))
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeUntilObserved(t, events, filepath.Join(root, "after.jsonl"))
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
