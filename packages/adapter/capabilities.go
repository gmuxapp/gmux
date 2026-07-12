package adapter

import (
	"context"
	"io"
	"time"
)

// Conversation refs.
//
// A conversation ref is an opaque, adapter-scoped locator for one stored
// conversation. Where a conversation lives is an adapter implementation
// detail (ADR 0014): today's file-backed adapters (pi, claude, codex) use
// the absolute conversation-file path as their ref, but a future adapter
// may store conversations in a database and use a row key or UUID. The
// daemon MUST treat refs as opaque strings — store them, compare them for
// equality, and hand them back to the owning adapter — never parse them,
// stat them, or assume they name a file.

// ConversationInfo holds adapter-parsed metadata for one stored
// conversation, returned by ConversationDescriber.DescribeConversation.
type ConversationInfo struct {
	ID      string // adapter-native conversation ID (typically a UUID)
	Title   string
	Slug    string // Human-readable, URL-safe session identity. Set by the adapter.
	Cwd     string
	Created time.Time
	// LastActivity is the adapter-reported time of the most recent
	// activity in this conversation. File-backed adapters derive it from
	// the conversation file's mtime; that is their implementation detail —
	// consumers must not stat anything themselves. Zero when unknown.
	LastActivity time.Time
	MessageCount int
	Ref          string // the opaque conversation ref this info was described from
	// AncestorIDs are IDs of conversations this one was resumed from, oldest-first
	// when that order is derivable; empty when the adapter resumes in place (ADR 0023 §2).
	AncestorIDs []string
}

// Launchable is implemented by adapters that want to expose one or more
// launch presets in the UI.
type Launchable interface {
	// Launchers returns the launch presets this adapter contributes.
	// Adapters may return zero, one, or many presets.
	Launchers() []Launcher
}

// ConversationDescriber is implemented by adapters whose tool persists
// conversations (pi, claude-code, codex, ...). It is the daemon's only way
// to turn an opaque conversation ref into display metadata; how the
// adapter reads its storage (JSONL file, database row, ...) is private to
// the adapter.
type ConversationDescriber interface {
	// DescribeConversation resolves an opaque conversation ref to display
	// metadata. Called by gmuxd to index conversations and resolve resume
	// commands.
	DescribeConversation(ref string) (*ConversationInfo, error)
}

// ConversationOpener is implemented by adapters that can stream a stored
// conversation's raw, adapter-native content (e.g. the JSONL transcript).
// This is the content seam for derived consumers such as the fulltext
// search index: enumerate refs via ConversationSource, then open each ref
// for indexing. The byte format is adapter-specific; consumers that need a
// normalized schema translate per ADR 0021.
type ConversationOpener interface {
	// OpenConversation returns a reader over the conversation's raw
	// content. The caller must Close it.
	OpenConversation(ref string) (io.ReadCloser, error)
}

// ConversationSink receives conversation-ref changes from a ConversationSource.
// Refs are opaque adapter-scoped locators the daemon resolves via the
// owning adapter's DescribeConversation.
type ConversationSink interface {
	// Upsert reports a conversation that exists, was created, or changed.
	Upsert(ref string)
	// Remove reports a conversation that no longer exists.
	Remove(ref string)
}

// ConversationSource is implemented by adapters that expose a set of
// conversations and can report them to the daemon's index. The adapter owns
// the observation mechanism (file watching, polling, a DB subscription); the
// daemon only consumes events. This is the single seam the daemon uses to keep
// the conversations index (URL resolution, search) current — there is no
// daemon-side file monitor.
type ConversationSource interface {
	// SnapshotConversations emits every currently-existing conversation to sink.
	// Synchronous; used once at startup to populate the index.
	SnapshotConversations(sink ConversationSink)
	// WatchConversations emits incremental conversation changes to sink until
	// ctx is cancelled. Returns ctx.Err() on cancellation.
	WatchConversations(ctx context.Context, sink ConversationSink) error
}

