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
	// Best-effort process-group sweep, on EVERY return path.
	//
	// Cancel is the only place the group is killed, and Go does not guarantee
	// it runs. When the DIRECT child exits before the deadline (the fork-and-exit
	// shim: it forks a grandchild to hold the pipe, then exits itself), exec's
	// watchCtx sees the process already gone, returns through its fast path, and
	// NEVER calls Cancel — "If pipes are closed due to WaitDelay, no Cancel call
	// has occurred" (exec.go). WaitDelay then closes the parent's pipes to
	// unblock the read, which unblocks Run, but the SIGKILL that reaches the
	// group never fires: the grandchild that inherited the pipe's write end is
	// reparented to init (ppid=1) and leaks for its lifetime. The probe's own
	// wall-clock budget stays bounded (Run returns at the grace), so this is
	// purely a process leak — a few per wedged host, accumulating.
	//
	// Sweep the group ourselves. The direct child is its group's leader
	// (Setpgid), so -pid targets the tree it left behind; the group id outlives
	// the leader, so it reaches grandchildren that outlived the shim. ESRCH (no
	// such group) is the healthy case — the group is already empty after a normal
	// probe, and after Cancel's kill on the deadline path — so the error is
	// ignored deliberately. cmd.Process is nil only if Start itself failed, in
	// which case no group was ever created.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
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
