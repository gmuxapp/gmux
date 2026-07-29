package main

import (
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// noticeInterruptedWait installs the one thing a ^C on a blocking wait owes its
// caller: the fact that only the WAIT stopped (ADR 0027's 2026-07-28 amendment).
//
// Without it, SIGINT on `gmux wait` or a synchronous `gmux agent prompt` reads
// like the agent was stopped — the session is still running, its turn is still
// running, and re-arming is one command away. The default disposition is kept
// (the process still dies on the signal, with the shell's usual semantics); this
// only prints the account first.
//
// Deliberately narrow:
//
//   - stdout report. A signal is a semantic wait outcome and uses the same
//     formatter as every other exchange report; stderr remains diagnostic-only.
//
//   - notice-then-die, not swallow-and-continue. A caller who pressed ^C wants
//     out; a wait that printed a notice and then kept waiting would be strictly
//     worse than the confusion this fixes — before the notice, ^C at least
//     exited. `$?` is the shell's 128+N, because gmux reached no verdict about
//     the turn at all: neither `1` nor `2` would be true of a turn that is still
//     running.
//
//     Dying is the hard part, and the shape of dieFromSignal is the reason.
//     Undoing our own `Notify` restores the disposition the process HAD, which
//     is not always "default": a shell gives a background child SIGINT set to
//     SIG_IGN, and the Go runtime faithfully restores that on both
//     `signal.Reset` and `signal.Stop` — measured, /proc `SigIgn` gains SIGINT in
//     both cases — so a re-raise is then DISCARDED and the process survives.
//     That is how this shipped as "notice, then an uninterruptible wait". So the
//     undo is `signal.Reset` (the correct undo of `Notify`, which leaves a
//     genuinely ignored signal ignored, as POSIX intends for a background job)
//     and the guarantee that the process ends comes from the explicit exit
//     after it — not from signal delivery.
//
//   - installed only for a BLOCKING wait. A `--no-wait` prompt or a predicate
//     wait that is about to return has nothing to say about a turn.
//
// The returned function uninstalls the handler; callers defer it so a
// long-running process (the test binary) does not accumulate handlers.
func noticeInterruptedWait(w io.Writer, _ string, options ...any) func() {
	out := w
	quiet := false
	if len(options) > 0 {
		if v, ok := options[0].(io.Writer); ok {
			out = v
		}
	}
	if len(options) > 1 {
		quiet, _ = options[1].(bool)
	}
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			if !quiet {
				_, _ = out.Write(adapter.RenderExchangeReport(adapter.ExchangeReport{Outcome: adapter.ExchangeWaitSignal}))
			}
			go dieFromSignal(sig)
			select {
			case second := <-ch:
				exitImmediately(exitSignaled(second))
			case <-done:
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// dieFromSignal is the terminal step: undo our handler, re-raise so the process
// dies the way the shell expects, and then — unconditionally — exit with the
// status that signal would have produced.
//
// The exit is not belt-and-braces; it is the guarantee. A re-raise is discarded
// whenever the process's pre-gmux disposition for the signal was SIG_IGN (a
// background job), so "the process ends" cannot rest on delivery. The short
// sleep gives the ordinary case — where the re-raise DOES kill us — the chance
// to produce a real signal death, which is what a shell reports as 128+N and
// what a supervisor inspecting `WaitStatus.Signaled()` expects to see.
//
// A variable so a test can observe the decision without killing the test binary.
var exitImmediately = os.Exit

var dieFromSignal = func(sig os.Signal) {
	if s, ok := sig.(syscall.Signal); ok {
		signal.Reset(s) // the correct undo of our Notify; see the note above
	}
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(sig)
	}
	time.Sleep(200 * time.Millisecond)
	os.Exit(exitSignaled(sig))
}

// exitSignaled is the shell's convention for "died from signal N", used only by
// the backstop above — the ordinary path is the real signal, which produces this
// same status through the kernel.
func exitSignaled(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128 + int(syscall.SIGINT)
}
