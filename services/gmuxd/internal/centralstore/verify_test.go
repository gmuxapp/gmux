package centralstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestVerifyMissingDatabase(t *testing.T) {
	err := Verify(context.Background(), filepath.Join(t.TempDir(), "state"))
	if !errors.Is(err, ErrDatabaseMissing) {
		t.Fatalf("expected ErrDatabaseMissing, got %v", err)
	}
}

func TestVerifyHealthyDatabase(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertSession(ctx, NewSession{ID: "sess-v1", Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{}, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, dir); err != nil {
		t.Fatalf("Verify on healthy DB: %v", err)
	}
}

// TestVerifyDoesNotMutate pins the read-only contract: Verify never creates,
// migrates, or repairs anything — the database bytes are identical before and
// after.
func TestVerifyDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, databaseName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Verify mutated the database file")
	}
}

// corrupt overwrites the cell content area of page 2 so
// integrity checks reliably fail.
func corrupt(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, databaseName)
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	junk := make([]byte, 512)
	for i := range junk {
		junk[i] = 0xA5
	}
	if _, err := f.WriteAt(junk, 4096+3584); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCorruptDatabaseFails(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertSession(ctx, NewSession{ID: "sess-v2", Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{}, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt(t, dir)
	err = Verify(ctx, dir)
	if err == nil {
		t.Fatal("expected Verify to fail on a corrupt database")
	}
	// Pin quick_check as the detection mechanism (not the downstream
	// schema-version read): removing quickCheck from Verify must fail this
	// assertion, not just shift the failure to a different error.
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity from quick_check, got %v", err)
	}
}

// TestOpenCorruptDatabaseFails pins the post-migration quick_check in Open:
// a corrupt page stops startup fail-closed and leaves the files in place.
func TestOpenCorruptDatabaseFails(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertSession(ctx, NewSession{ID: "sess-v3", Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{}, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt(t, dir)
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("expected Open to fail on a corrupt database")
	}
	if _, err := os.Stat(filepath.Join(dir, databaseName)); err != nil {
		t.Fatalf("database file must be preserved for diagnosis: %v", err)
	}
}

// TestVerifyUnderConcurrentWriter is the D-4 harness drill: phase 1a's
// read-only Verify runs against a WAL database that a concurrent writer is
// actively committing to. Verify must neither block indefinitely, nor fail
// on a healthy-but-busy database.
func TestVerifyUnderConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := SessionID(fmt.Sprintf("sess-w%04d", i))
			if _, _, err := store.InsertSession(ctx, NewSession{ID: id, Adapter: "shell", Command: []string{"sh"}, CWD: "/tmp", Remotes: map[string]string{}, CreatedAt: UnixMillis(i)}); err != nil {
				t.Errorf("writer insert: %v", err)
				return
			}
		}
	}()

	for range 10 {
		if err := Verify(ctx, dir); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("Verify failed against a healthy busy database: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
