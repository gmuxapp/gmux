package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// TestStoreDirectReadYourWrites verifies that a mutation committed to the
// store is visible on the very next REST GET — no sleeps, no retries. This
// is the contract that store-direct reads (ADR 0026 §2a) guarantee.
func TestStoreDirectReadYourWrites(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	converter := &wire.Converter{
		Titlers: make(map[string]func([]string) string),
	}

	t.Run("register then GET /v1/sessions", func(t *testing.T) {
		_, _, err := st.InsertSession(ctx, centralstore.NewSession{
			ID: "read-your-write-sess", Adapter: "shell",
			Command:   []string{"sh"},
			CreatedAt: centralstore.UnixMillis(time.Now().UnixMilli()),
		})
		if err != nil {
			t.Fatal(err)
		}

		// Render store-direct (same path as the REST handler).
		batch, err := central.RenderAll(ctx, st,
			central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts { return nil }),
			nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		frames := wire.Frames{}
		if batch.Sessions != nil {
			p := converter.Sessions(batch.Sessions, batch.Projects, nil)
			frames.Sessions = &p
		}
		if frames.Sessions == nil {
			t.Fatal("sessions payload is nil")
		}
		found := false
		for _, s := range frames.Sessions.Sessions {
			if s.ID == "read-your-write-sess" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("session not visible in store-direct read immediately after insert")
		}
	})

	t.Run("PUT projects then GET /v1/projects", func(t *testing.T) {
		_, _, err := st.ReplaceProjectCatalog(ctx, []centralstore.ProjectEntrySpec{{
			Owned: &centralstore.OwnedProjectSpec{
				Slug:  "test-proj",
				Rules: []centralstore.MatchRule{{Path: "/tmp/test"}},
			},
		}}, centralstore.UnixMillis(time.Now().UnixMilli()))
		if err != nil {
			t.Fatal(err)
		}

		batch, err := central.RenderAll(ctx, st,
			central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts { return nil }),
			nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if batch.Projects == nil {
			t.Fatal("projects payload is nil")
		}
		found := false
		for _, p := range batch.Projects.Projects {
			if p.Slug == "test-proj" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("project not visible in store-direct read immediately after ReplaceCatalog")
		}
	})
}

// TestStoreDirectWaitExistenceCheck verifies that the wait handler's
// store-direct existence check sees a just-inserted session.
func TestStoreDirectWaitExistenceCheck(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	_, _, err = st.InsertSession(ctx, centralstore.NewSession{
		ID: "wait-exist", Adapter: "shell", Command: []string{"sh"},
		CreatedAt: centralstore.UnixMillis(time.Now().UnixMilli()),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The session should be visible via Store.Session immediately.
	_, found, err := st.Session(ctx, "wait-exist")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("session not found via store-direct lookup immediately after insert")
	}
}

// TestStoreDirectConcurrentReadsRaceFree exercises concurrent store-direct
// renders alongside mutations under the race detector.
func TestStoreDirectConcurrentReadsRaceFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	converter := &wire.Converter{
		Titlers: make(map[string]func([]string) string),
	}
	runtime := central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts {
		return nil
	})

	// Seed a few sessions.
	for i := 0; i < 10; i++ {
		id := centralstore.SessionID(string(rune('a'+i)) + "-race")
		st.InsertSession(ctx, centralstore.NewSession{
			ID: id, Adapter: "shell", Command: []string{"sh"},
			CreatedAt: centralstore.UnixMillis(time.Now().UnixMilli()),
		})
	}

	var wg sync.WaitGroup
	// Writers: insert sessions concurrently.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				select {
				case <-ctx.Done():
					return
				default:
				}
				id := centralstore.SessionID(randomID())
				st.InsertSession(ctx, centralstore.NewSession{
					ID: id, Adapter: "shell", Command: []string{"sh"},
					CreatedAt: centralstore.UnixMillis(time.Now().UnixMilli()),
				})
			}
		}(w)
	}
	// Readers: render store-direct concurrently.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				select {
				case <-ctx.Done():
					return
				default:
				}
				batch, err := central.RenderAll(ctx, st, runtime, nil, nil)
				if err != nil {
					continue
				}
				if batch.Sessions != nil {
					_ = converter.Sessions(batch.Sessions, batch.Projects, nil)
				}
			}
		}()
	}
	wg.Wait()
}

