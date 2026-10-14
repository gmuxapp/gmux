//go:build linux || darwin

package tsdiag

import (
	"context"
	"io"
	"os/exec"
	"syscall"
)

// execTailscaleStatus runs `tailscale status --json` under a hard wall-clock
// budget.
//
// The budget is enforced against the WHOLE process tree, not just the direct
// child: `tailscale` is routinely a wrapper shim (`sudo tailscale "$@"`, a
// Nix/Guix-style non-exec wrapper, a shell shim that logs first), and a shim
// that forks leaves the pipe's write end open in the grandchild. Killing only
// the direct child then leaves the output read blocked for the grandchild's
// lifetime — measured at 33s for a documented 3s probe — which stalls the
// health handler, the browser's first world render, and the snapshot composer.
//
// Three pieces make the deadline real:
//   - Setpgid: the child leads its own process group, so the group can be
//     signalled as a unit;
//   - Cancel: on deadline, SIGKILL the whole group (negated pid), reaching any
//     forked grandchildren that inherited the pipes;
//   - WaitDelay: after the kill, stop waiting for the pipe-copy goroutines on
//     a short grace, so a grandchild in another group cannot hold the read.
//
// Output is read through a capped writer: CombinedOutput buffers without
// bound, letting a hostile/shrinking `tailscale` balloon the daemon's heap.
func execTailscaleStatus(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "status", "--json")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the process group; the child is its own leader (Setpgid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = probeGrace

	var buf cappedBuffer
	// Same writer for both streams: exec serialises writes to it, which is how
	// CombinedOutput gets its ordering — and we need the bounded writer, which
	// CombinedOutput cannot give us. Assigning nothing would silently send the
	// child's output to the void.
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if buf.truncated {
		// Keep the exec error (a truncated netmap is not a tailscaled
		// verdict); the classification layer maps this to probe_failed.
		return nil, errProbeOutputCapped
	}
	return buf.Bytes(), err
}

// cappedBuffer collects subprocess output up to maxProbeOutput, then discards
// the remainder. io.Writer must not report a short write (exec treats it as a
// write error and kills the child), so overflow is swallowed with a nil error.
type cappedBuffer struct {
	buf       []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= maxProbeOutput {
		b.truncated = true
		return len(p), nil
	}
	room := maxProbeOutput - len(b.buf)
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated = true
	} else {
		b.buf = append(b.buf, p...)
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf }

// compile-time assertion: the capped writer satisfies io.Writer.
var _ io.Writer = (*cappedBuffer)(nil)
