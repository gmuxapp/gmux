package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseReadExplicitAndFamily(t *testing.T) {
	c, err := parseCLI([]string{"read", "a", "b"})
	if err != nil || c.mode != modeRead || len(c.readRefs) != 2 {
		t.Fatalf("explicit read = %+v, %v", c, err)
	}
	c, err = parseCLI([]string{"read", "--family", "root"})
	if err != nil || c.familyRef != "root" || len(c.readRefs) != 0 {
		t.Fatalf("family read = %+v, %v", c, err)
	}
	for _, args := range [][]string{{"read"}, {"read", "--family", "root", "child"}} {
		if _, err := parseCLI(args); err == nil {
			t.Fatalf("parseCLI(%v) succeeded", args)
		}
	}
}

func TestReadFamilyAcknowledgesRootAndAllDescendants(t *testing.T) {
	d := startStubDaemon(t, []cliSession{
		{ID: "root"},
		{ID: "child", ParentSessionID: "root"},
		{ID: "grandchild", ParentSessionID: "child"},
		{ID: "other"},
	})
	d.on(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	stdout := captureStdout(t, func() {
		if code := cmdRead(nil, "root"); code != 0 {
			t.Fatalf("read exit = %d", code)
		}
	})
	if !strings.Contains(stdout, "marked 3 session(s) read") {
		t.Fatalf("stdout = %q", stdout)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) != 3 {
		t.Fatalf("requests = %+v", d.requests)
	}
	for _, id := range []string{"root", "child", "grandchild"} {
		found := false
		for _, req := range d.requests {
			found = found || req.path == "/v1/sessions/"+id+"/read"
		}
		if !found {
			t.Errorf("missing read for %s: %+v", id, d.requests)
		}
	}
}

func TestSessionFamilyIncludesTransitiveDescendantsOnly(t *testing.T) {
	root := cliSession{ID: "root"}
	sessions := []cliSession{
		root,
		{ID: "child", ParentSessionID: "root"},
		{ID: "grandchild", ParentSessionID: "child"},
		{ID: "other"},
	}
	got := sessionFamily(sessions, root)
	if len(got) != 3 || got[0].ID != "root" || got[1].ID != "child" || got[2].ID != "grandchild" {
		t.Fatalf("family = %+v", got)
	}
}
