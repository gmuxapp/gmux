package agentext

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const subprocessHelperEnv = "GMUX_AGENTEXT_SUBPROCESS_HELPER"

func TestMaterializePathConcurrentProcesses(t *testing.T) {
	testMaterializePathConcurrentProcesses(t, nil)
}

func TestMaterializePathConcurrentProcessesRepairInvalidRegular(t *testing.T) {
	testMaterializePathConcurrentProcesses(t, []byte("truncated\n"))
}

func testMaterializePathConcurrentProcesses(t *testing.T, incumbent []byte) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "pi-ext-content.mjs")
	source := bytes.Repeat([]byte("export default 'shared';\n"), 1024)
	if incumbent != nil {
		if err := os.WriteFile(p, incumbent, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The pre-fix implementation used this shared scratch pathname. Keeping an
	// unrelated entry there proves independent processes cannot share it.
	if err := os.Mkdir(p+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	const callers = 12
	gate := filepath.Join(dir, "start")
	type process struct {
		cmd *exec.Cmd
		out bytes.Buffer
	}
	processes := make([]process, callers)
	for i := range processes {
		ready := filepath.Join(dir, fmt.Sprintf("ready-%02d", i))
		cmd := exec.Command(os.Args[0], "-test.run=^TestMaterializePathSubprocessHelper$")
		cmd.Env = append(os.Environ(),
			subprocessHelperEnv+"=1",
			"GMUX_AGENTEXT_PATH="+p,
			"GMUX_AGENTEXT_SOURCE="+hex.EncodeToString(source),
			"GMUX_AGENTEXT_READY="+ready,
			"GMUX_AGENTEXT_GATE="+gate,
		)
		cmd.Stdout = &processes[i].out
		cmd.Stderr = &processes[i].out
		processes[i].cmd = cmd
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for i := range processes {
		ready := filepath.Join(dir, fmt.Sprintf("ready-%02d", i))
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("helpers did not become ready; helper %d output: %s", i, processes[i].out.String())
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := processes[i].cmd.Wait(); err != nil {
				t.Errorf("helper %d: %v\n%s", i, err, processes[i].out.String())
			}
		}()
	}
	wg.Wait()

	if err := validateArtifact(p, source); err != nil {
		t.Fatal(err)
	}
	assertNoPrivateTemps(t, dir, p)
}

func TestMaterializePathRepairsInvalidRegular(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pi-ext-content.mjs")
	source := []byte("authoritative embedded source\n")
	if err := os.WriteFile(p, []byte("truncated\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if got, err := materializePath(p, source); err != nil || got != p {
		t.Fatalf("materializePath = %q, %v", got, err)
	}
	if err := validateArtifact(p, source); err != nil {
		t.Fatal(err)
	}
	assertNoPrivateTemps(t, dir, p)
}

func TestMaterializePathRejectsNonRegularFinalArtifact(t *testing.T) {
	t.Run("matching symlink", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "pi-ext-content.mjs")
		source := []byte("matching\n")
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, source, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if _, err := materializePath(p, source); err == nil {
			t.Fatal("matching final symlink was accepted")
		}
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("final symlink was replaced: %v, %v", info, err)
		}
		assertNoPrivateTemps(t, dir, p)
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "pi-ext-content.mjs")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := materializePath(p, []byte("source\n")); err == nil {
			t.Fatal("final directory was accepted")
		}
		assertNoPrivateTemps(t, dir, p)
	})
}

func TestMaterializePathAllowsSymlinkedCacheParent(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "relocated-cache")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "cache")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(linkedDir, "pi-ext-content.mjs")
	source := []byte("source\n")
	if got, err := materializePath(p, source); err != nil || got != p {
		t.Fatalf("materialize through symlinked parent = %q, %v", got, err)
	}
	if err := validateArtifact(p, source); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializePathPreservesRestrictiveUmask(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pi-ext-content.mjs")
	source := []byte("private\n")
	cmd := exec.Command("sh", "-c", `umask 077; exec "$1" -test.run='^TestMaterializePathSubprocessHelper$'`, "sh", os.Args[0])
	cmd.Env = append(os.Environ(),
		subprocessHelperEnv+"=1",
		"GMUX_AGENTEXT_PATH="+p,
		"GMUX_AGENTEXT_SOURCE="+hex.EncodeToString(source),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restricted-umask helper: %v\n%s", err, out)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("published mode = %04o, want 0600", got)
	}
}

func TestMaterializePathSubprocessHelper(t *testing.T) {
	if os.Getenv(subprocessHelperEnv) != "1" {
		return
	}
	source, err := hex.DecodeString(os.Getenv("GMUX_AGENTEXT_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	if ready := os.Getenv("GMUX_AGENTEXT_READY"); ready != "" {
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if gate := os.Getenv("GMUX_AGENTEXT_GATE"); gate != "" {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for start gate")
			}
			time.Sleep(time.Millisecond)
		}
	}
	p := os.Getenv("GMUX_AGENTEXT_PATH")
	got, err := materializePath(p, source)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("materializePath returned %q, want %q", got, p)
	}
}

func assertNoPrivateTemps(t *testing.T, dir, p string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "."+filepath.Base(p)+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("private temporary files leaked: %v", matches)
	}
}

func TestPathMaterializesReadableExtension(t *testing.T) {
	// Path intentionally memoizes for the life of a production process. Give
	// this test invocation a private memoization cell because -count repeats
	// tests in one process after the prior invocation's TempDir is removed.
	// No tests in this package may run in parallel while swapping this global.
	oldOnce, oldPath, oldLoadErr := once, path, loadErr
	once, path, loadErr = new(sync.Once), "", nil
	t.Cleanup(func() {
		once, path, loadErr = oldOnce, oldPath, oldLoadErr
	})

	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if !strings.HasSuffix(p, ".mjs") {
		t.Errorf("expected .mjs path, got %q", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read materialized ext: %v", err)
	}
	if !strings.Contains(string(data), "session_start") {
		t.Error("materialized extension missing session_start handler")
	}

	// Idempotent: a second call returns the same path.
	p2, err := Path()
	if err != nil || p2 != p {
		t.Errorf("Path not idempotent: %q/%v vs %q", p2, err, p)
	}
}
