package main

import (
	"reflect"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// TestReconcileDeletedConversations_SafetyGate is the core regression
// guard: a session is retired only when the adapter is *confident* the
// file is gone (known && gone). An undeterminable answer (known=false,
// e.g. storage unreachable) must never retire — that's the whole reason
// the reconcile asks an adapter instead of stat'ing the file directly.
func TestReconcileDeletedConversations_SafetyGate(t *testing.T) {
	list := []store.Session{
		{ID: "deleted", Kind: "claude", SessionFile: "/c/deleted.jsonl"},
		{ID: "present", Kind: "claude", SessionFile: "/c/present.jsonl"},
		{ID: "unreachable", Kind: "codex", SessionFile: "/x/unreachable.jsonl"},
	}
	probe := func(kind, path string) (gone, known bool) {
		switch path {
		case "/c/deleted.jsonl":
			return true, true // confidently deleted
		case "/c/present.jsonl":
			return false, true // file still there
		case "/x/unreachable.jsonl":
			return true, false // storage unreachable — undeterminable
		}
		return false, false
	}

	var retired []string
	reconcileDeletedConversations(list, probe, func(p string) { retired = append(retired, p) })

	if !reflect.DeepEqual(retired, []string{"/c/deleted.jsonl"}) {
		t.Fatalf("only the confidently-deleted file should retire, got %v", retired)
	}
}

// TestReconcileDeletedConversations_Skips pins that alive, peer-owned,
// and file-less sessions are never probed or retired.
func TestReconcileDeletedConversations_Skips(t *testing.T) {
	list := []store.Session{
		{ID: "alive", Kind: "claude", SessionFile: "/c/a.jsonl", Alive: true},
		{ID: "peer", Kind: "claude", SessionFile: "/c/b.jsonl", Peer: "box2"},
		{ID: "nofile", Kind: "claude", SessionFile: ""},
	}
	var probed, retired []string
	reconcileDeletedConversations(list,
		func(_, path string) (bool, bool) { probed = append(probed, path); return true, true },
		func(p string) { retired = append(retired, p) })

	if len(probed) != 0 {
		t.Errorf("alive/peer/file-less sessions must not be probed, probed %v", probed)
	}
	if len(retired) != 0 {
		t.Errorf("nothing should be retired, retired %v", retired)
	}
}

// TestReconcileDeletedConversations_DedupsPaths pins that an N:1
// conversation (several dead sessions sharing one file) is probed and
// retired once — retire is expected to drop all matching sessions.
func TestReconcileDeletedConversations_DedupsPaths(t *testing.T) {
	const shared = "/c/shared.jsonl"
	list := []store.Session{
		{ID: "d1", Kind: "claude", SessionFile: shared},
		{ID: "d2", Kind: "claude", SessionFile: shared},
		{ID: "d3", Kind: "claude", SessionFile: shared},
	}
	var probed, retired []string
	reconcileDeletedConversations(list,
		func(_, path string) (bool, bool) { probed = append(probed, path); return true, true },
		func(p string) { retired = append(retired, p) })

	if !reflect.DeepEqual(probed, []string{shared}) {
		t.Errorf("shared path should be probed once, got %v", probed)
	}
	if !reflect.DeepEqual(retired, []string{shared}) {
		t.Errorf("shared path should be retired once, got %v", retired)
	}
}

// TestReconcileDeletedConversations_NoProberKind pins that a kind whose
// adapter can't probe (probe returns known=false) is left alone — this
// is how the real convProbe reports a missing/incapable adapter.
func TestReconcileDeletedConversations_NoProberKind(t *testing.T) {
	list := []store.Session{{ID: "s", Kind: "shell", SessionFile: "/s/x"}}
	var retired []string
	reconcileDeletedConversations(list,
		func(_, _ string) (bool, bool) { return false, false },
		func(p string) { retired = append(retired, p) })
	if len(retired) != 0 {
		t.Errorf("kind without a prober must not retire, got %v", retired)
	}
}
