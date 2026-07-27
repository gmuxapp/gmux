package main

import (
	"strings"
	"testing"
)

// TestShortFlagAliases pins every short alias against its long spelling:
// both must parse to the identical command. The aliases are only worth
// having if they are exact synonyms, and the help pages advertise them as
// such (--long/-short).
func TestShortFlagAliases(t *testing.T) {
	tests := []struct {
		name  string
		long  []string
		short []string
		check func(*testing.T, *command)
	}{
		{"ls --all", []string{"ls", "--all"}, []string{"ls", "-a"},
			func(t *testing.T, c *command) {
				if !c.all || c.json {
					t.Errorf("all=%v json=%v", c.all, c.json)
				}
			}},
		{"ls --json", []string{"ls", "--json"}, []string{"ls", "-j"},
			func(t *testing.T, c *command) {
				if c.all || !c.json {
					t.Errorf("all=%v json=%v", c.all, c.json)
				}
			}},
		{"ls both", []string{"ls", "--all", "--json"}, []string{"ls", "-a", "-j"},
			func(t *testing.T, c *command) {
				if !c.all || !c.json {
					t.Errorf("all=%v json=%v", c.all, c.json)
				}
			}},
		{"wait --quiet", []string{"wait", "--quiet", "s1"}, []string{"wait", "-q", "s1"},
			func(t *testing.T, c *command) {
				if !c.quiet || c.ref != "s1" {
					t.Errorf("quiet=%v ref=%q", c.quiet, c.ref)
				}
			}},
		{"wait --timeout", []string{"wait", "--timeout", "30", "s1"}, []string{"wait", "-t", "30", "s1"},
			func(t *testing.T, c *command) {
				if c.timeout != 30 || c.ref != "s1" {
					t.Errorf("timeout=%d ref=%q", c.timeout, c.ref)
				}
			}},
		{"wait --timeout=N after ref", []string{"wait", "s1", "--timeout=30"}, []string{"wait", "s1", "-t=30"},
			func(t *testing.T, c *command) {
				if c.timeout != 30 || c.ref != "s1" {
					t.Errorf("timeout=%d ref=%q", c.timeout, c.ref)
				}
			}},
		{"send --wait", []string{"send", "--wait", "abc", "go", "Enter"}, []string{"send", "-w", "abc", "go", "Enter"},
			func(t *testing.T, c *command) {
				if !c.sendWait || c.ref != "abc" || c.sendText == nil || *c.sendText != "go" {
					t.Errorf("sendWait=%v ref=%q text=%v", c.sendWait, c.ref, c.sendText)
				}
			}},
		{"send --timeout", []string{"send", "--wait", "--timeout", "60", "abc"},
			[]string{"send", "-w", "-t", "60", "abc"},
			func(t *testing.T, c *command) {
				if !c.sendWait || c.timeout != 60 {
					t.Errorf("sendWait=%v timeout=%d", c.sendWait, c.timeout)
				}
			}},
		{"send --timeout=N", []string{"send", "--wait", "--timeout=45", "abc"},
			[]string{"send", "-w", "-t=45", "abc"},
			func(t *testing.T, c *command) {
				if c.timeout != 45 {
					t.Errorf("timeout=%d", c.timeout)
				}
			}},
		{"agent prompt --timeout", []string{"agent", "prompt", "--timeout", "12", "s1", "hi"},
			[]string{"agent", "prompt", "-t", "12", "s1", "hi"},
			func(t *testing.T, c *command) {
				if c.timeout != 12 || c.ref != "s1" || c.promptText == nil || *c.promptText != "hi" {
					t.Errorf("timeout=%d ref=%q text=%v", c.timeout, c.ref, c.promptText)
				}
			}},
		{"agent prompt --timeout=N", []string{"agent", "prompt", "--timeout=7", "s1", "hi"},
			[]string{"agent", "prompt", "-t=7", "s1", "hi"},
			func(t *testing.T, c *command) {
				if c.timeout != 7 {
					t.Errorf("timeout=%d", c.timeout)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, args := range [][]string{tt.long, tt.short} {
				c, err := parseCLI(args)
				if err != nil {
					t.Fatalf("parseCLI(%v): %v", args, err)
				}
				tt.check(t, c)
			}
		})
	}
}

// TestNoShortsForSemanticFlags pins the deliberate omissions: the flags
// that change WHAT happens stay explicit, so a one-letter typo can never
// silently pick one.
func TestNoShortsForSemanticFlags(t *testing.T) {
	for _, args := range [][]string{
		{"wait", "-f", "x", "s1"},
		{"agent", "prompt", "-n", "s1", "hi"},
		{"agent", "prompt", "-f", "s1", "hi"},
		{"agent", "prompt", "-s", "s1", "hi"},
	} {
		if _, err := parseCLI(args); err == nil {
			t.Errorf("parseCLI(%v) = nil error, want a rejected short flag", args)
		}
	}
}

// TestAgentPromptTimeoutSingleUseAcrossSpellings pins that single-use is a
// property of the flag, not of a spelling: two spellings of --timeout are
// still the same flag twice, and the error names the canonical form.
func TestAgentPromptTimeoutSingleUseAcrossSpellings(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "prompt", "--timeout=5", "-t", "9", "s1", "x"},
		{"agent", "prompt", "-t", "5", "--timeout", "9", "s1", "x"},
		{"agent", "prompt", "-t", "5", "-t=9", "s1", "x"},
	} {
		_, err := parseCLI(args)
		if err == nil || !strings.Contains(err.Error(), "--timeout given more than once") {
			t.Errorf("parseCLI(%v) = %v, want '--timeout given more than once'", args, err)
		}
	}
	// -t is still bound by the same --no-wait rule.
	if _, err := parseCLI([]string{"agent", "prompt", "--no-wait", "-t", "5", "s1", "x"}); err == nil ||
		!strings.Contains(err.Error(), "cannot be combined with --no-wait") {
		t.Errorf("agent prompt --no-wait -t 5: %v, want the --no-wait refusal", err)
	}
}

