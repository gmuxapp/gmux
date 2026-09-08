// Package relaunch holds the single relaunch policy shared by presentation
// and execution. It is deliberately a dependency-free leaf: the snapshot wire
// converter (which decides whether the UI is offered the affordance) and the
// runner spawner (which decides whether to spawn) both import it, so neither
// can drift into advertising a relaunch the other refuses.
package relaunch

// Kind names the one relaunch action a dead session offers.
//
// The two kinds are genuinely different verbs, not labels: "resume" picks a
// recorded agent conversation back up (the adapter derives a `--resume <id>`
// style command from it), while "rerun" simply launches the session's recorded
// command again in the same directory, with no state carried over. A shell has
// no conversation to resume, so a dead shell can only ever be rerun.
type Kind string

const (
	// None means the daemon will refuse to relaunch the session.
	None Kind = ""
	// Resume means the adapter resolved a resume command from the
	// session's recorded conversation.
	Resume Kind = "resume"
	// Rerun means there is no conversation to resume, but the recorded
	// launch command can be run again.
	Rerun Kind = "rerun"
)

// Resolve is the single relaunch policy: given a session's adapter,
// conversation ref and recorded launch command, it returns the command a
// relaunch would execute and which verb that is.
//
// Presentation (the snapshot wire converter, which decides whether the UI
// offers the affordance) and execution (the runner spawner, which decides
// whether to spawn) must both go through this function. When they derived
// their verdicts independently the UI advertised relaunches the daemon then
// refused: a dead shell session has a recorded command but never a
// conversation ref, so presentation said "resumable" while the spawner —
// which only ever resolved the *resume* command — answered "session is not
// resumable" (and, via restart, killed the session on the way).
//
// resume resolves the adapter's resume command for a conversation ref. A nil
// func means "this caller has no resume policy at all" (a degenerate,
// test-only shape — production always injects one) and degrades to the rerun
// path rather than pretending the adapter refused. Rules:
//
//   - A conversation ref that resolves to a resume command → Resume.
//   - A conversation ref that resolves to nothing → None. The adapter is
//     authoritative for a bound conversation: an empty or unreadable
//     transcript is not silently downgraded to a fresh rerun, because the
//     user asked to continue *that* conversation.
//   - No conversation ref (shell, editor, one-off commands, and agents that
//     died before their hook bound a conversation) → Rerun of the recorded
//     command, when there is one.
func Resolve(resume func(adapterName, conversationRef string) []string, adapterName, conversationRef string, command []string) ([]string, Kind) {
	if conversationRef != "" && resume != nil {
		if cmd := resume(adapterName, conversationRef); len(cmd) > 0 {
			return cmd, Resume
		}
		return nil, None
	}
	if len(command) > 0 {
		return append([]string(nil), command...), Rerun
	}
	return nil, None
}
