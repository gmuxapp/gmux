package adapter

import (
	"context"
	"time"
)

// SessionFileInfo holds metadata extracted from a tool's session file.
type SessionFileInfo struct {
	ID           string
	Title        string
	Slug         string // Human-readable, URL-safe session identity. Set by the adapter.
	Cwd          string
	Created      time.Time
	MessageCount int
	FilePath     string
}

// Event is a partial session state update emitted by an adapter from observed
// PTY bytes (via Monitor). Zero/nil fields are no-ops; the system only applies
// fields that are explicitly set.
type Event struct {
	Title  string  // non-empty: update the adapter title
	Status *Status // non-nil: update status; &Status{} clears it
	Unread *bool   // non-nil: set or clear the unread flag
	Cwd    string  // non-empty: update the session's canonical directory
}

// BoolPtr returns a pointer to v. Convenience for setting Event.Unread.
func BoolPtr(v bool) *bool { return &v }

// Launchable is implemented by adapters that want to expose one or more
// launch presets in the UI.
type Launchable interface {
	// Launchers returns the launch presets this adapter contributes.
	// Adapters may return zero, one, or many presets.
	Launchers() []Launcher
}

// SessionFiler is implemented by adapters whose tools write session
// files to disk (pi, claude-code, etc). Used by gmuxd for resumable
// session discovery and session file attribution.
type SessionFiler interface {
	// SessionRootDir returns the parent directory containing all per-cwd
	// session subdirectories (e.g. ~/.pi/agent/sessions/).
	// Used by the scanner to enumerate all known working directories.
	SessionRootDir() string

	// SessionDir returns the directory where this tool stores session
	// files for the given cwd. Returns "" if not applicable.
	SessionDir(cwd string) string

	// ParseSessionFile reads a session file and returns display metadata.
	// Called by gmuxd to index conversations and resolve resume commands.
	ParseSessionFile(path string) (*SessionFileInfo, error)
}

// ConversationSink receives conversation-file changes from a ConversationSource.
// Paths are absolute session-file paths the daemon resolves via the adapter's
// ParseSessionFile.
type ConversationSink interface {
	// Upsert reports a conversation file that exists, was created, or changed.
	Upsert(path string)
	// Remove reports a conversation file that no longer exists.
	Remove(path string)
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
	ResumeCommand(info *SessionFileInfo) []string

	// CanResume returns whether a session file represents a resumable
	// session (vs a corrupted/empty/incompatible one).
	CanResume(path string) bool
}
