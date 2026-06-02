package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// probePeerHealth is the contract the add-peer flow relies on: it must
// reach /v1/health, forward the bearer token, and pull node_id + name
// out of the {data:{…}} envelope — and fail clearly when the host is
// unreachable/unauthorized or reports no name.
func TestProbePeerHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":{"service":"gmuxd","node_id":"node_abc","hostname":"gmux-laptop"}}`))
	}))
	defer srv.Close()

	id, name, err := probePeerHealth(context.Background(), srv.URL, "tok")
	if err != nil {
		t.Fatalf("probePeerHealth: %v", err)
	}
	if id != "node_abc" || name != "gmux-laptop" {
		t.Fatalf("got (node_id=%q, name=%q), want (node_abc, gmux-laptop)", id, name)
	}
}

func TestProbePeerHealth_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, _, err := probePeerHealth(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error when the probe is unauthorized")
	}
}

func TestProbePeerHealth_MissingName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"service":"gmuxd","node_id":"node_abc"}}`))
	}))
	defer srv.Close()
	// A host that reports no name can't be given a routing identity.
	if _, _, err := probePeerHealth(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error when the host reports no name")
	}
}

func TestProbePeerHealth_NotGmux(t *testing.T) {
	// A reachable HTTP endpoint that isn't gmuxd must be rejected, so a
	// stray URL can't be registered as a peer (parity with discovery).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"service":"other","hostname":"x"}}`))
	}))
	defer srv.Close()
	if _, _, err := probePeerHealth(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected an error when the host is not running gmux")
	}
}
