package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

// Translation is the whole of the peer-reparent policy: what crosses the
// boundary, in whose namespace, and what is refused.
func TestRewritePeerReparentBody(t *testing.T) {
	for _, tc := range []struct {
		name, body, childPeer, wantForward, wantCode string
	}{
		// Promote-to-root is reparent-to-null: no reference to translate and
		// nothing cross-peer about it.
		{"promote", `{"parent_session_id":null}`, "box", `{"parent_session_id":null}`, ""},
		{"same peer parent", `{"parent_session_id":"p@box"}`, "box", `{"parent_session_id":"p"}`, ""},
		// A hub two hops out addresses the far session as p@mid@box; the next
		// hop keeps resolving its own suffix.
		{"chained peer parent", `{"parent_session_id":"p@mid@box"}`, "box", `{"parent_session_id":"p@mid"}`, ""},
		{"local parent", `{"parent_session_id":"p"}`, "box", "", codeCrossPeer},
		{"other peer parent", `{"parent_session_id":"p@other"}`, "box", "", codeCrossPeer},
		{"missing field", `{}`, "box", "", "bad_request"},
		{"empty parent", `{"parent_session_id":""}`, "box", "", "bad_request"},
		{"non-string parent", `{"parent_session_id":3}`, "box", "", "bad_request"},
		{"invalid json", `{`, "box", "", "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forward, code, message := rewritePeerReparentBody([]byte(tc.body), tc.childPeer)
			if code != tc.wantCode {
				t.Fatalf("code=%q message=%q, want %q", code, message, tc.wantCode)
			}
			if tc.wantCode != "" {
				if message == "" {
					t.Fatal("a refusal owes a reason")
				}
				return
			}
			var got, want map[string]any
			if err := json.Unmarshal(forward, &got); err != nil {
				t.Fatalf("forwarded body is not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.wantForward), &want); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("forwarded %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// An additive field on this route must survive the hub's rewrite rather than
// being dropped on the way to the owning daemon.
func TestRewritePeerReparentBodyKeepsUnknownFields(t *testing.T) {
	forward, code, _ := rewritePeerReparentBody([]byte(`{"parent_session_id":"p@box","future":42}`), "box")
	if code != "" {
		t.Fatalf("code=%q", code)
	}
	var got map[string]any
	if err := json.Unmarshal(forward, &got); err != nil {
		t.Fatal(err)
	}
	if got["parent_session_id"] != "p" || got["future"] != float64(42) {
		t.Fatalf("forwarded %v", got)
	}
}

type spokeReparent struct {
	mu   sync.Mutex
	path string
	body string
}

func (s *spokeReparent) record(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.body = path, body
}

func (s *spokeReparent) seen() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path, s.body
}

// The hub must not apply a peer session's family edge to its own projection,
// and must not refuse it either: the request belongs to the owning daemon,
// under that peer's own credentials.
func TestPeerReparentIsForwardedToOwner(t *testing.T) {
	seen := &spokeReparent{}
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.record(r.URL.Path, string(body))
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})
	}))
	defer spoke.Close()
	pm := peering.NewProjectionManager(
		[]config.PeerConfig{{Name: "box", URL: spoke.URL, Token: "tok"}}, "self", nil, peering.EventHooks{})

	post := func(id, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/reparent", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		handleCentralSessionAction(recorder, req, nil, newSSEFanout(), nil, pm, nil, "", nil)
		return recorder
	}

	// Promote a peer child: forwarded verbatim, addressed by the owner's ID.
	if response := post("kid@box", `{"parent_session_id":null}`); response.Code != http.StatusOK {
		t.Fatalf("promote code=%d body=%s", response.Code, response.Body.String())
	}
	if path, body := seen.seen(); path != "/v1/sessions/kid/reparent" || body != `{"parent_session_id":null}` {
		t.Fatalf("spoke saw path=%q body=%q", path, body)
	}

	// Same-peer reparent: legitimate, with the parent translated out of the
	// viewer's namespace.
	if response := post("kid@box", `{"parent_session_id":"boss@box"}`); response.Code != http.StatusOK {
		t.Fatalf("reparent code=%d body=%s", response.Code, response.Body.String())
	}
	path, body := seen.seen()
	if path != "/v1/sessions/kid/reparent" {
		t.Fatalf("spoke saw path=%q", path)
	}
	var forwarded map[string]any
	if err := json.Unmarshal([]byte(body), &forwarded); err != nil {
		t.Fatalf("spoke saw %q: %v", body, err)
	}
	if forwarded["parent_session_id"] != "boss" {
		t.Fatalf("spoke saw parent %v, want the peer-local id", forwarded["parent_session_id"])
	}

	// Cross-peer stays refused, and never reaches the wire.
	seen.record("", "")
	response := post("kid@box", `{"parent_session_id":"local-boss"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), codeCrossPeer) {
		t.Fatalf("cross-peer code=%d body=%s", response.Code, response.Body.String())
	}
	if path, _ := seen.seen(); path != "" {
		t.Fatalf("refused request still forwarded to %q", path)
	}
	if response := post("kid@box", `{"parent_session_id":"other@elsewhere"}`); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), codeCrossPeer) {
		t.Fatalf("other-peer code=%d body=%s", response.Code, response.Body.String())
	}
	if path, _ := seen.seen(); path != "" {
		t.Fatalf("refused request still forwarded to %q", path)
	}
	// A GET is not a mutation; it must not be proxied as one.
	recorder := httptest.NewRecorder()
	handleCentralSessionAction(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions/kid@box/reparent", nil),
		nil, newSSEFanout(), nil, pm, nil, "", nil)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code=%d", recorder.Code)
	}
}

// The mirror image: a local child cannot join a peer's family. The refusal
// must say so instead of surfacing as "that parent does not exist here".
func TestLocalReparentUnderPeerParentIsRefusedAsCrossPeer(t *testing.T) {
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := store.InsertSession(ctx, centralstore.NewSession{
		ID: "a", Adapter: "shell", Command: []string{"sh"}, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	coord := sessioncoord.New(nil, nil, store, nil, nil)
	boot := &Bootstrap{Store: store, Coordinator: coord, Composer: central.New(store, nil, nil)}
	pm := peering.NewProjectionManager(
		[]config.PeerConfig{{Name: "box", URL: "http://127.0.0.1:1"}}, "self", nil, peering.EventHooks{})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/a/reparent", strings.NewReader(`{"parent_session_id":"boss@box"}`))
	req.Header.Set("Content-Type", "application/json")
	handleCentralSessionAction(recorder, req, boot, newSSEFanout(), nil, pm, nil, "", nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), codeCrossPeer) {
		t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// Promoting a local session with a peer roster present is untouched.
	recorder = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/a/reparent", strings.NewReader(`{"parent_session_id":null}`))
	req.Header.Set("Content-Type", "application/json")
	handleCentralSessionAction(recorder, req, boot, newSSEFanout(), nil, pm, nil, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("local promote code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
