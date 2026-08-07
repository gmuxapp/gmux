package main

import (
	"testing"
)

// TestRunnerEventProjectionMetaParsesRunnerWireShape pins the exact snake_case
// payload the runner emits (cli/gmux/internal/session/state.go emitMetaLocked
// and SetSlug). Regression: the projection struct had untagged fields, and
// Go's case-insensitive matching does not bridge "adapter_title" to
// AdapterTitle — every live title/slug update was silently dropped, so
// sessions started after a daemon restart never showed their titles.
func TestRunnerEventProjectionMetaParsesRunnerWireShape(t *testing.T) {
	raw := []byte(`{"title":"docs-audit","shell_title":"π - docs-audit - rest-reads","adapter_title":"docs-audit","subtitle":"sub","slug":"docs-audit"}`)
	ev, ok := runnerEventProjection("meta", raw)
	if !ok {
		t.Fatal("meta event rejected")
	}
	f := ev.Facts
	for name, got := range map[string]*string{
		"shell_title":   f.ShellTitle,
		"adapter_title": f.AdapterTitle,
		"subtitle":      f.Subtitle,
		"slug":          f.Slug,
	} {
		if got == nil {
			t.Errorf("%s not parsed from runner meta event", name)
		}
	}
	if f.AdapterTitle != nil && *f.AdapterTitle != "docs-audit" {
		t.Errorf("adapter_title = %q, want %q", *f.AdapterTitle, "docs-audit")
	}
	if f.ShellTitle != nil && *f.ShellTitle != "π - docs-audit - rest-reads" {
		t.Errorf("shell_title = %q", *f.ShellTitle)
	}

	// unread-only meta events (state.go SetUnread) keep working.
	ev, ok = runnerEventProjection("meta", []byte(`{"unread":true,"unread_token":"result-1"}`))
	if !ok || ev.Facts.Unread == nil || !*ev.Facts.Unread || ev.Facts.UnreadToken == nil || *ev.Facts.UnreadToken != "result-1" {
		t.Fatal("unread meta event/token not parsed")
	}
}

// TestRunnerEventProjectionStatusCarriesTheTurnFrame pins the coupled turn edge:
// the runner puts a turn's frame INSIDE its status event so a close and the
// result it asserted cannot be separated in transit
// (docs/runner-hook-protocol.md; cli/gmux/internal/session/turnframe.go
// turnEdge). Decoding the status fields but dropping the frame would resolve
// every wait result-free — "completed, exit 0, no answer", the phenotype this
// stack exists to kill — while every test that stamps frames by hand still
// passed, so the wire shape is pinned here.
func TestRunnerEventProjectionExitCarriesUnreadGeneration(t *testing.T) {
	ev, ok := runnerEventProjection("exit", []byte(`{"exit_code":7,"unread":true,"unread_token":"result-8"}`))
	if !ok || ev.Alive == nil || *ev.Alive || ev.Facts.Unread == nil || !*ev.Facts.Unread ||
		ev.Facts.UnreadToken == nil || *ev.Facts.UnreadToken != "result-8" {
		t.Fatalf("exit/unread facts not parsed atomically: %+v ok=%v", ev, ok)
	}
}

func TestRunnerEventProjectionStatusCarriesTheTurnFrame(t *testing.T) {
	raw := []byte(`{"active":false,"error":false,"interrupted":false,"unread":true,"unread_token":"result-7",` +
		`"turn_frame":{"seq":12,"last":{"turn_seq":7,"outcome":"completed","trigger":"old ask","output":"4","truncated":true}}}`)
	ev, ok := runnerEventProjection("status", raw)
	if !ok {
		t.Fatal("coupled status event rejected")
	}
	if ev.Facts.Active == nil || *ev.Facts.Active || ev.Facts.Unread == nil || !*ev.Facts.Unread ||
		ev.Facts.UnreadToken == nil || *ev.Facts.UnreadToken != "result-7" {
		t.Fatalf("status/unread facts not parsed atomically: %+v", ev.Facts)
	}
	if ev.FrameOnly {
		t.Fatal("a turn edge must be applied durably, not treated as frame-only")
	}
	closed := ev.Frame.ClosedTurn(7)
	if closed == nil || closed.Output != "4" || closed.Trigger != "old ask" || !closed.Truncated || len(closed.Exchanges) != 0 {
		t.Fatalf("the edge's frame did not survive projection: %+v", ev.Frame)
	}

	// A status write that is NOT a turn edge (a raw PUT /status, a shell
	// session's lifetime turn, a runner too old to send a frame) carries none —
	// the frame-less case that must still resolve, result-free.
	ev, ok = runnerEventProjection("status", []byte(`{"active":true}`))
	if !ok || ev.Frame != nil || ev.FrameOnly {
		t.Fatalf("frame-less status event = %+v", ev)
	}

	// A frame-only event (injection, rebind clear, replay snapshot) is retained
	// but must never produce a durable observation.
	ev, ok = runnerEventProjection("turn_frame", []byte(`{"seq":3,"current":{"turn_seq":9}}`))
	if !ok || !ev.FrameOnly || ev.Frame.CurrentTurnSeq() != 9 {
		t.Fatalf("frame-only event = %+v (frameOnly=%v)", ev.Frame, ev.FrameOnly)
	}
}
