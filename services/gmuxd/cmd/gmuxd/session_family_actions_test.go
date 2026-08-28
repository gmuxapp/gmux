package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
)

func launchedFromExport(t *testing.T, store *centralstore.Store, id centralstore.SessionID) string {
	t.Helper()
	state, err := store.ExportState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range state.Sessions {
		if session.ID == id && session.LaunchedFromSessionID != nil {
			return string(*session.LaunchedFromSessionID)
		}
	}
	return ""
}

func TestSessionReparentHTTPValidation(t *testing.T) {
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []centralstore.SessionID{"a", "b"} {
		if _, _, err := store.InsertSession(ctx, centralstore.NewSession{ID: id, Adapter: "shell", Command: []string{"sh"}, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	coord := sessioncoord.New(nil, nil, store, nil, nil)
	boot := &Bootstrap{Store: store, Coordinator: coord, Composer: central.New(store, nil, nil)}
	request := func(id, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handleCentralSessionAction(recorder, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/reparent", strings.NewReader(body)), boot, newSSEFanout(), nil, nil, nil, "", nil)
		return recorder
	}
	if response := request("a", `{"parent_session_id":"a"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "self_parent") {
		t.Fatalf("self response=%d %s", response.Code, response.Body.String())
	}
	if response := request("a", `{"parent_session_id":"missing"}`); response.Code != http.StatusNotFound {
		t.Fatalf("missing response=%d %s", response.Code, response.Body.String())
	}
	if response := request("a", `{}`); response.Code != http.StatusBadRequest {
		t.Fatalf("missing field response=%d %s", response.Code, response.Body.String())
	}
	if response := request("a", `{"parent_session_id":"b"}`); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if response := request("b", `{"parent_session_id":"a"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "parent_cycle") {
		t.Fatalf("cycle response=%d %s", response.Code, response.Body.String())
	}
}
