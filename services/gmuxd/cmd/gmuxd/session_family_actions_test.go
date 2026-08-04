package main

import (
	"context"
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
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, row := range []centralstore.NewSession{
		{ID: "root", Adapter: "pi", Command: []string{"pi"}, CreatedAt: 1},
		{ID: "other", Adapter: "pi", Command: []string{"pi"}, CreatedAt: 2},
		{ID: "child", Adapter: "pi", Command: []string{"pi"}, CreatedAt: 3, LaunchParentID: sessionIDPtr("root")},
	} {
		if _, _, err := st.InsertSession(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	boot := &Bootstrap{Store: st, Composer: central.New(st, nil, nil)}
	fanout := newSSEFanout()
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		handleCentralSessionAction(rec, req, boot, fanout, nil, nil, nil, "")
		return rec
	}

	if rec := post("/v1/sessions/child/promote", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("promote status=%d body=%s", rec.Code, rec.Body.String())
	}
	row, _, _ := st.Session(ctx, "child")
	if !row.PromotedToRoot || row.LaunchParentID == nil || *row.LaunchParentID != "root" {
		t.Fatalf("promotion did not preserve provenance: %#v", row)
	}
	if rec := post("/v1/sessions/child/demote", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("demote status=%d body=%s", rec.Code, rec.Body.String())
	}
	row, _, _ = st.Session(ctx, "child")
	if row.PromotedToRoot || row.LaunchParentID == nil || *row.LaunchParentID != "root" {
		t.Fatalf("demotion did not restore edge: %#v", row)
	}

	if rec := post("/v1/sessions/child/parent", `{"parent_session_id":"other"}`); rec.Code != http.StatusOK {
		t.Fatalf("reparent status=%d body=%s", rec.Code, rec.Body.String())
	}
	row, _, _ = st.Session(ctx, "child")
	if row.LaunchParentID == nil || *row.LaunchParentID != "other" {
		t.Fatalf("reparent not stored: %#v", row)
	}
	batch, err := central.RenderAll(ctx, st, central.RuntimeSourceFunc(func() map[centralstore.SessionID]central.RuntimeFacts { return nil }), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	projected := (&wire.Converter{Titlers: map[string]func([]string) string{}, SemanticAgents: map[string]bool{"pi": true}}).Sessions(batch.Sessions, batch.Projects, nil)
	foundProjected := false
	for _, session := range projected.Sessions {
		if session.ID == "child" {
			foundProjected = true
			if session.ParentSessionID != "other" || !session.SemanticAgent {
				t.Fatalf("daemon family projection=%#v", session)
			}
		}
	}
	if !foundProjected {
		t.Fatal("child absent from daemon projection")
	}
	if rec := post("/v1/sessions/child/parent", `{"parent_session_id":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	row, _, _ = st.Session(ctx, "child")
	if row.LaunchParentID != nil {
		t.Fatalf("clear not stored: %#v", row)
	}
}

func TestSessionParentHTTPValidation(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []centralstore.SessionID{"a", "b"} {
		if _, _, err := st.InsertSession(ctx, centralstore.NewSession{ID: id, Adapter: "shell", Command: []string{"sh"}, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	boot := &Bootstrap{Store: st, Composer: central.New(st, nil, nil)}
	fanout := newSSEFanout()
	request := func(id, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handleCentralSessionAction(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+id+"/parent", strings.NewReader(body)), boot, fanout, nil, nil, nil, "")
		return rec
	}
	if rec := request("a", `{"parent_session_id":"a"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "self_parent") {
		t.Fatalf("self response=%d %s", rec.Code, rec.Body.String())
	}
	if rec := request("a", `{"parent_session_id":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing response=%d %s", rec.Code, rec.Body.String())
	}
	if rec := request("a", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing field response=%d %s", rec.Code, rec.Body.String())
	}
	if rec := request("a", `{"parent_session_id":"b"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := request("b", `{"parent_session_id":"a"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "parent_cycle") {
		t.Fatalf("cycle response=%d %s", rec.Code, rec.Body.String())
	}
}

func sessionIDPtr(id centralstore.SessionID) *centralstore.SessionID { return &id }
