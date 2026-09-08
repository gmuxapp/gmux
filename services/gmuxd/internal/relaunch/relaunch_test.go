package relaunch

import (
	"reflect"
	"testing"
)

func TestResolve(t *testing.T) {
	resume := func(adapterName, ref string) []string {
		if ref == "good" {
			return []string{adapterName, "--resume", ref}
		}
		return nil
	}

	for _, tc := range []struct {
		name     string
		ref      string
		command  []string
		resume   func(string, string) []string
		wantCmd  []string
		wantKind Kind
	}{
		{
			name: "conversation resolves to a resume command",
			ref:  "good", command: []string{"pi"}, resume: resume,
			wantCmd: []string{"pi", "--resume", "good"}, wantKind: Resume,
		},
		{
			// The user asked to continue *that* conversation; silently
			// starting a fresh one would be a different session.
			name: "unresolvable conversation is refused, not downgraded to rerun",
			ref:  "gone", command: []string{"pi"}, resume: resume,
			wantCmd: nil, wantKind: None,
		},
		{
			// The reported bug: dead shells have a command but never a
			// conversation ref, and presentation used to call this
			// "resumable" while execution refused it.
			name: "no conversation, recorded command reruns",
			ref:  "", command: []string{"/root/.local/bin/fish"}, resume: resume,
			wantCmd: []string{"/root/.local/bin/fish"}, wantKind: Rerun,
		},
		{
			name: "agent that died before binding a conversation reruns",
			ref:  "", command: []string{"claude"}, resume: resume,
			wantCmd: []string{"claude"}, wantKind: Rerun,
		},
		{
			name: "nothing recorded at all",
			ref:  "", command: nil, resume: resume,
			wantCmd: nil, wantKind: None,
		},
		{
			name: "nil resolver has no resume policy and degrades to rerun",
			ref:  "good", command: []string{"pi"}, resume: nil,
			wantCmd: []string{"pi"}, wantKind: Rerun,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, kind := Resolve(tc.resume, "pi", tc.ref, tc.command)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if !reflect.DeepEqual(cmd, tc.wantCmd) {
				t.Errorf("cmd = %v, want %v", cmd, tc.wantCmd)
			}
			if (len(cmd) > 0) != (kind != None) {
				t.Errorf("command/kind disagree: %v / %q", cmd, kind)
			}
		})
	}
}

// TestResolveDoesNotAliasCommand guards the rerun path against handing the
// caller the durable slice: the spawner rewrites what it gets.
func TestResolveDoesNotAliasCommand(t *testing.T) {
	durable := []string{"bash", "-lc", "true"}
	cmd, kind := Resolve(nil, "shell", "", durable)
	if kind != Rerun {
		t.Fatalf("kind = %q, want rerun", kind)
	}
	cmd[0] = "mutated"
	if durable[0] != "bash" {
		t.Errorf("durable command aliased and mutated: %v", durable)
	}
}