// ConversationProber is implemented by conversation-storing adapters that
// can tell a *deleted* conversation apart from one whose storage is
// merely *unavailable* (unmounted home, a tool dir that doesn't exist
// yet, an unreachable database). The startup retention reconcile
// (services/gmuxd) uses it to retire dead sessions whose backing
// conversation is genuinely gone without nuking entries when the tool's
// storage simply isn't reachable.
//
// The distinction is adapter-owned because only the adapter knows which
// directory anchors "storage is present" for its layout — e.g. the
// per-cwd project dir under ~/.claude/projects vs codex's date-nested
// ~/.codex/sessions/YYYY/MM/DD. The shared ConversationGoneAtRoot helper
// implements the common root-anchored rule; adapters with quirkier
// layouts may answer differently.
type ConversationProber interface {
	// ConversationGone reports whether the conversation identified by the
	// opaque ref was deleted. ok=false means the answer is undeterminable
	// (storage unavailable), in which case the caller MUST NOT treat the
	// conversation as gone. When the conversation is present, returns
	// (false, true).
	ConversationGone(ref string) (gone bool, ok bool)
}

// SessionExtender marks adapters whose agent exposes a native extension/hook
// API that can report the active session authoritatively — the strongest
// signal, catching even a cache-served /resume-select that leaves no fs
// trace. The runner materializes the gmux hook and asks the adapter to splice
// it into the launch argv via ExtendCommand; the hook posts an authoritative
// "session" event to the runner socket on every bind (pi's session_start).
//
// Only adapters whose argv gmux fully controls should opt in: argv injection
// (unlike env injection) does not survive a shell-wrapped launch.
type SessionExtender interface {
	// ExtendCommand returns args with the flags that load the gmux session
	// extension at extPath spliced in immediately after the agent binary —
	// which is NOT necessarily args[0] (e.g. `npx pi`, `env pi`). The adapter
	// owns locating its own binary. Return args unchanged to inject nothing.
	ExtendCommand(args []string, extPath string) []string
}

// SessionHookCommand marks adapters whose agent runs the gmux binary itself as
// a command hook (`gmux __<agent>-hook <event>`), injected per-launch on the
// argv via the agent's config-override flags. Contrast SessionExtender, whose
// hook is a separate materialized extension file. codex is the first
// implementer; see ADR 0013 and docs/runner-hook-protocol.md.
type SessionHookCommand interface {
	// HookCommand returns args with the flags spliced in that (a) define a gmux
	// hook which relays session events to the runner socket and (b) let it run.
	// selfBin is the absolute path of the gmux binary the hook invokes. The
	// binary token may not be args[0] (e.g. `env codex`). Returns ok=false (and
	// args unchanged) when the agent version doesn't support hooks, so the
	// runner launches unmodified and the daemon's fallback attribution applies.
	HookCommand(args []string, selfBin string) (out []string, ok bool)
}

// HookDriven reports whether sessions of this adapter derive their
// turn state (Status.Working, unread, error) from an agent-side hook
// (SessionExtender or SessionHookCommand — ADR 0011/0015): the agent
// itself reports turn start/end over the runner socket, and the
// runner applies no turn inference of its own.
//
// Every other adapter gets the runner's default turn model instead:
// the session is active (Working=true) from launch, OSC 133 prompt
// marks in the PTY output — when the child's shell integration emits
// them — upgrade it to per-command prompt-cycle turns, and for
// sessions that never emit marks the process exit closes the one
// lifetime-long turn.
func HookDriven(a Adapter) bool {
	if a == nil {
		return false
	}
	if _, ok := a.(SessionExtender); ok {
		return true
	}
	if _, ok := a.(SessionHookCommand); ok {
		return true
	}
	return false
}

// PassthroughDetector marks adapters that recognize invocations which are NOT
// interactive sessions — one-shot subcommands like `pi update` or `pi list`.
// gmux execs these directly (inheriting the tty, returning their exit code)
// instead of wrapping them in a runner/PTY and registering a session. Wrapping
// would be both pointless (a one-shot command is not a session to monitor) and
// broken (pi requires its subcommand at argv[1], but gmux prepends -e for the
// session extension, demoting the subcommand to a chat prompt).
type PassthroughDetector interface {
	// IsPassthrough reports whether args (args[0] = binary) is a one-shot,
	// non-session invocation that gmux should exec directly rather than manage.
	IsPassthrough(args []string) bool
}

