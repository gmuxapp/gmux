package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

func routeBudgetCoordinator(limit int, rows []centralstore.Session) *sessioncoord.Coordinator {
	return sessioncoord.New(nil, nil, nil, nil, nil,
		sessioncoord.WithActiveSubagentBudget([]int{limit}, false, func(adapter string) bool { return adapter == "pi" }, rows))
}

func TestActiveSubagentReservationAPIAllowedAndRejected(t *testing.T) {
	root := centralstore.SessionID("root")
	coord := routeBudgetCoordinator(1, []centralstore.Session{{ID: root, Adapter: "shell"}})

	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/agent-launch-reservations", strings.NewReader(`{"parent_session_id":"root"}`))
		handleActiveSubagentReservation(rec, req, coord)
		return rec
	}
	allowed := request()
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed status=%d body=%s", allowed.Code, allowed.Body.String())
	}
	var receipt struct {
		Data struct {
			Token  string `json:"token"`
			Root   string `json:"root_session_id"`
			Depth  int    `json:"depth"`
			Active int    `json:"active"`
			Limit  int    `json:"limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(allowed.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Data.Token == "" || receipt.Data.Root != "root" || receipt.Data.Depth != 1 || receipt.Data.Active != 0 || receipt.Data.Limit != 1 {
		t.Fatalf("receipt = %+v", receipt.Data)
	}

	rejected := request()
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	var envelope struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rejected.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != codeSubagentLimitReached || !strings.Contains(envelope.Error.Message, "subagent limit reached at depth 1 for root root: 1 of 1 autonomous subagents") || !strings.Contains(envelope.Error.Message, "gmux ls") {
		t.Fatalf("error = %+v", envelope.Error)
	}

	release := httptest.NewRecorder()
	handleActiveSubagentReservationRelease(release, httptest.NewRequest(http.MethodDelete, "/", nil), coord, receipt.Data.Token)
	if release.Code != http.StatusNoContent {
		t.Fatalf("release status=%d", release.Code)
	}
	if again := request(); again.Code != http.StatusOK {
		t.Fatalf("slot not released: %d %s", again.Code, again.Body.String())
	}
}

func TestFormatSubagentLimitMessageAtBlockedDepth(t *testing.T) {
	message := formatSubagentLimitMessage(&sessioncoord.SubagentLimitError{Root: "root", Depth: 3, Limit: 0})
	for _, want := range []string{"depth 3", "root root", "may not spawn subagents", "ask a human"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q missing %q", message, want)
		}
	}
}

func TestActiveSubagentReservationMountedProductionMux(t *testing.T) {
	coord := routeBudgetCoordinator(1, []centralstore.Session{{ID: "root", Adapter: "shell"}})
	mux := http.NewServeMux()
	registerActiveSubagentRoutes(mux, coord)
	server := httptest.NewServer(mux)
	defer server.Close()
	resp, err := http.Post(server.URL+"/v1/agent-launch-reservations", "application/json", strings.NewReader(`{"parent_session_id":"root"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestActiveSubagentReservationAPITopLevelRootsAreIndependent(t *testing.T) {
	coord := routeBudgetCoordinator(1, nil)
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/agent-launch-reservations", strings.NewReader(`{"parent_session_id":null}`))
		handleActiveSubagentReservation(rec, req, coord)
		if rec.Code != http.StatusOK {
			t.Fatalf("top-level launch %d status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}

func TestActiveSubagentReservationAPIStructuredValidation(t *testing.T) {
	coord := routeBudgetCoordinator(8, nil)
	for _, body := range []string{`{}`, `{"parent_session_id":""}`, `{"parent_session_id":3}`, `{"parent_session_id":null,"extra":true}`, `{`} {
		rec := httptest.NewRecorder()
		handleActiveSubagentReservation(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), coord)
		if body == `{}` {
			// Omitted and explicit null both mean no trustworthy behavioral
			// parent, preserving the existing top-level launch semantics.
			if rec.Code != http.StatusOK {
				t.Fatalf("body=%s status=%d", body, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}
