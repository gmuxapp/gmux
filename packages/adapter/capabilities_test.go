package adapter

import (
	"testing"
	"time"
)

// fakeEncoder implements AgentActionEncoder and supports only
// ActionSend, exercising the reject path the capability allows for
// adapters that cannot express an action.
type fakeEncoder struct {
	Adapter // embed to satisfy the base interface without implementing it
}

func (f *fakeEncoder) EncodeAction(action AgentAction) (string, bool) {
	if action == ActionSend {
		return "\r", true
	}
	return "", false
}

func (f *fakeEncoder) ActionReadyTimeout() time.Duration { return 3 * time.Second }

// TestAgentActionEncoderIsOptional pins that the capability is opt-in
// and has NO default: an adapter without it cannot be asked to perform
// a semantic action at all. There is deliberately no "fall back to
// Enter" helper — the removed `send --steering/--follow-up` default did
// that and silently mislabeled raw input as a semantic mode.
func TestAgentActionEncoderIsOptional(t *testing.T) {
	var unknown Adapter // nil: adapter name unknown to this build (e.g. newer peer)
	if _, ok := unknown.(AgentActionEncoder); ok {
		t.Error("nil adapter must not satisfy AgentActionEncoder")
	}
	var plain Adapter = &plainAdapter{}
	if _, ok := plain.(AgentActionEncoder); ok {
		t.Error("an adapter without the capability must not satisfy AgentActionEncoder")
	}
	var encoder Adapter = &fakeEncoder{}
	if _, ok := encoder.(AgentActionEncoder); !ok {
		t.Error("fakeEncoder should satisfy AgentActionEncoder")
	}
}

// plainAdapter is an adapter with no optional capabilities at all.
type plainAdapter struct{ Adapter }

// TestFakeEncoderRejection verifies an adapter's right to reject an
// action it cannot express (ok=false), which callers must surface as an
// error instead of sending bytes with the wrong meaning.
func TestFakeEncoderRejection(t *testing.T) {
	f := &fakeEncoder{}
	if seq, ok := f.EncodeAction(ActionSend); !ok || seq != "\r" {
		t.Errorf("EncodeAction(ActionSend) = %q, %v; want \\r, true", seq, ok)
	}
	for _, action := range []AgentAction{ActionSendAfterTurn, ActionInterrupt} {
		if seq, ok := f.EncodeAction(action); ok || seq != "" {
			t.Errorf("EncodeAction(%d) = %q, %v; want \"\", false", action, seq, ok)
		}
	}
	if got := f.ActionReadyTimeout(); got != 3*time.Second {
		t.Errorf("ActionReadyTimeout() = %v, want 3s", got)
	}
}
