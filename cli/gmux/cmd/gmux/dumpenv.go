package main

import (
	"io"
	"log"
	"os"
)

// dumpEnvFD is the file descriptor gmuxd hands to the `gmux __dump-env`
// probe (via cmd.ExtraFiles[0], which the child sees as fd 3). Writing
// the environment to a dedicated fd — rather than stdout — keeps the
// payload clean of any banner/prompt noise a login shell's rc files
// print to stdout/stderr. See ADR 0006.
const dumpEnvFD = 3

// dumpEnv writes os.Environ() to fd 3 as NUL-terminated entries, then
// exits. It is the inner command of gmuxd's env-capture probe
//
//	$SHELL -l -i -c '<gmux> __dump-env'
//
// The login shell sources the user's dotfiles, so the environment we
// observe here is the freshly-sourced one gmuxd wants to launch the
// session with. NUL termination is used (not newlines) because exported
// shell functions in bash carry literal newlines in their values.
//
// On any write failure we return non-zero; gmuxd treats a short/empty
// read as a failed capture and falls back to its own environment, so a
// partial dump never silently corrupts a session's env.
func dumpEnv() int {
	f := os.NewFile(dumpEnvFD, "gmux-dump-env")
	if f == nil {
		log.Printf("__dump-env: fd %d not available", dumpEnvFD)
		return 1
	}
	defer f.Close()

	if err := writeNulEnv(f, os.Environ()); err != nil {
		log.Printf("__dump-env: write fd %d: %v", dumpEnvFD, err)
		return 1
	}
	return 0
}

// writeNulEnv writes each env entry to w followed by a NUL byte. Split
// out from dumpEnv so the wire format can be unit-tested without an
// actual fd 3.
func writeNulEnv(w io.Writer, env []string) error {
	buf := make([]byte, 0, 8192)
	for _, e := range env {
		buf = append(buf, e...)
		buf = append(buf, 0)
	}
	_, err := w.Write(buf)
	return err
}
