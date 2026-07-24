package centralstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestReadsSurviveHeldWriteConnection pins the pool separation directly:
// a transaction pinning the WRITE connection (pool of 1) must not block
// ReadSnapshot, because reads run on their own read-only pool. On the
// pre-split code (one shared pool, MaxOpenConns=1) this test fails
// deterministically: ReadSnapshot's BeginTx queues behind the held
// connection until the context deadline.
//
// This is the honest replacement for an earlier version of this test that
// tried to reproduce the field wedge via read saturation — sub-millisecond
// reads never starve a FIFO pool long enough to bite, and that version
// passed on the broken code.
func TestReadsSurviveHeldWriteConnection(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, _, err := s.RegisterRunner(ctx, RunnerRegistration{
		ID: "held-write", Adapter: "test", Alive: true,
		CreatedAt: UnixMillis(1000), ObservedAt: UnixMillis(1000),
		Facts: RunnerFacts{CWD: strPtr("/tmp")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pin the single write connection inside an open transaction.
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.ReadSnapshot(readCtx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadSnapshot failed while write connection was held: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ReadSnapshot blocked behind the held write connection: pools are not separated")
	}
}

// TestWritesSurviveHeldReadConnections is the inverse: transactions pinning
// every connection of the READ pool must not block a mutation, which runs
// on the dedicated write connection. Fails on a shared pool for the same
// reason as above.
func TestWritesSurviveHeldReadConnections(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Pin all read-pool connections (pool size 4).
	var txs []interface{ Rollback() error }
	for i := 0; i < 4; i++ {
		tx, err := s.readDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("pin read conn %d: %v", i, err)
		}
		txs = append(txs, tx)
	}
	defer func() {
		for _, tx := range txs {
			_ = tx.Rollback()
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, _, err := s.RegisterRunner(ctx, RunnerRegistration{
			ID: "held-read", Adapter: "test", Alive: true,
			CreatedAt: UnixMillis(2000), ObservedAt: UnixMillis(2000),
			Facts: RunnerFacts{CWD: strPtr("/tmp")},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RegisterRunner failed while read pool was held: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RegisterRunner blocked behind held read connections: pools are not separated")
	}
}

// TestReadPoolConcurrentReadWrite runs concurrent readers and writers under
// -race to verify the separate pools don't introduce data races.
func TestReadPoolConcurrentReadWrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const writers = 4
	const readers = 4
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for w := range writers {
		go func() {
			defer wg.Done()
			for i := range iterations {
				id := SessionID(fmt.Sprintf("rw-%d-%d", w, i))
				_, _, _ = s.RegisterRunner(ctx, RunnerRegistration{
					ID: id, Adapter: "test", Alive: true,
					CreatedAt: UnixMillis(int64(w*1000 + i)), ObservedAt: UnixMillis(int64(w*1000 + i)),
					Facts: RunnerFacts{CWD: strPtr("/tmp")},
				})
			}
		}()
	}
	for range readers {
		go func() {
			defer wg.Done()
			for range iterations {
				_, _ = s.ReadSnapshot(ctx, SnapshotQuery{IncludeSessions: true, IncludeProjects: true})
			}
		}()
	}
	wg.Wait()
}

func openTestStoreInDir(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func strPtr(s string) *string { return &s }
