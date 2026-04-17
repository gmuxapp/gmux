package main

import (
	"reflect"
	"testing"
)

// TestParseCLI covers every distinct dispatch path the user can reach.
// We check the returned mode, the relevant flag values, and the
// positional remainder — these three together describe what main.go
// will actually do.
func TestParseCLI(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantMode mode
		wantRest []string
		check    func(t *testing.T, f *flags)
	}{
		// ── Run-mode shapes ─────────────────────────────────────────
		{
			name:     "bare gmux opens the UI",
			args:     nil,
			wantMode: modeUI,
		},
		{
			name:     "plain command is a run",
			args:     []string{"pytest"},
			wantMode: modeRun,
			wantRest: []string{"pytest"},
		},
		{
			name:     "command keeps its own flags",
			args:     []string{"pytest", "--watch", "-x"},
			wantMode: modeRun,
			wantRest: []string{"pytest", "--watch", "-x"},
		},
		{
			name:     "double-dash shields a dash-prefixed command",
			args:     []string{"--", "--weird-binary", "arg"},
			wantMode: modeRun,
			wantRest: []string{"--weird-binary", "arg"},
		},
		{
			name:     "no-attach carries into run mode",
			args:     []string{"--no-attach", "pytest", "--watch"},
			wantMode: modeRun,
			wantRest: []string{"pytest", "--watch"},
			check: func(t *testing.T, f *flags) {
				if !f.noAttach {
					t.Error("noAttach should be true")
				}
			},
		},

		// ── Management shapes ───────────────────────────────────────
		{
			name:     "--list has no positionals",
			args:     []string{"--list"},
			wantMode: modeList,
		},
		{
			name:     "-l is the short form of --list",
			args:     []string{"-l"},
			wantMode: modeList,
		},
		{
			name:     "--attach takes one session id",
			args:     []string{"--attach", "sess-abcd"},
			wantMode: modeAttach,
			wantRest: []string{"sess-abcd"},
		},
		{
			name:     "--kill takes one session id",
			args:     []string{"--kill", "sess-abcd"},
			wantMode: modeKill,
			wantRest: []string{"sess-abcd"},
		},
		{
			name:     "--tail takes a count and a session id",
			args:     []string{"--tail", "100", "sess-abcd"},
			wantMode: modeTail,
			wantRest: []string{"sess-abcd"},
			check: func(t *testing.T, f *flags) {
				if f.tail != 100 {
					t.Errorf("tail = %d, want 100", f.tail)
				}
			},
		},
		{
			name:     "--send with inline text",
			args:     []string{"--send", "sess-abcd", "hello"},
			wantMode: modeSend,
			wantRest: []string{"sess-abcd", "hello"},
		},
		{
			name:     "--send without text reads stdin",
			args:     []string{"--send", "sess-abcd"},
			wantMode: modeSend,
			wantRest: []string{"sess-abcd"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, f, rest, err := parseCLI(tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m != tc.wantMode {
				t.Errorf("mode = %v, want %v", m, tc.wantMode)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				// Treat nil and empty slice as equivalent for readability.
				if !(len(rest) == 0 && len(tc.wantRest) == 0) {
					t.Errorf("rest = %v, want %v", rest, tc.wantRest)
				}
			}
			if tc.check != nil && f != nil {
				tc.check(t, f)
			}
		})
	}
}

// TestParseCLIErrors asserts that every user-facing invariant the CLI
// promises is actually enforced. We don't pin error strings (those are
// free to improve), only that an error is returned in each case.
func TestParseCLIErrors(t *testing.T) {
	invalid := [][]string{
		{"--list", "extra"},                      // --list takes no args
		{"--attach"},                             // --attach needs an id
		{"--attach", "a", "b"},                   // --attach takes exactly one id
		{"--kill"},                               // --kill needs an id
		{"--tail", "100"},                        // --tail needs an id
		{"--tail", "0", "sess-a"},                // --tail needs a positive count
		{"--send"},                               // --send needs an id
		{"--send", "a", "b", "c"},                // --send takes at most two args
		{"--list", "--attach", "sess-a"},         // mutually exclusive actions
		{"--kill", "--attach", "sess-a"},         // mutually exclusive actions
		{"--send", "--kill", "sess-a"},           // mutually exclusive actions
		{"--no-attach"},                          // --no-attach needs a command
		{"--no-attach", "--list"},                // --no-attach doesn't make sense with --list
		{"--no-attach", "--attach", "sess-a"},
		{"--no-attach", "--send", "sess-a", "x"}, // --no-attach doesn't make sense with --send
	}

	for _, args := range invalid {
		t.Run(joinArgs(args), func(t *testing.T) {
			if _, _, _, err := parseCLI(args); err == nil {
				t.Errorf("expected error for %v", args)
			}
		})
	}
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	if out == "" {
		return "(no args)"
	}
	return out
}
