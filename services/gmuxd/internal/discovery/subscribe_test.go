package discovery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// TestSubscribeReplacesExistingForSameID guards the contract that
// Subscribe is "cancel-and-replace" rather than "no-op-if-existing."
//
// Restart sequence (see ADR 0003):
//
//  1. /kill returns 204 once R1's PTY child has exited and the
//     sockfile path is unlinked. R1's listener stays up briefly so
//     the daemon's existing SSE subscription can drain.
//  2. The daemon launches R2 under the same canonical session id.
//  3. R2 calls /v1/register, which triggers Register → subs.Subscribe.
//  4. R1 (still alive) eventually fires its /v1/deregister and exits.
//
// Steps 3 and 4 race. Before this fix, if step 3 won, Subscribe found
// sub.active[id] still occupied by R1 and no-op'd, leaving R2 with
// no SSE subscription at all. Status / meta / exit events from R2
// would be silently dropped until the next discovery scan caught the
// missing subscription.
//
// This test reproduces the race deterministically: R1's /events
// handler holds open while R2's emits a single "meta" event that
// updates the store. After Subscribe(id, R2) is layered over an
// existing Subscribe(id, R1), the store's session title must
// reflect R2's event, not R1's silence.
func TestSubscribeReplacesExistingForSameID(t *testing.T) {
	// R1: holds /events open indefinitely, never emits anything.
	holdR1 := make(chan struct{})
	t.Cleanup(func() { close(holdR1) })
	r1 := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-holdR1:
		}
	}))
	t.Cleanup(r1.cleanup)

	// R2: emits a single "meta" event with a recognizable title,
	// then keeps the stream open so the subscription stays IsActive.
	holdR2 := make(chan struct{})
	t.Cleanup(func() { close(holdR2) })
	const r2Title = "from-replacement-runner"
	r2 := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		body, _ := json.Marshal(map[string]any{"adapter_title": r2Title})
		fmt.Fprintf(w, "event: meta\ndata: %s\n\n", body)
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-holdR2:
		}
	}))
	t.Cleanup(r2.cleanup)

	const id = "sess-restart-race"
	sessions := store.New()
	sessions.Upsert(store.Session{ID: id, Kind: "shell", Alive: true, SocketPath: r1.socketPath})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	// Establish R1's subscription first and wait for the SSE
	// connection to actually open at the fake runner. IsActive flips
	// true synchronously on Subscribe (before the goroutine dials),
	// so without this wait we'd be testing the easy-mode "old
	// goroutine hasn't started yet" path instead of the race.
	subs.Subscribe(id, r1.socketPath)
	r1.waitOpen(t, 1)

	// Layer R2's subscription on top with the existing entry still
	// in the active map. Pre-fix, this Subscribe call no-op'd and
	// R2's /events was never dialed.
	subs.Subscribe(id, r2.socketPath)
	r2.waitOpen(t, 1)

	// Wait for the meta event to flow through R2's subscription and
	// reach the store. handleEvent → store.Update is asynchronous
	// from the test's perspective.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := sessions.Get(id)
		if got.AdapterTitle == r2Title {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got, _ := sessions.Get(id)
	if got.AdapterTitle != r2Title {
		t.Errorf("AdapterTitle = %q, want %q (R2's meta event must have reached the store; the existing R1 subscription must not block R2's Subscribe call)",
			got.AdapterTitle, r2Title)
	}
	if !subs.IsActive(id) {
		t.Errorf("subscription disappeared from active map (the dying R1 goroutine's defer must skip the delete when its entry has been replaced)")
	}
}

// TestSubscribeOldDeferDoesNotEvictNewEntry pins the load-bearing
// pointer-identity check in runSubscription's cleanup defer.
//
// Sequence: an old subscription's runSubscription returns (e.g. the
// runner's listener went away mid-stream), enters its cleanup defer
// and contends for sub.mu against a fresh Subscribe for the same id.
// If the defer's delete is unconditional, it can clear the map entry
// the new Subscribe just installed.
//
// We force the contention deterministically by canceling the first
// subscription and immediately resubscribing under a fresh socket;
// after both calls, IsActive must remain true.
func TestSubscribeOldDeferDoesNotEvictNewEntry(t *testing.T) {
	first := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(first.cleanup)

	second := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(second.cleanup)

	const id = "sess-defer-eviction"
	sessions := store.New()
	sessions.Upsert(store.Session{ID: id, Kind: "shell", Alive: true})

	subs := NewSubscriptions(sessions)
	t.Cleanup(func() { subs.UnsubscribeAll() })

	subs.Subscribe(id, first.socketPath)
	first.waitOpen(t, 1)

	// Replace; the first goroutine's cleanup defer races with this
	// call. Subscribe holds sub.mu across cancel-and-overwrite, so
	// the old defer's mu acquisition serializes after the overwrite.
	subs.Subscribe(id, second.socketPath)
	second.waitOpen(t, 1)

	// Allow the old goroutine's defer to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && first.open.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if first.open.Load() != 0 {
		t.Fatalf("first subscription's connection did not close after replacement")
	}

	if !subs.IsActive(id) {
		t.Error("subscription cleared from active map; the old goroutine's defer must compare entry identity before deleting")
	}

	// Sanity: a brand-new Unsubscribe should still find the entry.
	subs.Unsubscribe(id)
	if subs.IsActive(id) {
		t.Error("Unsubscribe failed to clear the second subscription's entry")
	}
}
