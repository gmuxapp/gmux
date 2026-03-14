package adapters

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/cli/gmuxr/internal/adapter"
)

// --- Generic adapter tests ---

func TestGenericMatchAll(t *testing.T) {
	g := NewGeneric(0)
	if !g.Match([]string{"anything"}) {
		t.Fatal("generic should match any command")
	}
	if !g.Match([]string{}) {
		t.Fatal("generic should match empty command")
	}
}

func TestGenericName(t *testing.T) {
	g := NewGeneric(0)
	if g.Name() != "generic" {
		t.Fatalf("expected 'generic', got %q", g.Name())
	}
}

func TestGenericPreparePassthrough(t *testing.T) {
	g := NewGeneric(0)
	cmd, env := g.Prepare(adapter.PrepareContext{
		Command: []string{"echo", "hello"},
	})
	if len(cmd) != 2 || cmd[0] != "echo" || cmd[1] != "hello" {
		t.Fatalf("expected passthrough, got %v", cmd)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env, got %v", env)
	}
}

func TestGenericMonitorFirstOutput(t *testing.T) {
	g := NewGeneric(time.Second)
	status := g.Monitor([]byte("hello"))
	if status == nil {
		t.Fatal("first output should produce status")
	}
	if status.State != "active" {
		t.Fatalf("expected 'active', got %q", status.State)
	}
}

func TestGenericMonitorSubsequentOutput(t *testing.T) {
	g := NewGeneric(time.Second)
	g.Monitor([]byte("first"))
	status := g.Monitor([]byte("second"))
	if status != nil {
		t.Fatal("subsequent output should not produce status (no change)")
	}
}

func TestGenericCheckSilence(t *testing.T) {
	g := NewGeneric(10 * time.Millisecond)
	if s := g.CheckSilence(); s != nil {
		t.Fatal("no output yet, should return nil")
	}
	g.Monitor([]byte("hello"))
	if s := g.CheckSilence(); s != nil {
		t.Fatal("just produced output, should not be silent")
	}
	time.Sleep(20 * time.Millisecond)
	status := g.CheckSilence()
	if status == nil {
		t.Fatal("should detect silence")
	}
	if status.State != "paused" {
		t.Fatalf("expected 'paused', got %q", status.State)
	}
	status = g.Monitor([]byte("more"))
	if status == nil || status.State != "active" {
		t.Fatal("output after silence should produce 'active'")
	}
}

// --- Pi adapter tests ---

func TestPiName(t *testing.T) {
	p := NewPi()
	if p.Name() != "pi" {
		t.Fatalf("expected 'pi', got %q", p.Name())
	}
}

func TestPiMatchDirect(t *testing.T) {
	p := NewPi()
	if !p.Match([]string{"pi"}) {
		t.Fatal("should match 'pi'")
	}
	if !p.Match([]string{"pi-coding-agent"}) {
		t.Fatal("should match 'pi-coding-agent'")
	}
}

func TestPiMatchWrapped(t *testing.T) {
	p := NewPi()
	if !p.Match([]string{"npx", "pi"}) {
		t.Fatal("should match 'npx pi'")
	}
	if !p.Match([]string{"env", "pi", "--flag"}) {
		t.Fatal("should match 'env pi --flag'")
	}
	if !p.Match([]string{"/home/user/.local/bin/pi"}) {
		t.Fatal("should match full path")
	}
}

func TestPiMatchStopsAtDoubleDash(t *testing.T) {
	p := NewPi()
	if p.Match([]string{"echo", "--", "pi"}) {
		t.Fatal("should not match 'pi' after '--'")
	}
}

func TestPiNoMatchOther(t *testing.T) {
	p := NewPi()
	if p.Match([]string{"pytest", "tests/"}) {
		t.Fatal("should not match pytest")
	}
	if p.Match([]string{"pipeline"}) {
		t.Fatal("should not match 'pipeline' (contains 'pi' but base name is 'pipeline')")
	}
}

func TestPiPreparePassthrough(t *testing.T) {
	p := NewPi()
	cmd, env := p.Prepare(adapter.PrepareContext{
		Command:   []string{"pi"},
		SessionID: "sess-test",
	})
	if len(cmd) != 1 || cmd[0] != "pi" {
		t.Fatalf("expected passthrough, got %v", cmd)
	}
	if len(env) != 0 {
		t.Fatalf("expected no extra env, got %v", env)
	}
}

func TestPiMonitorReturnsNil(t *testing.T) {
	p := NewPi()
	// v1: pi adapter doesn't parse output yet
	if s := p.Monitor([]byte("some output")); s != nil {
		t.Fatal("v1 pi adapter should not parse output")
	}
}