// TestPerVerbShortsDoNotCollideWithRemovedFlags pins the boundary between
// the per-verb shorts and the top-level migration shim: the shim only ever
// matches in head position, so `gmux ls -a` is --all while a bare `gmux -a`
// still reports the removed flag it used to be.
func TestPerVerbShortsDoNotCollideWithRemovedFlags(t *testing.T) {
	c, err := parseCLI([]string{"ls", "-a"})
	if err != nil || c.mode != modeList || !c.all {
		t.Fatalf("parseCLI(ls -a) = (%+v, %v), want modeList with all=true", c, err)
	}
	if _, err := parseCLI([]string{"-a"}); err == nil || !strings.Contains(err.Error(), "gmux attach") {
		t.Errorf("parseCLI(-a) = %v, want the removed-flag guidance for -a", err)
	}
	if _, err := parseCLI([]string{"-t"}); err == nil || !strings.Contains(err.Error(), "gmux tail") {
		t.Errorf("parseCLI(-t) = %v, want the removed-flag guidance for -t", err)
	}
	// The same short in verb position keeps its verb meaning.
	c, err = parseCLI([]string{"wait", "-t", "5", "s1"})
	if err != nil || c.timeout != 5 {
		t.Fatalf("parseCLI(wait -t 5 s1) = (%+v, %v)", c, err)
	}
}

// TestHelpPagesShowShortAliases pins the presentation rule: wherever a
// short alias exists, the page spells the pair --long/-short, so nobody
// has to discover the alias elsewhere. send-keys' -t is the tmux TARGET
// flag and must not be advertised as a timeout.
func TestHelpPagesShowShortAliases(t *testing.T) {
	pairs := map[string][]string{
		"ls":   {"--all/-a", "--json/-j"},
		"wait": {"--timeout/-t", "--quiet/-q"},
		"send": {"--wait/-w", "--timeout/-t"},
	}
	for topic, want := range pairs {
		page := verbHelpPages[topic]
		for _, w := range want {
			if !strings.Contains(page, w) {
				t.Errorf("%s help page does not document %q", topic, w)
			}
		}
	}
	// send-keys' -t is tmux's TARGET flag. The page may (and now does) name
	// send's --timeout to disambiguate the collision, but it must never
	// advertise -t as a timeout of its own.
	skPage := verbHelpPages["send-keys"]
	if !strings.Contains(skPage, "-t <id>    target session") {
		t.Errorf("send-keys page must document -t as the target flag:\n%s", skPage)
	}
	if strings.Contains(skPage, "--timeout/-t") {
		t.Errorf("send-keys page must not present -t as a timeout alias:\n%s", skPage)
	}
	// Both sides of the collision are explained where the caller meets it.
	if !strings.Contains(verbHelpPages["send"], "send-keys") {
		t.Error("send page must point at send-keys for the tmux '-t <id>' target")
	}
}
