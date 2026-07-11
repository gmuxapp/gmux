package adapter

import "testing"

// fakeSubmitter implements PromptSubmitter and supports only steering,
// exercising the reject path the capability allows for adapters that
// can't honor a mode.
type fakeSubmitter struct {
	Adapter // embed to satisfy the base interface without implementing it
}

func (f *fakeSubmitter) SubmitSeq(mode SubmitMode) (string, bool) {
	if mode == SubmitSteering {
		return "\r", true
	}
	return "", false
}

// TestSubmitSeqForDefault pins the fallback contract: adapters without
// the PromptSubmitter capability — and nil (adapter name unknown to
// this build) — resolve to Enter for both modes. This is what makes
// `gmux send --follow-up/--steering` work on every session out of the
// box: Enter is the single submit keystroke of every PTY application,
// and agents like claude/codex use it both to submit when idle and to
// queue when busy.
func TestSubmitSeqForDefault(t *testing.T) {
	for _, mode := range []SubmitMode{SubmitSteering, SubmitFollowUp} {
		if seq, ok := SubmitSeqFor(nil, mode); !ok || seq != "\r" {
			t.Errorf("SubmitSeqFor(nil, %d) = %q, %v; want \\r, true", mode, seq, ok)
		}
	}
}

// TestSubmitSeqForDelegates verifies the capability is consulted when
// present, including its right to reject a mode (ok=false), which the
// CLI turns into a usage error instead of sending wrong-meaning bytes.
func TestSubmitSeqForDelegates(t *testing.T) {
	f := &fakeSubmitter{}
	if seq, ok := SubmitSeqFor(f, SubmitSteering); !ok || seq != "\r" {
		t.Errorf("SubmitSeqFor(fake, steering) = %q, %v; want \\r, true", seq, ok)
	}
	if _, ok := SubmitSeqFor(f, SubmitFollowUp); ok {
		t.Error("SubmitSeqFor(fake, follow-up) should propagate the adapter's rejection")
	}
}
