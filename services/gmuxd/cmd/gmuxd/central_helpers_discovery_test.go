package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func TestGetProjectsRouteKeepsAdvertisingHostBoundary(t *testing.T) {
	frames := wire.Frames{
		Sessions: &wire.SessionsPayload{Sessions: []wire.Session{
			{ID: "local", Adapter: "shell", Cwd: "/work/local", Alive: true},
			{ID: "container@dev", Peer: "dev", Adapter: "shell", Cwd: "/work/container", Alive: true},
			{ID: "remote@node-c", Peer: "node-c", Adapter: "shell", Cwd: "/work/remote", Alive: true},
		}},
		World: &wire.WorldPayload{Projects: []wire.ProjectItem{{Slug: "configured"}}},
	}
	mux := http.NewServeMux()
	registerGetProjectsRoute(mux, func(*http.Request) (wire.Frames, error) { return frames, nil }, func(name string) bool { return name == "dev" })

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Configured           []projects.Item              `json:"configured"`
			Discovered           []projects.DiscoveredProject `json:"discovered"`
			UnmatchedActiveCount int                          `json:"unmatched_active_count"`
		} `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || len(envelope.Data.Configured) != 1 || envelope.Data.Configured[0].Slug != "configured" {
		t.Fatalf("configured projects changed: %#v", envelope)
	}
	gotPaths := make(map[string]bool, len(envelope.Data.Discovered))
	for _, row := range envelope.Data.Discovered {
		gotPaths[row.Paths[0]] = true
	}
	if len(envelope.Data.Discovered) != 2 || !gotPaths["/work/local"] || !gotPaths["/work/container"] || gotPaths["/work/remote"] {
		t.Fatalf("discovered = %#v, want local and Local-peer rows only", envelope.Data.Discovered)
	}
	if envelope.Data.UnmatchedActiveCount != 2 {
		t.Fatalf("unmatched_active_count = %d, want 2", envelope.Data.UnmatchedActiveCount)
	}
}
