package main

import (
	"strings"
	"testing"
)

// TestParseCLI exercises the verb-first grammar (ADR 0009): each verb,
// the explicit run form, and the daemon-internal forms.
func TestParseCLI(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantMode mode
		check    func(t *testing.T, c *command)
	}{
		{name: "no args prints help", args: nil, wantMode: modeHelp},
		{name: "help verb", args: []string{"help"}, wantMode: modeHelp},
		{name: "help topic", args: []string{"help", "send"}, wantMode: modeHelp,
			check: func(t *testing.T, c *command) {
				if c.helpTopic != "send" {
					t.Errorf("helpTopic = %q, want send", c.helpTopic)
				}
			}},
		{name: "version", args: []string{"version"}, wantMode: modeVersion},
		{name: "open", args: []string{"open"}, wantMode: modeOpen},

		{name: "run via --", args: []string{"--", "pytest", "-q"}, wantMode: modeRun,
			check: func(t *testing.T, c *command) {
				if strings.Join(c.runArgs, " ") != "pytest -q" {
					t.Errorf("runArgs = %v", c.runArgs)
				}
				if c.detach {
					t.Error("detach should be false")
				}
			}},
		{name: "detached run", args: []string{"-d", "--", "server"}, wantMode: modeRun,
			check: func(t *testing.T, c *command) {
				if !c.detach {
					t.Error("detach should be true")
				}
				if strings.Join(c.runArgs, " ") != "server" {
					t.Errorf("runArgs = %v", c.runArgs)
				}
			}},
		{name: "detach long form", args: []string{"--detach", "--", "x"}, wantMode: modeRun,
			check: func(t *testing.T, c *command) {
				if !c.detach {
					t.Error("detach should be true")
				}
			}},
		{name: "child flags after -- are not gmux flags", args: []string{"--", "pi", "--all", "prompt"}, wantMode: modeRun,
			check: func(t *testing.T, c *command) {
				if strings.Join(c.runArgs, " ") != "pi --all prompt" {
					t.Errorf("runArgs = %v, child flags must pass through", c.runArgs)
				}
			}},

		{name: "ls", args: []string{"ls"}, wantMode: modeList},
		{name: "ls --all --json", args: []string{"ls", "--all", "--json"}, wantMode: modeList,
			check: func(t *testing.T, c *command) {
				if !c.all || !c.json {
					t.Errorf("all=%v json=%v, want both true", c.all, c.json)
				}
			}},

		{name: "attach", args: []string{"attach", "abc"}, wantMode: modeAttach,
			check: func(t *testing.T, c *command) {
				if c.ref != "abc" {
					t.Errorf("ref = %q", c.ref)
				}
			}},
		{name: "kill with peer ref", args: []string{"kill", "abc@laptop"}, wantMode: modeKill,
			check: func(t *testing.T, c *command) {
				if c.ref != "abc@laptop" {
					t.Errorf("ref = %q", c.ref)
				}
			}},

		{name: "tail defaults to 100 lines", args: []string{"tail", "abc"}, wantMode: modeTail,
			check: func(t *testing.T, c *command) {
				if c.tailLines != 100 || c.raw {
					t.Errorf("tailLines=%d raw=%v", c.tailLines, c.raw)
				}
			}},
		{name: "tail -n and --raw", args: []string{"tail", "-n", "500", "--raw", "abc"}, wantMode: modeTail,
			check: func(t *testing.T, c *command) {
				if c.tailLines != 500 || !c.raw {
					t.Errorf("tailLines=%d raw=%v", c.tailLines, c.raw)
				}
			}},

		{name: "send text + Enter", args: []string{"send", "abc", "pytest -q", "Enter"}, wantMode: modeSend,
			check: func(t *testing.T, c *command) {
				if c.ref != "abc" || c.sendText == nil || *c.sendText != "pytest -q" {
					t.Errorf("ref=%q text=%v", c.ref, c.sendText)
				}
				if len(c.sendKeys) != 1 || c.sendKeys[0] != "Enter" {
					t.Errorf("keys = %v", c.sendKeys)
				}
			}},
		{name: "send keys only", args: []string{"send", "abc", "C-c"}, wantMode: modeSend,
			check: func(t *testing.T, c *command) {
				if c.sendText != nil {
					t.Errorf("text should be nil, got %v", *c.sendText)
				}
				if len(c.sendKeys) != 1 || c.sendKeys[0] != "C-c" {
					t.Errorf("keys = %v", c.sendKeys)
				}
			}},
		{name: "send stdin (ref only)", args: []string{"send", "abc"}, wantMode: modeSend,
			check: func(t *testing.T, c *command) {
				if c.sendText != nil || len(c.sendKeys) != 0 {
					t.Errorf("expected stdin form: text=%v keys=%v", c.sendText, c.sendKeys)
				}
			}},

		{name: "send-keys tmux compat", args: []string{"send-keys", "-t", "abc", "C-c"}, wantMode: modeSendKeys,
			check: func(t *testing.T, c *command) {
				if c.ref != "abc" || len(c.keys) != 1 || c.keys[0] != "C-c" {
					t.Errorf("ref=%q keys=%v", c.ref, c.keys)
				}
			}},
		{name: "send-keys literal", args: []string{"send-keys", "-t", "abc", "-l", "hello"}, wantMode: modeSendKeys,
			check: func(t *testing.T, c *command) {
				if !c.keysLiteral {
					t.Error("keysLiteral should be true")
				}
			}},

		{name: "wait idle default", args: []string{"wait", "abc"}, wantMode: modeWait},
		{name: "wait --for-text --timeout", args: []string{"wait", "--for-text", "DONE", "--timeout", "30", "abc"}, wantMode: modeWait,
			check: func(t *testing.T, c *command) {
				if c.forText != "DONE" || c.timeout != 30 {
					t.Errorf("forText=%q timeout=%d", c.forText, c.timeout)
				}
			}},
		{name: "wait flags after positional", args: []string{"wait", "abc", "--for-regex", "^>>>"}, wantMode: modeWait,
			check: func(t *testing.T, c *command) {
				if c.forRegex != "^>>>" || c.ref != "abc" {
					t.Errorf("forRegex=%q ref=%q", c.forRegex, c.ref)
				}
			}},

		{name: "daemon status", args: []string{"daemon", "status"}, wantMode: modeDaemon,
			check: func(t *testing.T, c *command) {
				if c.daemonSub != "status" {
					t.Errorf("daemonSub = %q", c.daemonSub)
				}
			}},
		{name: "auth", args: []string{"auth"}, wantMode: modeAuth},
		{name: "remote", args: []string{"remote"}, wantMode: modeRemote},

		{name: "internal __run with directives", args: []string{"__run", "--resume-id=sess-1", "--initial-cols=80", "--", "pi"}, wantMode: modeRun,
			check: func(t *testing.T, c *command) {
				if c.resumeID != "sess-1" || c.initialCols != 80 {
					t.Errorf("resumeID=%q cols=%d", c.resumeID, c.initialCols)
				}
				if strings.Join(c.runArgs, " ") != "pi" {
					t.Errorf("runArgs = %v", c.runArgs)
				}
			}},
		{name: "internal __dump-env", args: []string{"__dump-env"}, wantMode: modeDumpEnv},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseCLI(tc.args)
			if err != nil {
				t.Fatalf("parseCLI(%v) unexpected error: %v", tc.args, err)
			}
			if c.mode != tc.wantMode {
				t.Fatalf("mode = %v, want %v", c.mode, tc.wantMode)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

func TestParseCLIErrors(t *testing.T) {
	bad := [][]string{
		{"-d"},                          // detach without command
		{"-d", "ls"},                    // detach only pairs with --
		{"--"},                          // run with no command
		{"open", "extra"},               // open takes no args
		{"attach"},                      // missing id
		{"attach", "a", "b"},            // too many
		{"tail"},                        // missing id
		{"tail", "-n", "0", "abc"},      // non-positive count
		{"wait"},                        // missing id
		{"wait", "--for-text", "x", "--for-regex", "y", "abc"}, // mutually exclusive
		{"send-keys", "C-c"},            // missing -t
		{"daemon"},                      // missing subcommand
		{"daemon", "frobnicate"},        // unknown subcommand
		{"ls", "stray"},                 // ls takes no positional
	}
	for _, args := range bad {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			if _, err := parseCLI(args); err == nil {
				t.Errorf("parseCLI(%v) = nil error, want error", args)
			}
		})
	}
}

// TestParseCLIMigrationShim checks that removed pre-2.0 forms and the
// dropped bare-command shorthand produce guidance errors, not silent
// behavior (ADR 0009 error-only shim).
func TestParseCLIMigrationShim(t *testing.T) {
	cases := []struct {
		args     []string
		contains string
	}{
		{[]string{"--list"}, "gmux ls"},
		{[]string{"-l"}, "gmux ls"},
		{[]string{"--kill", "abc"}, "gmux kill"},
		{[]string{"--no-attach", "x"}, "gmux -d"},
		{[]string{"--host=laptop"}, "@<peer>"},
		{[]string{"pytest", "-q"}, "gmux -- pytest"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			_, err := parseCLI(tc.args)
			if err == nil {
				t.Fatalf("parseCLI(%v) = nil error, want migration error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestDidYouMean(t *testing.T) {
	if got := didYouMean("opn"); got != "open" {
		t.Errorf("didYouMean(opn) = %q, want open", got)
	}
	if got := didYouMean("klil"); got != "" { // two edits away
		t.Errorf("didYouMean(klil) = %q, want empty", got)
	}
}
