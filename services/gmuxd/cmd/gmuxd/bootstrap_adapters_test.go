package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

type capabilityAdapter struct {
	name     string
	describe func(string) (*adapter.ConversationInfo, error)
	probe    func(string) (bool, bool)
}

func (a capabilityAdapter) Name() string                  { return a.name }
func (capabilityAdapter) Discover() bool                  { return true }
func (capabilityAdapter) Match([]string) bool             { return false }
func (capabilityAdapter) Env(adapter.EnvContext) []string { return nil }
func (a capabilityAdapter) DescribeConversation(ref string) (*adapter.ConversationInfo, error) {
	return a.describe(ref)
}
func (a capabilityAdapter) ConversationGone(ref string) (bool, bool) { return a.probe(ref) }

type baseOnlyAdapter struct{ name string }

func (a baseOnlyAdapter) Name() string                  { return a.name }
func (baseOnlyAdapter) Discover() bool                  { return true }
func (baseOnlyAdapter) Match([]string) bool             { return false }
func (baseOnlyAdapter) Env(adapter.EnvContext) []string { return nil }

func withAdapters(t *testing.T, values ...adapter.Adapter) {
	t.Helper()
	old := adapters.All
	adapters.All = append([]adapter.Adapter(nil), values...)
	t.Cleanup(func() { adapters.All = old })
}

func TestProductionConversationCapabilitiesDispatch(t *testing.T) {
	withAdapters(t,
		baseOnlyAdapter{name: "missing-cap"},
		capabilityAdapter{name: "cap", describe: func(ref string) (*adapter.ConversationInfo, error) {
			if ref == "fail" {
				return nil, errors.New("describe failed")
			}
			return &adapter.ConversationInfo{ID: "id-" + ref, AncestorIDs: []string{"parent"}}, nil
		}, probe: func(ref string) (bool, bool) {
			switch ref {
			case "gone":
				return true, true
			case "live":
				return false, true
			default:
				return false, false
			}
		}},
	)
	r := productionConversationResolver{}
	info, err := r.DescribeConversation(context.Background(), "cap", "ok")
	if err != nil || info.ID != "id-ok" || len(info.AncestorIDs) != 1 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	for _, name := range []string{"missing-cap", "unknown"} {
		if _, err := r.DescribeConversation(context.Background(), name, "x"); err == nil {
			t.Fatalf("%s unexpectedly described", name)
		}
	}
	if _, err := r.DescribeConversation(context.Background(), "cap", "fail"); err == nil {
		t.Fatal("describe failure swallowed")
	}

	batch := []sessioncoord.ReconcileCandidate{{ID: "a", ConversationRef: "gone"}, {ID: "b", ConversationRef: "live"}, {ID: "c", ConversationRef: "unknown"}}
	got, err := (productionAdapterReconciler{}).ReconcileRetained(context.Background(), "cap", batch)
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	want := []sessioncoord.Disposition{sessioncoord.DispositionRemove, sessioncoord.DispositionRetain, sessioncoord.DispositionUnknown}
	for i := range want {
		if got[i].Disposition != want[i] {
			t.Fatalf("decision[%d]=%v", i, got[i])
		}
	}
	got, err = (productionAdapterReconciler{}).ReconcileRetained(context.Background(), "missing-cap", batch)
	if err != nil || got != nil {
		t.Fatalf("missing capability got=%+v err=%v", got, err)
	}
}
