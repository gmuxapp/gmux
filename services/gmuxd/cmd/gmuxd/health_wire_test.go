package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
)

// TestHealthWireDataCarriesHealthInfoFields pins the struct↔wire pairing.
//
// /v1/health builds its data map field by field, so a new field on
// central.HealthInfo is invisible to every consumer until it is published
// HERE too. That is not hypothetical: health.started_at (the per-boot marker
// the CLI renders next to the never-connected count) shipped on the struct,
// passed every Go test, and was absent from the wire — the CLI silently fell
// back to the pre-PR render. A reflection check makes the whole class of bug
// structural instead of per-field.
//
// auth_token is exempt: it is injected by the handler for local Unix-socket
// connections only and must never ride snapshot.world.
func TestHealthWireDataCarriesHealthInfoFields(t *testing.T) {
	h := central.HealthInfo{
		Service: "gmuxd", Version: "1.2.3", NodeID: "n1", Status: "ready",
		StartedAt: "2026-10-14T07:03:00Z", Hostname: "h", Listen: "127.0.0.1:8790",
		TailscaleURL: "https://h.ts.net", UpdateAvailable: "1.2.4",
		RunnerHash: "abc", DefaultLauncher: "shell",
		SessionRecovery: &central.SessionRecovery{Status: "ready"},
		UpstreamAccess:  &central.UpstreamAccess{State: "ok"},
		Tailscale:       &tsauth.DiagStatus{FQDN: "h.ts.net"},
	}
	data := healthWireData(h, map[string]any{"state": "ready"}, false, "")

	typ := reflect.TypeOf(h)
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" || name == "auth_token" {
			continue
		}
		if _, ok := data[name]; !ok {
			t.Errorf("HealthInfo field %q (json %q) is never published on /v1/health: wire keys = %v",
				typ.Field(i).Name, name, sortedKeys(data))
		}
	}

	// The values that the CLI's never-connected render turns on must survive
	// the projection, not merely be present.
	if got := data["started_at"]; got != "2026-10-14T07:03:00Z" {
		t.Errorf("started_at = %v, want the daemon boot time", got)
	}
	if data["upstream_access"] == nil {
		t.Error("upstream_access dropped from the wire")
	}

	// auth_token: only on the unix socket, never on TCP.
	if _, ok := data["auth_token"]; ok {
		t.Error("auth_token published on a non-unix health response")
	}
	tcp := healthWireData(h, nil, false, "secret")
	if _, ok := tcp["auth_token"]; ok {
		t.Error("auth_token leaked to a TCP health response")
	}
	unix := healthWireData(h, nil, true, "secret")
	if unix["auth_token"] != "secret" {
		t.Error("auth_token missing from the unix-socket health response")
	}

	// The wire stays JSON-encodable with an empty diagnostic set (the pre-PR
	// shape): nil pointers are omitted, not emitted as nulls-with-keys.
	bare := healthWireData(central.HealthInfo{Service: "gmuxd"}, map[string]any{}, false, "")
	b, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("wire not JSON-encodable: %v", err)
	}
	for _, absent := range []string{"session_recovery", "upstream_access", "tailscale", "tailscale_url", "update_available", "started_at"} {
		if strings.Contains(string(b), "\""+absent+"\"") {
			t.Errorf("empty diagnostic %q must be omitted from the wire, got %s", absent, b)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
