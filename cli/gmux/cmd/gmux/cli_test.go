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
			check: func(t *testing.T, f *flags) {
				if f.noSubmit {
					t.Errorf("noSubmit = true, want false (submit-by-default)")
				}
			},
		},
		{
			name:     "--send --no-submit suppresses the submit",
			args:     []string{"--send", "--no-submit", "sess-abcd", "draft"},
			wantMode: modeSend,
			wantRest: []string{"sess-abcd", "draft"},
			check: func(t *testing.T, f *flags) {
				if !f.noSubmit {
					t.Errorf("noSubmit = false, want true")
				}
			},
		},
		{
			name:     "--wait takes a session id",
			args:     []string{"--wait", "sess-abcd"},
			wantMode: modeWait,
			wantRest: []string{"sess-abcd"},
			check: func(t *testing.T, f *flags) {
				if f.waitTimeout != 0 {
					t.Errorf("waitTimeout = %d, want 0", f.waitTimeout)
				}
			},
		},
		{
			name:     "--wait with --timeout",
			args:     []string{"--wait", "--timeout", "30", "sess-abcd"},
			wantMode: modeWait,
			wantRest: []string{"sess-abcd"},
			check: func(t *testing.T, f *flags) {
				if f.waitTimeout != 30 {
					t.Errorf("waitTimeout = %d, want 30", f.waitTimeout)
				}
			},
		},
		// Management modes accept flags in any order: there's no
		// wrapped child command at the end, so the POSIX runner
		// stop-at-first-positional rule that run mode needs would only
		// turn `gmux --wait <id> --timeout 60` into a silent foot-trap.
		// These cases pin the lenient parsing for each management
		// action that takes a flag besides its mode flag.
		{
			name:     "--wait accepts --timeout after the id",
			args:     []string{"--wait", "sess-abcd", "--timeout", "30"},
			wantMode: modeWait,
			wantRest: []string{"sess-abcd"},
			check: func(t *testing.T, f *flags) {
				if f.waitTimeout != 30 {
					t.Errorf("waitTimeout = %d, want 30", f.waitTimeout)
				}
			},
		},
		{
			name:     "--send accepts --no-submit after the id",
			args:     []string{"--send", "sess-abcd", "--no-submit", "text"},
			wantMode: modeSend,
			wantRest: []string{"sess-abcd", "text"},
			check: func(t *testing.T, f *flags) {
				if !f.noSubmit {
					t.Errorf("noSubmit = false, want true")
				}
			},
		},
		{
			name:     "--send accepts --no-submit after both positionals",
			args:     []string{"--send", "sess-abcd", "text", "--no-submit"},
			wantMode: modeSend,
			wantRest: []string{"sess-abcd", "text"},
			check: func(t *testing.T, f *flags) {
				if !f.noSubmit {
					t.Errorf("noSubmit = false, want true")
				}
			},
		},
		// `gmux --send <id> -- <text>` is the documented escape for
		// inline text that starts with dashes. The lenient flag parser
		// must respect `--` as a hard terminator, otherwise text like
		// `--no-submit` (a real gmux flag name) gets silently swallowed
		// as a flag and never reaches the agent. This is the case where
		// being too lenient would corrupt user data, so it gets a test
		// of its own rather than living inside the table.
		{
			name:     "--send respects -- as a flag terminator for inline text",
			args:     []string{"--send", "sess-abcd", "--", "--no-submit"},
			wantMode: modeSend,
			wantRest: []string{"sess-abcd", "--no-submit"},
			check: func(t *testing.T, f *flags) {
				if f.noSubmit {
					t.Errorf("noSubmit = true, want false: -- should have stopped flag parsing so --no-submit is text, not a flag")
				}
			},
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

// TestRunModeKeepsPOSIXRunnerSemantics pins the contract that flags
// after the wrapped command go to the child, not to gmux. This is
// the load-bearing case for run mode and the reason management modes
// (which lack a wrapped command) get lenient parsing while run mode
// keeps the strict stop-at-first-positional behavior.
func TestRunModeKeepsPOSIXRunnerSemantics(t *testing.T) {
	// `gmux pi --some-pi-flag` — --some-pi-flag must reach pi as part
	// of the command, not be parsed as an unknown gmux flag.
	_, _, rest, err := parseCLI([]string{"pi", "--some-pi-flag", "prompt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"pi", "--some-pi-flag", "prompt"}
	if !reflect.DeepEqual(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
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
		{"--no-submit", "sess-a"},                // --no-submit only applies with --send
		{"--no-submit", "--list"},                // --no-submit only applies with --send
		{"--wait"},                               // --wait needs an id
		{"--wait", "a", "b"},                     // --wait takes exactly one id
		{"--wait", "--send", "sess-a"},           // mutually exclusive
		{"--no-attach", "--wait", "sess-a"},      // --no-attach has no effect with --wait
		{"--timeout", "30", "sess-a"},            // --timeout only applies with --wait
		{"--wait", "--timeout", "-1", "sess-a"},  // --timeout must be non-negative
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
