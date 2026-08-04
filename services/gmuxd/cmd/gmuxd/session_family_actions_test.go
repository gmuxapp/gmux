package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func TestSessionFamilyMutationHTTPRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := centralstore.SessionID("root")
	for _, row := range []centralstore.NewSession{
		{ID: root, Adapter: "pi", Command: []string{"pi"}, CreatedAt: 1},
		{ID: "other", Adapter: "pi", Command: []string{"pi"}, CreatedAt: 2},
		{ID: "child", Adapter: "pi", Command: []string{"pi"}, CreatedAt: 3, ParentSessionID: &root},
	} {
		if _, _, err := store.InsertSession(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	boot := &Bootstrap{Store: store, Composer: central.New(store, nil, nil)}
	fanout := newSSEFanout()
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		handleCentralSessionAction(recorder, request, boot, fanout, nil, nil, nil, "")
		return recorder
	}

	if response := post("/v1/sessions/child/promote", `{}`); response.Code != http.StatusOK {
		t.Fatalf("promote status=%d body=%s", response.Code, response.Body.String())
	}
	row, _, _ := store.Session(ctx, "child")
	if !row.PromotedToRoot || row.ParentSessionID == nil || *row.ParentSessionID != "root" {
		t.Fatalf("promotion did not preserve parent: %#v", row)
	}
	if response := post("/v1/sessions/child/demote", `{}`); response.Code != http.StatusOK {
		t.Fatalf("demote status=%d body=%s", response.Code, response.Body.String())
	}
	row, _, _ = store.Session(ctx, "child")
	if row.PromotedToRoot || row.ParentSessionID == nil || *row.ParentSessionID != "root" {
		t.Fatalf("demotion did not restore child presentation: %#v", row)
	}

	if response := post("/v1/sessions/child/reparent", `{"parent_session_id":"other"}`); response.Code != http.StatusOK {
		t.Fatalf("reparent status=%d body=%s", response.Code, response.Body.String())
	}
	row, _, _ = store.Session(ctx, "child")
	if row.ParentSessionID == nil || *row.ParentSessionID != "other" || launchedFromExport(t, store, "child") != "root" {
		t.Fatalf("reparented row=%#v launched_from=%q", row, launchedFromExport(t, store, "child"))
	}

	batch, err := central.RenderAll(ctx, store, central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts { return nil }), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	projected := (&wire.Converter{Titlers: map[string]func([]string) string{}, SemanticAgents: map[string]bool{"pi": true}}).Sessions(batch.Sessions, batch.Projects, nil)
	var child wire.Session
	for _, session := range projected.Sessions {
		if session.ID == "child" {
			child = session
		}
	}
	if child.ID == "" || child.ParentSessionID != "other" || !child.SemanticAgent {
		t.Fatalf("daemon family projection=%#v", child)
	}
	encoded, err := json.Marshal(map[string]any{"ok": true, "data": projected.Sessions})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "launched_from_session_id") {
		t.Fatalf("historical launch provenance leaked onto snapshot/REST wire: %s", encoded)
	}

	if response := post("/v1/sessions/child/reparent", `{"parent_session_id":null}`); response.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", response.Code, response.Body.String())
	}
	row, _, _ = store.Session(ctx, "child")
	if row.ParentSessionID != nil || launchedFromExport(t, store, "child") != "root" {
		t.Fatalf("clear changed launch provenance: %#v launched_from=%q", row, launchedFromExport(t, store, "child"))
	}
}

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
	boot := &Bootstrap{Store: store, Composer: central.New(store, nil, nil)}
	request := func(id, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handleCentralSessionAction(recorder, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/reparent", strings.NewReader(body)), boot, newSSEFanout(), nil, nil, nil, "")
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
