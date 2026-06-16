package agentshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func get(env []string, key string) (string, bool) {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return e[len(key)+1:], true
		}
	}
	return "", false
}

func TestPreloadEnvFreshVars(t *testing.T) {
	out := PreloadEnv([]string{"PATH=/bin"}, "/s/hook.mjs", "/run/sock")
	if v, _ := get(out, "NODE_OPTIONS"); v != "--import file:///s/hook.mjs" {
		t.Errorf("NODE_OPTIONS = %q", v)
	}
	if v, _ := get(out, "BUN_OPTIONS"); v != "--preload /s/hook.mjs" {
		t.Errorf("BUN_OPTIONS = %q", v)
	}
	if v, _ := get(out, "GMUX_RUNNER_SOCK"); v != "/run/sock" {
		t.Errorf("GMUX_RUNNER_SOCK = %q", v)
	}
}

func TestPreloadEnvAppendsToExisting(t *testing.T) {
	out := PreloadEnv([]string{
		"NODE_OPTIONS=--max-old-space-size=256",
		"BUN_OPTIONS=--smol",
	}, "/s/hook.mjs", "/run/sock")
	if v, _ := get(out, "NODE_OPTIONS"); v != "--max-old-space-size=256 --import file:///s/hook.mjs" {
		t.Errorf("NODE_OPTIONS not appended: %q", v)
	}
	if v, _ := get(out, "BUN_OPTIONS"); v != "--smol --preload /s/hook.mjs" {
		t.Errorf("BUN_OPTIONS not appended: %q", v)
	}
}

func TestPreloadEnvOverridesInheritedSock(t *testing.T) {
	out := PreloadEnv([]string{"GMUX_RUNNER_SOCK=/stale"}, "/s/hook.mjs", "/run/sock")
	n := 0
	for _, e := range out {
		if strings.HasPrefix(e, "GMUX_RUNNER_SOCK=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one GMUX_RUNNER_SOCK, got %d", n)
	}
	if v, _ := get(out, "GMUX_RUNNER_SOCK"); v != "/run/sock" {
		t.Errorf("stale sock not overridden: %q", v)
	}
}

func TestMaterializeReadableFile(t *testing.T) {
	// materialize() is the un-cached worker behind Path(); call it directly
	// so the sync.Once in Path() doesn't pin a path from another test run.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p, err := materialize()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, ".mjs") {
		t.Errorf("path not a .mjs: %s", p)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, hookSource) {
		t.Errorf("materialized content does not match embedded source")
	}
}

// TestHookStripsSelfPreloadContentAddressed is the regression guard for the
// strip bug: the shim must remove the --import/--preload flag that points at
// its own (content-addressed) path so spawned children inherit a clean env.
// Runs the real embedded hook under node against a hook-<hash>.mjs filename.
func TestHookStripsSelfPreloadContentAddressed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	// Materialize under the production-style content-addressed name.
	hook := filepath.Join(dir, "hook-deadbeef12ab.mjs")
	if err := os.WriteFile(hook, hookSource, 0o644); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "main.mjs")
	if err := os.WriteFile(main, []byte(`console.log("NODE_OPTIONS="+(process.env.NODE_OPTIONS??"(unset)"));`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, main)
	cmd.Env = append(os.Environ(),
		"GMUX_RUNNER_SOCK=/tmp/nonexistent-shim.sock",
		"NODE_OPTIONS=--max-old-space-size=256 --import file://"+hook,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node run: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if strings.Contains(got, "hook-deadbeef12ab.mjs") || strings.Contains(got, "--import") {
		t.Errorf("shim did not strip its own --import flag: %q", got)
	}
	if !strings.Contains(got, "--max-old-space-size=256") {
		t.Errorf("shim clobbered an unrelated upstream flag: %q", got)
	}
}