// TestStoreDirectHealthCountsFresh verifies that health session counts
// derived from a store-direct render reflect the current state, not a
// stale cache.
func TestStoreDirectHealthCountsFresh(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert a session.
	_, _, err = st.InsertSession(ctx, centralstore.NewSession{
		ID: "health-count", Adapter: "shell", Command: []string{"sh"},
		CreatedAt: centralstore.UnixMillis(time.Now().UnixMilli()),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Render with runtime showing it alive.
	runtimeSrc := central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts {
		return map[centralstore.SessionID]central.RuntimeFacts{
			"health-count": {PID: 42, Endpoint: "/tmp/x"},
		}
	})
	converter := &wire.Converter{Titlers: make(map[string]func([]string) string)}

	batch, err := central.RenderAll(ctx, st, runtimeSrc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := converter.Sessions(batch.Sessions, batch.Projects, nil)
	var alive int
	for _, s := range p.Sessions {
		if s.Alive {
			alive++
		}
	}
	if alive != 1 {
		t.Fatalf("expected 1 alive session, got %d", alive)
	}

	// Now render with runtime empty (session dead).
	runtimeDead := central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts {
		return nil
	})
	batch2, err := central.RenderAll(ctx, st, runtimeDead, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2 := converter.Sessions(batch2.Sessions, batch2.Projects, nil)
	alive = 0
	for _, s := range p2.Sessions {
		if s.Alive {
			alive++
		}
	}
	if alive != 0 {
		t.Fatalf("expected 0 alive sessions after runtime cleared, got %d", alive)
	}
}

// TestRenderStoreDirectHTTPHandler exercises the full renderStoreDirect
// path through a simulated HTTP handler.
func TestRenderStoreDirectHTTPHandler(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	converter := &wire.Converter{Titlers: make(map[string]func([]string) string)}
	runtimeSrc := central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts {
		return nil
	})
	// Insert a session.
	_, _, err = st.InsertSession(ctx, centralstore.NewSession{
		ID: "http-test", Adapter: "shell", Command: []string{"sh"},
		CreatedAt: centralstore.UnixMillis(time.Now().UnixMilli()),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate GET /v1/sessions using RenderAll directly (avoids
	// needing a full peer-adapter with a live peering.Manager).
	batch, err := central.RenderAll(ctx, st, runtimeSrc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var frames wire.Frames
	if batch.Sessions != nil {
		p := converter.Sessions(batch.Sessions, batch.Projects, nil)
		frames.Sessions = &p
	}
	if err != nil {
		t.Fatal(err)
	}
	sessions := []wire.Session{}
	if frames.Sessions != nil {
		sessions = frames.Sessions.Sessions
	}
	found := false
	for _, s := range sessions {
		if s.ID == "http-test" {
			found = true
		}
	}
	if !found {
		t.Fatal("http-test session not found in renderStoreDirect result")
	}

	// Verify JSON shape matches the flat-array contract.
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]any{"ok": true, "data": sessions})
	var resp struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatal("response not ok")
	}
	// data must be an array, not an object.
	if len(resp.Data) == 0 || resp.Data[0] != '[' {
		t.Fatalf("data is not an array: %s", resp.Data)
	}

	// Verify wait handler sees the session immediately.
	_, found, err = st.Session(ctx, "http-test")
	if err != nil || !found {
		t.Fatal("wait existence check failed for just-inserted session")
	}
}

func randomID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// Ensure http import is used.
var _ = http.StatusOK