// SubmitMode selects which submit keystroke ends a prompt typed into an
// agent's composer. `gmux send --steering / --follow-up` resolves the
// mode to concrete PTY bytes via SubmitSeqFor, so callers never need to
// know adapter-specific key encodings (issue #385).
type SubmitMode int

const (
	// SubmitSteering delivers the prompt into the current turn
	// immediately (pi: Enter). On an idle agent it simply submits.
	SubmitSteering SubmitMode = iota
	// SubmitFollowUp queues the prompt until the current turn ends
	// (pi: Alt+Enter). On an idle agent it degrades to a plain submit.
	SubmitFollowUp
)

// PromptSubmitter is optionally implemented by adapters whose agent
// distinguishes submit keystrokes by mode. Adapters that don't
// implement it get the universal default (see SubmitSeqFor): Enter for
// both modes — correct for shells and for agents like claude/codex,
// where Enter both submits when idle and queues when busy. An
// implementer may reject a mode it can't honor by returning ok=false;
// the CLI surfaces that as a usage error instead of sending bytes with
// the wrong meaning.
type PromptSubmitter interface {
	// SubmitSeq returns the PTY byte sequence that submits a composed
	// prompt in the given mode — the encoding of the corresponding
	// keystroke that the adapter's tool parses most robustly (which
	// may differ from what an xterm-class terminal emits; see the pi
	// adapter's Kitty CSI-u choice). Sequences without a carriage
	// return must be kept in sync with gmuxd's inputSubmits guard so
	// `send --wait` keeps accepting them as submitting input.
	SubmitSeq(mode SubmitMode) (seq string, ok bool)
}

// SubmitSeqFor resolves the submit byte sequence for a session's
// adapter. Nil adapters (names FindByAdapter doesn't know, e.g. a peer
// session created by a newer gmux) and adapters without the
// PromptSubmitter capability fall back to "\r" — Enter, the single
// submit keystroke of every PTY application — for both modes.
func SubmitSeqFor(a Adapter, mode SubmitMode) (string, bool) {
	if ps, ok := a.(PromptSubmitter); ok {
		return ps.SubmitSeq(mode)
	}
	return "\r", true
}

// CommandTitler is optionally implemented by adapters that want to
// control how a command array is displayed as a fallback title.
// Without it, the store joins the full command (e.g. "pytest -x").
// Adapters that use resume commands should implement this to avoid
// titles like "codex resume 019cfb54-...".
type CommandTitler interface {
	// CommandTitle returns a display title for the given command.
	CommandTitle(command []string) string
}

// RegistrationInfo holds initial session metadata returned by SessionRegistrar.
type RegistrationInfo struct {
	// Slug is the human-readable session identifier to assign at registration.
	// Empty means the daemon should derive one itself.
	Slug string
}

// SessionRegistrar is optionally implemented by adapters that need to perform
// work when gmuxd registers a new session (e.g. writing a state file for
// restart recovery). Returns initial metadata like the session slug.
// A non-nil error is logged by gmuxd but does not abort registration.
type SessionRegistrar interface {
	OnRegister(id, cwd string, command []string) (RegistrationInfo, error)
}

// SessionFinalizer is optionally implemented by adapters that need cleanup
// when a session is dismissed from gmuxd (e.g. removing a state file).
type SessionFinalizer interface {
	OnDismiss(id, cwd string)
}

// Resumer is implemented by adapters whose sessions can be resumed
// after the process exits.
type Resumer interface {
	// ResumeCommand returns the command to resume the given session.
	ResumeCommand(info *ConversationInfo) []string

	// CanResume returns whether the conversation identified by the opaque
	// ref represents a resumable session (vs a corrupted/empty/incompatible
	// one).
	CanResume(ref string) bool
}
